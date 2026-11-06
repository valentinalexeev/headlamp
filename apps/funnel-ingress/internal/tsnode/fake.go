package tsnode

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"strings"
	"sync"
)

// ErrUnknownCaller is returned by Fake.WhoIs for an address nobody registered.
var ErrUnknownCaller = errors.New("unknown caller")

// Fake is an in-memory Node for tests.
type Fake struct {
	mu     sync.Mutex
	ids    map[netip.Addr]Identity
	peers  []Peer
	loaded bool
	self   Identity
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
}

// NewFake returns a Fake with a loaded, empty peer list.
func NewFake() *Fake {
	return &Fake{ids: map[netip.Addr]Identity{}, loaded: true, self: Identity{NodeID: "self", Name: "funnel-ingress", Addr: netip.MustParseAddr("100.64.0.1")}}
}

// AddIdentity makes WhoIs answer id for connections from remoteIP.
func (f *Fake) AddIdentity(remoteIP string, id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := netip.MustParseAddr(remoteIP)
	if !id.Addr.IsValid() {
		id.Addr = a
	}
	f.ids[a] = id
}

// RemoveIdentity makes WhoIs fail for remoteIP again.
func (f *Fake) RemoveIdentity(remoteIP string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.ids, netip.MustParseAddr(remoteIP))
}

// SetPeers replaces the peer list.
func (f *Fake) SetPeers(ps []Peer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.peers = append([]Peer(nil), ps...)
}

// SetLoaded sets Snapshot().Loaded.
func (f *Fake) SetLoaded(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loaded = v
}

// SetSelf sets the proxy node's own identity.
func (f *Fake) SetSelf(id Identity) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.self = id
}

// SetDial sets the function behind DialContext.
func (f *Fake) SetDial(d func(ctx context.Context, network, addr string) (net.Conn, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dial = d
}

func (f *Fake) WhoIs(_ context.Context, remoteAddr string) (Identity, error) {
	host := remoteAddr
	if ap, err := netip.ParseAddrPort(remoteAddr); err == nil {
		host = ap.Addr().String()
	} else if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	a, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil {
		return Identity{}, ErrUnknownCaller
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.ids[a.Unmap()]
	if !ok {
		return Identity{}, ErrUnknownCaller
	}
	return id, nil
}

func (f *Fake) Snapshot(context.Context) (Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Snapshot{Loaded: f.loaded, Peers: append([]Peer(nil), f.peers...)}, nil
}

func (f *Fake) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	d := f.dial
	f.mu.Unlock()
	if d == nil {
		return nil, errors.New("fake: no dialer")
	}
	return d(ctx, network, addr)
}

func (f *Fake) Self() Identity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.self
}
