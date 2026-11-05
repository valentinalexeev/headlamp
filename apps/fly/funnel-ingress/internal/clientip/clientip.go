// Package clientip finds the address of the client behind a reverse proxy or load balancer. A forwarding header is read
// only when the direct peer is a trusted proxy, so a client that connects directly cannot claim another address.
package clientip

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// DefaultHeader is the header the default hosting platform's edge sets. It keeps the behaviour of earlier releases.
const DefaultHeader = "Fly-Client-IP"

// Resolver decides the client address of a request.
type Resolver struct {
	header  string         // canonical header name; "" means use the peer address only
	list    bool           // the header is a comma-separated chain (X-Forwarded-For)
	trusted []netip.Prefix // peers allowed to set the header; empty means every peer
}

// New builds a Resolver. header is a header name, or "none" to ignore all headers. trusted is a comma-separated list
// of CIDRs or addresses; when empty, the header is read from every peer, which is safe only when nothing but the
// proxy can reach the listener. Error texts name the problem, not the input.
func New(header, trusted string) (*Resolver, error) {
	r := &Resolver{}
	switch h := strings.TrimSpace(header); {
	case h == "":
		r.header = DefaultHeader
	case strings.EqualFold(h, "none"):
	case !validHeaderName(h):
		return nil, errors.New("not a valid header name")
	default:
		r.header = http.CanonicalHeaderKey(h)
	}
	r.list = r.header == "X-Forwarded-For"
	for _, f := range strings.Split(trusted, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		p, err := parsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("entry %d is not a CIDR or an address", len(r.trusted)+1)
		}
		r.trusted = append(r.trusted, p)
	}
	return r, nil
}

// Header returns the header the resolver reads, or "" when it reads none.
func (r *Resolver) Header() string { return r.header }

// TrustedPeer reports whether the direct peer of req may set forwarding headers.
func (r *Resolver) TrustedPeer(req *http.Request) bool {
	if len(r.trusted) == 0 {
		return true
	}
	peer, ok := peerAddr(req)
	return ok && r.isTrusted(peer)
}

// ClientIP returns the client address of req: from the header when the peer is trusted and the header holds a valid
// address, otherwise the direct peer.
func (r *Resolver) ClientIP(req *http.Request) string {
	peer, peerOK := peerAddr(req)
	if r.header != "" && r.TrustedPeer(req) {
		if ip, ok := r.fromHeader(req); ok {
			return ip.String()
		}
	}
	if peerOK {
		return peer.String()
	}
	return req.RemoteAddr
}

// fromHeader reads the header. For a chain, every proxy appends the address it saw, so the client is the right-most
// entry that is not a trusted proxy. When every entry is trusted, the left-most one is the best answer there is.
func (r *Resolver) fromHeader(req *http.Request) (netip.Addr, bool) {
	values := req.Header.Values(r.header)
	if !r.list {
		if len(values) != 1 {
			return netip.Addr{}, false // missing, or set twice: do not guess which one the proxy wrote
		}
		ip, err := netip.ParseAddr(strings.TrimSpace(values[0]))
		return norm(ip), err == nil
	}
	var chain []netip.Addr
	for _, v := range values {
		for _, part := range strings.Split(v, ",") {
			ip, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return netip.Addr{}, false // a broken chain cannot be trusted at all
			}
			chain = append(chain, norm(ip))
		}
	}
	if len(chain) == 0 {
		return netip.Addr{}, false
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if !r.isTrusted(chain[i]) {
			return chain[i], true
		}
	}
	return chain[0], true
}

func (r *Resolver) isTrusted(ip netip.Addr) bool {
	for _, p := range r.trusted {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

func peerAddr(req *http.Request) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		host = req.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}
	return norm(ip), true
}

// norm drops the IPv4-mapped form and any IPv6 zone, so an address compares equal to the CIDRs it belongs to.
func norm(ip netip.Addr) netip.Addr { return ip.Unmap().WithZone("") }

func parsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "/") {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, err
		}
		// Peers are compared unmapped, so a mapped CIDR (::ffff:10.0.0.0/104) becomes its IPv4 form.
		if a := p.Addr(); a.Is4In6() {
			if p.Bits() < 96 {
				return netip.Prefix{}, errors.New("mapped prefix shorter than /96")
			}
			p = netip.PrefixFrom(a.Unmap(), p.Bits()-96)
		}
		return p.Masked(), nil
	}
	ip, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Prefix{}, err
	}
	ip = norm(ip)
	return netip.PrefixFrom(ip, ip.BitLen()), nil
}

func validHeaderName(s string) bool {
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return s != ""
}
