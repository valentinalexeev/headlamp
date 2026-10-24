package core

import (
	"context"
	"net"
	"sync"
	"time"
)

// Tailnet is the network seam: listeners and dialing go through it so the
// real implementation (tsnet) can be replaced by plain TCP in tests.
type Tailnet interface {
	ListenAdmin() (net.Listener, error)
	ListenPublic() (net.Listener, error)
	Dial(ctx context.Context, network, addr string) (net.Conn, error)
	WhoIs(ctx context.Context, remoteAddr string) (login string, tags []string, err error)
}

// Local is a plain-TCP Tailnet for tests: both listeners bind host:0.
type Local struct {
	host        string
	mu          sync.Mutex
	admin       net.Listener
	public      net.Listener
	adminReady  chan struct{}
	publicReady chan struct{}
}

// NewLocal returns a Local tailnet bound to host (for example "127.0.0.1").
func NewLocal(host string) *Local {
	return &Local{host: host, adminReady: make(chan struct{}), publicReady: make(chan struct{})}
}

func (l *Local) listen(slot *net.Listener, ready chan struct{}) (net.Listener, error) {
	ln, err := net.Listen("tcp", net.JoinHostPort(l.host, "0"))
	if err != nil {
		return nil, err
	}
	l.mu.Lock()
	*slot = ln
	l.mu.Unlock()
	close(ready)
	return ln, nil
}

func (l *Local) ListenAdmin() (net.Listener, error)  { return l.listen(&l.admin, l.adminReady) }
func (l *Local) ListenPublic() (net.Listener, error) { return l.listen(&l.public, l.publicReady) }

func (l *Local) wait(ready chan struct{}, slot *net.Listener) string {
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		return ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return (*slot).Addr().String()
}

// AdminAddr waits for the admin listener and returns its address ("" on timeout).
func (l *Local) AdminAddr() string { return l.wait(l.adminReady, &l.admin) }

// PublicAddr waits for the public listener and returns its address ("" on timeout).
func (l *Local) PublicAddr() string { return l.wait(l.publicReady, &l.public) }

// AdminListened reports whether ListenAdmin was ever called.
func (l *Local) AdminListened() bool {
	select {
	case <-l.adminReady:
		return true
	default:
		return false
	}
}

// PublicListened reports whether ListenPublic was ever called.
func (l *Local) PublicListened() bool {
	select {
	case <-l.publicReady:
		return true
	default:
		return false
	}
}

func (l *Local) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, network, addr)
}

// WhoIs is a test-only stub: it always reports login "local" and no tags.
func (l *Local) WhoIs(ctx context.Context, remoteAddr string) (string, []string, error) {
	return "local", nil, nil
}
