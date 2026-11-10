package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// TsnetConfig configures the embedded tailscale node.
type TsnetConfig struct {
	ControlURL string
	Hostname   string
	AuthKey    string
	Dir        string
	Verbose    bool // TSNET_VERBOSE=1: also forward the backend's own logs
	// Store, when set, holds the node state instead of a file under Dir (Dir stays set: tsnet still
	// keeps other files there). Nil means the default file store.
	Store ipn.StateStore
}

// TsnetConfigFromEnv reads HEADSCALE_LOGIN_SERVER, TAILNET_HOSTNAME
// (default node-control) and TAILSCALE_AUTH_KEY; state lives in
// /var/lib/tailscale. TSNET_VERBOSE=1 turns on the backend's own (very
// chatty) logs; without it only user-facing messages such as the login URL
// are logged.
func TsnetConfigFromEnv(env Env) TsnetConfig {
	return TsnetConfig{
		ControlURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
		Hostname:   env.Default("TAILNET_HOSTNAME", "node-control"),
		AuthKey:    env.Getenv("TAILSCALE_AUTH_KEY"),
		Dir:        "/var/lib/tailscale",
		Verbose:    env.Getenv("TSNET_VERBOSE") == "1",
	}
}

// Tsnet is the production Tailnet backed by tailscale.com/tsnet.
type Tsnet struct {
	srv     *tsnet.Server
	authKey string
}

// NewTsnet builds (but does not start) the embedded node; it joins the
// tailnet on first Listen or Dial.
func NewTsnet(cfg TsnetConfig) *Tsnet {
	srv := &tsnet.Server{
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Dir:        cfg.Dir,
		Store:      cfg.Store,
		UserLogf:   func(format string, args ...any) { slog.Info(fmt.Sprintf("tailnet: "+format, args...)) },
	}
	if cfg.Verbose {
		srv.Logf = func(format string, args ...any) { slog.Info(fmt.Sprintf("tsnet: "+format, args...)) }
	}
	return &Tsnet{srv: srv, authKey: cfg.AuthKey}
}

// Up joins the tailnet and waits until the node is running, so a bad auth key
// or an unreachable control server fails at start instead of at first use.
func (t *Tsnet) Up(ctx context.Context) error {
	st, err := t.srv.Up(ctx)
	if err != nil {
		return upError(err, t.authKey)
	}
	slog.Info("tailnet up", "addresses", len(st.TailscaleIPs))
	return nil
}

// upError wraps err for logging with the auth key removed.
func upError(err error, authKey string) error {
	msg := err.Error()
	if authKey != "" {
		msg = strings.ReplaceAll(msg, authKey, "***")
	}
	return errors.New(msg)
}

func (t *Tsnet) ListenAdmin() (net.Listener, error)  { return t.srv.Listen("tcp", ":80") }
func (t *Tsnet) ListenPublic() (net.Listener, error) { return t.srv.Listen("tcp", ":8081") }

// ListenDNS listens on port 53 of the node's IPv4 tailnet address, UDP and TCP. tsnet needs a concrete address for
// UDP (it refuses ":53"), so the node must be up first; the spike on #117 showed both paths work through tsnet.
func (t *Tsnet) ListenDNS() (net.PacketConn, net.Listener, error) {
	ip4, _ := t.srv.TailscaleIPs()
	if !ip4.IsValid() {
		return nil, nil, errors.New("tailnet has no IPv4 address yet")
	}
	pc, err := t.srv.ListenPacket("udp", netip.AddrPortFrom(ip4, 53).String())
	if err != nil {
		return nil, nil, err
	}
	ln, err := t.srv.Listen("tcp", ":53")
	if err != nil {
		pc.Close()
		return nil, nil, err
	}
	return pc, ln, nil
}

func (t *Tsnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.srv.Dial(ctx, network, addr)
}

func (t *Tsnet) WhoIs(ctx context.Context, remoteAddr string) (string, []string, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return "", nil, err
	}
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", nil, err
	}
	var login string
	if who.UserProfile != nil {
		login = who.UserProfile.LoginName
	}
	var tags []string
	if who.Node != nil {
		tags = who.Node.Tags
	}
	return login, tags, nil
}

// Close shuts the embedded node down.
func (t *Tsnet) Close() error { return t.srv.Close() }

// PeerVersion is the tailscale version a tailnet peer reports (Hostinfo.IPNVersion, e.g. "1.104.5-t1234abcd"), read
// from this node's own netmap through WhoIs on the peer's address (#195). It sends nothing to the peer.
func (t *Tsnet) PeerVersion(ctx context.Context, ip string) (string, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return "", err
	}
	who, err := lc.WhoIs(ctx, ip)
	if err != nil {
		return "", err
	}
	if who.Node == nil || !who.Node.Hostinfo.Valid() {
		return "", nil
	}
	return who.Node.Hostinfo.IPNVersion(), nil
}
