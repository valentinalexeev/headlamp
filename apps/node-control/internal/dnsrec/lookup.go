package dnsrec

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"strings"
)

// Lookup checks the record with a DNS lookup and never writes it (#384): the edge (Caddy
// dynamic_dns) keeps the record, and node-control holds no DNS credentials. The wildcard is checked
// through a probe name under it, so the answer is the record's, not another name's.
type Lookup struct {
	// Resolve answers the addresses or the CNAME of a name; nil means the system resolver.
	Resolve func(ctx context.Context, name, typ string) ([]string, error)
}

// ErrNotWritten is returned by Put: this provider only checks.
var ErrNotWritten = errors.New(
	"the record is kept by the edge (FUNNEL_DNS_CHECK=lookup); node-control does not write it")

func (l *Lookup) Name() string { return "lookup" }

// probeLabel is the label put in place of the "*" of a wildcard to check it.
const probeLabel = "headlamp-probe"

func (l *Lookup) resolve(ctx context.Context, name, typ string) ([]string, error) {
	if l.Resolve != nil {
		return l.Resolve(ctx, name, typ)
	}
	if typ == "CNAME" {
		c, err := net.DefaultResolver.LookupCNAME(ctx, name)
		if err != nil {
			return nil, err
		}
		return []string{c}, nil
	}
	network := map[string]string{"A": "ip4", "AAAA": "ip6"}[typ]
	ips, err := net.DefaultResolver.LookupNetIP(ctx, network, name)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ips))
	for _, ip := range ips {
		out = append(out, ip.Unmap().String())
	}
	return out, nil
}

// Get returns the target when the record answers it: an address in the A/AAAA answer, or the
// CNAME. A name that does not resolve is "no record" (ok false), not an error. With several
// addresses, want (when set by Check) is returned if it is one of them.
func (l *Lookup) Get(ctx context.Context, name, typ string) (string, bool, error) {
	return l.get(ctx, name, typ, "")
}

// Check is Ensure for the lookup: it reports whether the record answers r.Target, and never
// writes.
func (l *Lookup) Check(ctx context.Context, r Record) (bool, error) {
	got, ok, err := l.get(ctx, r.Name, r.Type, r.Target)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, errors.New("no " + r.Type + " record answers " + r.Name)
	}
	if !strings.EqualFold(got, r.Target) {
		return false, errors.New(r.Name + " answers " + got + ", want " + r.Target)
	}
	return true, nil
}

func (l *Lookup) get(ctx context.Context, name, typ, want string) (string, bool, error) {
	q := name
	if rest, ok := strings.CutPrefix(name, "*."); ok {
		q = probeLabel + "." + rest
	}
	got, err := l.resolve(ctx, q, typ)
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if len(got) == 0 {
		return "", false, nil
	}
	if typ == "CNAME" {
		return strings.TrimSuffix(strings.ToLower(got[0]), "."), true, nil
	}
	if want != "" && slices.Contains(got, want) {
		return want, true, nil
	}
	slices.SortFunc(got, func(a, b string) int {
		pa, _ := netip.ParseAddr(a)
		pb, _ := netip.ParseAddr(b)
		return pa.Compare(pb)
	})
	return got[0], true, nil
}

// Put never writes. Ensure calls it only when the record is missing or different, so the status
// shows the record as out of sync, with this error.
func (l *Lookup) Put(context.Context, Record) error { return ErrNotWritten }
