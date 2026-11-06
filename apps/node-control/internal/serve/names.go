// Package serve publishes tailnet services under private names, the equivalent of tailscale serve (#117). Names
// live under the headscale MagicDNS base_domain and resolve only on the tailnet. node-control is the only authority:
// it checks every name against the namespace of the node that asks, signs the node's certificate, and publishes the
// record list that a records-sync agent next to headscale writes to extra_records_path.
package serve

import (
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// maxNameLen is the DNS limit for a full name without the trailing dot.
const maxNameLen = 253

// ErrName is returned for a name the policy refuses. The message says why, for the caller's log.
var ErrName = errors.New("name refused")

func refuse(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrName, fmt.Sprintf(format, a...))
}

// NormalizeName lower-cases n and removes one trailing dot.
func NormalizeName(n string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(n)), ".")
}

// validLabel reports whether l is one DNS label: 1 to 63 of a-z, 0-9 and "-", not starting or ending with "-".
func validLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 || l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := 0; i < len(l); i++ {
		c := l[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// Policy decides which names a namespace may claim.
type Policy struct {
	// BaseDomain is the headscale MagicDNS base_domain. Every name is under it.
	BaseDomain string
	// Suffixes maps a namespace to the zones its nodes may claim names in, written in full ("cluster.<base>"), as the
	// store keeps them. A name must end in ".<zone>". The zone itself is not claimable, so one node cannot take a
	// whole zone.
	Suffixes map[string][]string
	// NodeNames reports whether a full name is a MagicDNS node name; such names are refused, so serve never
	// shadows a node.
	NodeNames func(name string) bool
}

// Check returns the normalised name if namespace ns may claim it, or an ErrName.
func (p Policy) Check(ns, name string) (string, error) {
	n := NormalizeName(name)
	base := NormalizeName(p.BaseDomain)
	if base == "" {
		return "", refuse("no base domain configured")
	}
	if len(n) > maxNameLen {
		return "", refuse("name longer than %d characters", maxNameLen)
	}
	for _, l := range strings.Split(n, ".") {
		if !validLabel(l) {
			return "", refuse("%q is not a valid DNS name", name)
		}
	}
	if !strings.HasSuffix(n, "."+base) {
		return "", refuse("%q is not under the base domain %s", name, base)
	}
	ok := false
	for _, s := range p.Suffixes[ns] {
		zone := NormalizeName(s)
		if (zone == base || strings.HasSuffix(zone, "."+base)) && strings.HasSuffix(n, "."+zone) {
			ok = true
			break
		}
	}
	if !ok {
		return "", refuse("namespace %q may not use %q", ns, name)
	}
	if p.NodeNames != nil && p.NodeNames(n) {
		return "", refuse("%q is the name of a tailnet node", name)
	}
	return n, nil
}

// CheckSuffix validates a suffix an admin adds to a namespace, written without the base domain ("cluster" or
// "v2.apps"), and returns the full zone the store keeps ("cluster.<base>").
func CheckSuffix(s, base string) (string, error) {
	n := NormalizeName(s)
	b := NormalizeName(base)
	if b == "" {
		return "", refuse("no base domain configured")
	}
	if n == "" {
		return "", refuse("empty suffix")
	}
	if n == b || strings.HasSuffix(n, "."+b) {
		return "", refuse("write the suffix without the base domain")
	}
	for _, l := range strings.Split(n, ".") {
		if !validLabel(l) {
			return "", refuse("%q is not a valid DNS suffix", s)
		}
	}
	return n + "." + b, nil
}

// Record is one DNS answer published to headscale.
type Record struct {
	Name string     `json:"name"`
	Type string     `json:"type"`
	IP   netip.Addr `json:"value"`
}

// RecordFor builds the A or AAAA record for name at a tailnet address. Only tailnet addresses are allowed, so a
// name can never point outside the tailnet.
func RecordFor(name string, ip netip.Addr) (Record, error) {
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	ula := netip.MustParsePrefix("fd7a:115c:a1e0::/48")
	switch {
	case ip.Is4() && cgnat.Contains(ip):
		return Record{Name: name, Type: "A", IP: ip}, nil
	case ip.Is6() && ula.Contains(ip):
		return Record{Name: name, Type: "AAAA", IP: ip}, nil
	}
	return Record{}, refuse("%s is not a tailnet address", ip)
}
