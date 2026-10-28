// Package tsnode is the narrow view of the embedded tailscale node that the rest of funnel-ingress uses:
// who is calling, which peers exist, and a dialer into the tailnet. The tsnet implementation lives in
// tsnet.go; tests use Fake.
package tsnode

import (
	"context"
	"net"
	"net/netip"
)

// Identity is a node as the control plane knows it. NodeID is the stable ID: it survives renames and is
// never reused, which is what routes are bound to.
type Identity struct {
	NodeID string
	Name   string
	Addr   netip.Addr
	Tags   []string
}

// Peer is an entry of the node's peer list.
type Peer struct {
	NodeID string
	Name   string
	Addr   netip.Addr
	Tags   []string
	Online bool
}

// Snapshot is the peer list at a point in time. Loaded is false until the node has seen a complete list
// (running, with a self node); nothing may be cleaned up from an unloaded snapshot.
type Snapshot struct {
	Loaded bool
	Peers  []Peer
}

// Node is the embedded tailscale node.
type Node interface {
	// WhoIs resolves the tailnet address of a connecting client ("ip:port" or "ip") to its identity.
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
	Snapshot(ctx context.Context) (Snapshot, error)
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	// Self is the proxy node's own identity.
	Self() Identity
}
