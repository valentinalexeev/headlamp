package authz

import (
	"context"
	"errors"
	"net"
)

// Who is the WhoIs answer the fake gives for one remote IP.
type Who struct {
	Login string
	Tags  []string
	Err   error
}

// FakeTailnet is a core.Tailnet for tests whose WhoIs is keyed by the remote IP.
type FakeTailnet struct{ who map[string]Who }

// NewFakeTailnet returns a FakeTailnet answering from who (keyed by IP, no port).
func NewFakeTailnet(who map[string]Who) *FakeTailnet { return &FakeTailnet{who: who} }

var errUnsupported = errors.New("fake tailnet: not supported")

func (f *FakeTailnet) ListenAdmin() (net.Listener, error)  { return nil, errUnsupported }
func (f *FakeTailnet) ListenPublic() (net.Listener, error) { return nil, errUnsupported }

func (f *FakeTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return nil, errUnsupported
}

// WhoIs answers for the host part of remoteAddr; unknown or malformed addresses give an error.
func (f *FakeTailnet) WhoIs(ctx context.Context, remoteAddr string) (string, []string, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", nil, err
	}
	w, ok := f.who[host]
	if !ok {
		return "", nil, errors.New("fake tailnet: unknown address")
	}
	return w.Login, w.Tags, w.Err
}
