package controlsync

import (
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

// Applier validates a configuration and installs it.
type Applier struct {
	Table              *routes.Table
	NS                 *auth.Namespaces
	Domain             *atomic.Pointer[string]
	ProxyPort, APIPort int
}

// ValidDomain normalises a public domain (lower case, no trailing dot) and checks it has at least two valid
// DNS labels.
func ValidDomain(d string) (string, bool) {
	d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), ".")
	labels := strings.Split(d, ".")
	if len(labels) < 2 {
		return "", false
	}
	for _, l := range labels {
		if !routes.ValidName(l) {
			return "", false
		}
	}
	return d, true
}

// Apply validates cfg completely before changing anything, then installs namespaces, domain, static routes
// and blocked keys. It returns the keys of dynamic routes that this configuration removed (taken over by a
// static route or blocked). On a validation error nothing changes.
func (a *Applier) Apply(cfg Config) ([]routes.Key, error) {
	domain, ok := ValidDomain(cfg.PublicDomain)
	if !ok {
		return nil, errors.New("rejected configuration: public_domain is missing or not a valid domain")
	}
	for ns, rs := range cfg.Namespaces {
		if !routes.ValidName(ns) {
			return nil, errors.New("rejected configuration: invalid namespace name")
		}
		if err := config.ValidateRanges(rs); err != nil {
			return nil, fmt.Errorf("rejected configuration: namespace %s: %v", ns, err)
		}
		for _, r := range rs {
			for _, own := range []int{a.ProxyPort, a.APIPort} {
				if own >= r.Lo && own <= r.Hi {
					return nil, fmt.Errorf("rejected configuration: namespace %s allows port %d, which is one of the ingress's own ports", ns, own)
				}
			}
		}
	}
	seen := map[routes.Key]bool{}
	statics := make([]routes.Route, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		r.Kind = routes.Static
		if err := r.Validate(); err != nil {
			return nil, fmt.Errorf("rejected configuration: invalid route %s/%s", r.NS, r.Service)
		}
		if seen[r.Key] {
			return nil, fmt.Errorf("rejected configuration: duplicate route %s/%s", r.NS, r.Service)
		}
		seen[r.Key] = true
		statics = append(statics, r)
	}
	for _, k := range cfg.Blocked {
		if !routes.ValidName(k.NS) || !routes.ValidName(k.Service) {
			return nil, errors.New("rejected configuration: invalid blocked entry")
		}
	}

	a.NS.Set(cfg.Namespaces)
	a.Domain.Store(&domain)
	removed := a.Table.SetBlocked(cfg.Blocked)
	removed = append(removed, a.Table.SetStatic(statics)...)
	return removed, nil
}
