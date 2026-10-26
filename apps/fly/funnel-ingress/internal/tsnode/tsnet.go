package tsnode

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"
	"sync"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
	"tailscale.com/types/views"

	"funnel-ingress/internal/config"
)

// TSNet is the production Node: an embedded tailscale node (tsnet) joined to the control server.
type TSNet struct {
	srv     *tsnet.Server
	authKey string

	mu     sync.Mutex
	self   Identity
	loaded bool // a status with a running backend and a self node has been seen
}

// NewTSNet builds (but does not start) the embedded node. store holds its state (nil: tsnet's default file
// under cfg.StateDir). It joins the tailnet in Up.
func NewTSNet(cfg config.Config, store ipn.StateStore) *TSNet {
	srv := &tsnet.Server{
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Dir:        cfg.StateDir,
		Store:      store,
		UserLogf:   func(format string, args ...any) { slog.Info(fmt.Sprintf("tailnet: "+format, args...)) },
	}
	return &TSNet{srv: srv, authKey: cfg.AuthKey}
}

// Up joins the tailnet and waits until the node is running, so a bad auth key or an unreachable control
// server fails at start. The error never contains the auth key.
func (t *TSNet) Up(ctx context.Context) error {
	st, err := t.srv.Up(ctx)
	if err != nil {
		msg := err.Error()
		if t.authKey != "" {
			msg = strings.ReplaceAll(msg, t.authKey, "***")
		}
		return errors.New(msg)
	}
	t.mu.Lock()
	if st.Self != nil {
		t.self = Identity{NodeID: string(st.Self.ID), Name: st.Self.HostName, Tags: tagsOf(st.Self.Tags)}
	}
	if len(st.TailscaleIPs) > 0 {
		t.self.Addr = st.TailscaleIPs[0]
	}
	t.mu.Unlock()
	slog.Info("tailnet up", "addresses", len(st.TailscaleIPs))
	return nil
}

// ListenAPI listens on the node's tailnet address only; the registration API is unreachable from the internet
// by construction.
func (t *TSNet) ListenAPI(port int) (net.Listener, error) {
	return t.srv.Listen("tcp", fmt.Sprintf(":%d", port))
}

// Close shuts the embedded node down.
func (t *TSNet) Close() error { return t.srv.Close() }

func (t *TSNet) Self() Identity {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.self
}

func (t *TSNet) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.srv.Dial(ctx, network, addr)
}

func (t *TSNet) WhoIs(ctx context.Context, remoteAddr string) (Identity, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return Identity{}, err
	}
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	if who.Node == nil || who.Node.StableID == "" {
		return Identity{}, errors.New("whois: no node")
	}
	addr, err := parseRemote(remoteAddr)
	if err != nil {
		return Identity{}, err
	}
	// Prefer the node's IPv4 address: a stable target no matter which family the call came in on.
	for _, p := range who.Node.Addresses {
		if p.Addr().Is4() {
			addr = p.Addr()
			break
		}
	}
	return Identity{NodeID: string(who.Node.StableID), Name: who.Node.ComputedName, Addr: addr, Tags: append([]string(nil), who.Node.Tags...)}, nil
}

func (t *TSNet) Snapshot(ctx context.Context) (Snapshot, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return Snapshot{}, err
	}
	st, err := lc.Status(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	var snap Snapshot
	for _, p := range st.Peer {
		peer := Peer{NodeID: string(p.ID), Name: p.HostName, Tags: tagsOf(p.Tags), Online: p.Online}
		if len(p.TailscaleIPs) > 0 {
			peer.Addr = p.TailscaleIPs[0]
		}
		snap.Peers = append(snap.Peers, peer)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if st.BackendState == "Running" && st.Self != nil {
		t.loaded = true
	}
	snap.Loaded = t.loaded
	return snap, nil
}

func tagsOf(v *views.Slice[string]) []string {
	if v == nil {
		return nil
	}
	return v.AsSlice()
}

// parseRemote extracts the address from "ip:port", "[ip]:port" or a bare IP, unmapping IPv4-mapped IPv6.
func parseRemote(addr string) (netip.Addr, error) {
	if ap, err := netip.ParseAddrPort(addr); err == nil {
		return ap.Addr().Unmap(), nil
	}
	a, err := netip.ParseAddr(strings.Trim(addr, "[]"))
	if err != nil {
		return netip.Addr{}, errors.New("bad remote address")
	}
	return a.Unmap(), nil
}
