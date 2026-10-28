// Package sidecar is the Kubernetes registration sidecar of funnel-ingress. It joins the tailnet as its own
// ephemeral, tagged node, forwards TCP from its tailnet address to a pod-local or in-cluster target, and
// registers the services with funnel-ingress. The backend of every route is therefore the sidecar's own
// tailnet address, which keeps the ingress's trust rule.
package sidecar

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"funnel-ingress/internal/routes"
)

// Service is one published service: it listens on the tailnet at TailnetPort and forwards to Target.
type Service struct {
	Name        string
	TailnetPort int
	Target      string // host:port
}

// ParseServices parses "name=port" (forward to 127.0.0.1:port) or "name=port@host:port2", comma separated.
func ParseServices(s string) ([]Service, error) {
	if strings.TrimSpace(s) == "" {
		return nil, errors.New("no services")
	}
	var out []Service
	names := map[string]bool{}
	ports := map[int]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		name, rest, ok := strings.Cut(item, "=")
		if !ok || !routes.ValidName(name) {
			return nil, fmt.Errorf("invalid service %q: want name=port[@host:port] with a DNS-label name", item)
		}
		portStr, target, hasTarget := strings.Cut(rest, "@")
		port, err := parsePort(portStr)
		if err != nil {
			return nil, fmt.Errorf("service %s: %v", name, err)
		}
		if hasTarget {
			host, tp, err := net.SplitHostPort(target)
			if err != nil || host == "" {
				return nil, fmt.Errorf("service %s: the target after @ must be host:port", name)
			}
			if _, err := parsePort(tp); err != nil {
				return nil, fmt.Errorf("service %s: target %v", name, err)
			}
		} else {
			target = net.JoinHostPort("127.0.0.1", portStr)
		}
		if names[name] {
			return nil, fmt.Errorf("duplicate service name %s", name)
		}
		if ports[port] {
			return nil, fmt.Errorf("port %d is used by two services", port)
		}
		names[name], ports[port] = true, true
		out = append(out, Service{Name: name, TailnetPort: port, Target: target})
	}
	return out, nil
}

func parsePort(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("port must be 1-65535")
	}
	return n, nil
}
