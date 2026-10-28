// Package auth decides which namespaces and ports a tailnet node may publish, from the tags the headscale
// ACL gave it. It is pure: no network, no clock.
package auth

import (
	"errors"
	"sort"
	"strings"
	"sync"

	"funnel-ingress/internal/config"
)

// Namespaces is the runtime-replaceable map from namespace to its allowed port ranges. Namespaces and ports
// come from the configuration source, so they change while the process runs.
type Namespaces struct {
	mu sync.RWMutex
	m  map[string][]config.PortRange
}

// NewNamespaces returns an empty set: nothing is allowed until the first configuration is applied.
func NewNamespaces() *Namespaces { return &Namespaces{m: map[string][]config.PortRange{}} }

// Set atomically replaces the whole map.
func (n *Namespaces) Set(m map[string][]config.PortRange) {
	c := make(map[string][]config.PortRange, len(m))
	for ns, rs := range m {
		c[ns] = append([]config.PortRange(nil), rs...)
	}
	n.mu.Lock()
	n.m = c
	n.mu.Unlock()
}

// Allowed reports whether port is inside one of the namespace's ranges. An unknown namespace or an empty
// range list allows nothing.
func (n *Namespaces) Allowed(ns string, port int) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	for _, r := range n.m[ns] {
		if port >= r.Lo && port <= r.Hi {
			return true
		}
	}
	return false
}

// Configured reports whether the namespace exists at all.
func (n *Namespaces) Configured(ns string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	_, ok := n.m[ns]
	return ok
}

// Names lists the configured namespaces, sorted.
func (n *Namespaces) Names() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	names := make([]string, 0, len(n.m))
	for ns := range n.m {
		names = append(names, ns)
	}
	sort.Strings(names)
	return names
}

// Policy turns a node's tags into publishing rights.
type Policy struct {
	FunnelTag, NSTagPrefix string
	NS                     *Namespaces
}

// Authorisation failures. They carry no detail on purpose: the API answers 403 or 422 with an empty body.
var (
	ErrForbidden         = errors.New("forbidden")
	ErrNamespaceRequired = errors.New("namespace required")
	ErrPortNotAllowed    = errors.New("port not allowed")
)

// NamespacesOf lists the namespaces named by the node's namespace tags, prefix stripped and sorted. It is nil
// unless the node carries the funnel tag (exact match).
func (p Policy) NamespacesOf(tags []string) []string {
	funnel := false
	for _, t := range tags {
		if t == p.FunnelTag {
			funnel = true
			break
		}
	}
	if !funnel {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for _, t := range tags {
		if !strings.HasPrefix(t, p.NSTagPrefix) {
			continue
		}
		ns := strings.TrimPrefix(t, p.NSTagPrefix)
		if ns == "" {
			continue
		}
		if _, dup := seen[ns]; dup {
			continue
		}
		seen[ns] = struct{}{}
		out = append(out, ns)
	}
	sort.Strings(out)
	return out
}

// Authorize checks that a node with these tags may publish on port in namespace ns, and returns the resolved
// namespace. ns may be empty when the node has exactly one namespace tag.
func (p Policy) Authorize(tags []string, ns string, port int) (string, error) {
	mine := p.NamespacesOf(tags)
	if len(mine) == 0 {
		return "", ErrForbidden
	}
	if ns == "" {
		if len(mine) != 1 {
			return "", ErrNamespaceRequired
		}
		ns = mine[0]
	}
	owned := false
	for _, m := range mine {
		if m == ns {
			owned = true
			break
		}
	}
	if !owned || !p.NS.Configured(ns) {
		return "", ErrForbidden
	}
	if !p.NS.Allowed(ns, port) {
		return "", ErrPortNotAllowed
	}
	return ns, nil
}
