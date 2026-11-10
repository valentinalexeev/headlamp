package core

import (
	"context"
	"net"
)

// Caller is an identity that in-process code has already authenticated, for a request that it sends
// to the admin router itself. The MCP module uses it: it checks a tailnet login or an OIDC bearer
// token, then runs each tool as an admin request on behalf of that login. A remote request cannot
// set a context value, so only code in this process can make a Caller.
type Caller struct {
	Login string
	// Groups are the OIDC groups, if any. They count where a permission check sees the whole
	// subject.
	Groups []string
}

type callerKey struct{}

// WithCaller returns ctx that carries c. Use it only after c is authenticated.
func WithCaller(ctx context.Context, c Caller) context.Context {
	return context.WithValue(ctx, callerKey{}, c)
}

// CallerFrom returns the Caller set by WithCaller, if any.
func CallerFrom(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok && c.Login != ""
}

// CallerAware returns a Tailnet whose WhoIs reports the Caller of the request context, when there
// is one, as a human login with no tags. Without a Caller it asks tn. Every handler that names its
// caller by WhoIs (route gates, audit actors) then sees the same login for an in-process request.
// Run wraps the Tailnet it hands to modules with it.
func CallerAware(tn Tailnet) Tailnet {
	switch tn.(type) {
	case callerAwareTailnet, callerAwareDNSTailnet:
		return tn
	}
	base := callerAwareTailnet{tn}
	if dl, ok := tn.(DNSListener); ok {
		return callerAwareDNSTailnet{base, dl}
	}
	return base
}

type callerAwareTailnet struct{ Tailnet }

func (c callerAwareTailnet) WhoIs(
	ctx context.Context, remoteAddr string,
) (string, []string, error) {
	if who, ok := CallerFrom(ctx); ok {
		return who.Login, nil, nil
	}
	return c.Tailnet.WhoIs(ctx, remoteAddr)
}

// callerAwareDNSTailnet keeps the optional DNSListener of the wrapped Tailnet.
type callerAwareDNSTailnet struct {
	callerAwareTailnet
	dns DNSListener
}

func (c callerAwareDNSTailnet) ListenDNS() (net.PacketConn, net.Listener, error) {
	return c.dns.ListenDNS()
}
