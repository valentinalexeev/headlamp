// Package controlsync pulls funnel-ingress's configuration (public domain, namespaces and their allowed
// ports, manual routes, blocked routes) from node-control or a local file, applies it, caches the last good
// copy in the state store, and reports observed state back. The ingress is the only initiator: node-control
// never calls it, and a node-control outage leaves the last good configuration in force.
package controlsync

import (
	"encoding/json"
	"errors"
	"sort"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

// MaxBody caps what is read from the configuration source and sent to the state endpoint.
const MaxBody = 1 << 20

// Config is one version of the pulled configuration.
type Config struct {
	Version      string
	PublicDomain string
	Namespaces   map[string][]config.PortRange
	Routes       []routes.Route // Kind static
	Blocked      []routes.Key
}

// The wire shape, shared by node-control's 200 body, the local file source and the on-disk cache.
type wireRange struct {
	Lo int `json:"lo"`
	Hi int `json:"hi"`
}

type wireNS struct {
	Ports []wireRange `json:"ports"`
}

type wireRoute struct {
	NS                 string `json:"ns"`
	Service            string `json:"service"`
	Target             string `json:"target"`
	Port               int    `json:"port"`
	Scheme             string `json:"scheme,omitempty"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
}

type wireConfig struct {
	Version      string            `json:"version"`
	PublicDomain string            `json:"public_domain"`
	Namespaces   map[string]wireNS `json:"namespaces"`
	Routes       []wireRoute       `json:"routes"`
	Blocked      []routes.Key      `json:"blocked"`
}

// parse decodes a configuration body. It checks the shape only; Applier.Apply validates the values.
func parse(raw []byte) (Config, error) {
	var w wireConfig
	if err := json.Unmarshal(raw, &w); err != nil {
		return Config{}, errors.New("configuration is not valid JSON of the expected shape")
	}
	c := Config{Version: w.Version, PublicDomain: w.PublicDomain, Blocked: w.Blocked}
	if len(w.Namespaces) > 0 {
		c.Namespaces = make(map[string][]config.PortRange, len(w.Namespaces))
		for ns, v := range w.Namespaces {
			rs := make([]config.PortRange, 0, len(v.Ports))
			for _, p := range v.Ports {
				rs = append(rs, config.PortRange{Lo: p.Lo, Hi: p.Hi})
			}
			c.Namespaces[ns] = rs
		}
	}
	for _, r := range w.Routes {
		c.Routes = append(c.Routes, routes.Route{
			Key: routes.Key{NS: r.NS, Service: r.Service}, Target: r.Target, Port: r.Port,
			Scheme: r.Scheme, InsecureSkipVerify: r.InsecureSkipVerify, Kind: routes.Static,
		})
	}
	return c, nil
}

// marshal is the inverse of parse, with stable ordering.
func marshal(c Config) ([]byte, error) {
	w := wireConfig{Version: c.Version, PublicDomain: c.PublicDomain, Namespaces: map[string]wireNS{}, Blocked: c.Blocked}
	for ns, rs := range c.Namespaces {
		v := wireNS{Ports: []wireRange{}}
		for _, r := range rs {
			v.Ports = append(v.Ports, wireRange{Lo: r.Lo, Hi: r.Hi})
		}
		w.Namespaces[ns] = v
	}
	for _, r := range c.Routes {
		w.Routes = append(w.Routes, wireRoute{NS: r.NS, Service: r.Service, Target: r.Target, Port: r.Port, Scheme: r.Scheme, InsecureSkipVerify: r.InsecureSkipVerify})
	}
	sort.Slice(w.Routes, func(i, j int) bool {
		if w.Routes[i].NS != w.Routes[j].NS {
			return w.Routes[i].NS < w.Routes[j].NS
		}
		return w.Routes[i].Service < w.Routes[j].Service
	})
	return json.Marshal(w)
}
