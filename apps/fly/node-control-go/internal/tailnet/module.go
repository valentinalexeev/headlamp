// Package tailnet serves headscale's view of the tracked nodes.
package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

type module struct {
	src     headscale.NodeSource
	tracked func(context.Context) map[string]bool
}

// New returns the module serving GET /tailnet/status. tracked supplies the
// set of tailnet names this app controls.
func New(src headscale.NodeSource, tracked func(context.Context) map[string]bool) core.Module {
	return &module{src: src, tracked: tracked}
}

func (m *module) Name() string                { return "tailnet" }
func (m *module) Start(context.Context) error { return nil }
func (m *module) Register(r *core.Routes) error {
	r.Admin.HandleFunc("GET /tailnet/status", m.status)
	return nil
}

type entry struct {
	Name           string     `json:"name"`
	Online         bool       `json:"online"`
	LastSeen       *time.Time `json:"last_seen"`
	IPAddresses    []string   `json:"ip_addresses"`
	ApprovedCount  int        `json:"approved_route_count"`
	AvailableCount int        `json:"available_route_count"`
}

func (m *module) status(w http.ResponseWriter, req *http.Request) {
	nodes, err := m.src.Nodes(req.Context())
	if errors.Is(err, headscale.ErrNotConfigured) {
		writeJSON(w, 200, map[string]string{"status": "not_configured", "detail": err.Error()})
		return
	}
	if err != nil {
		writeJSON(w, 502, map[string]string{"detail": err.Error()})
		return
	}
	var tracked map[string]bool
	if m.tracked != nil {
		tracked = m.tracked(req.Context())
	}
	result := map[string]entry{}
	for _, n := range nodes {
		// A node is reported under the tracked name it matches: its own name
		// (a genuine "relay-1"), else its name without a "-N" re-registration suffix.
		key := n.Name
		if !tracked[key] {
			key = baseName(n.Name)
			if !tracked[key] {
				continue
			}
		}
		e := entry{
			Name: n.Name, Online: n.Online, LastSeen: n.LastSeen,
			IPAddresses: n.IPAddresses, ApprovedCount: len(n.ApprovedRoutes), AvailableCount: len(n.AvailableRoutes),
		}
		if e.IPAddresses == nil {
			e.IPAddresses = []string{}
		}
		if ex, ok := result[key]; !ok || (e.Online && !ex.Online) {
			result[key] = e
		}
	}
	writeJSON(w, 200, result)
}

// baseName strips a trailing "-N" (numeric) re-registration suffix.
func baseName(name string) string {
	i := strings.LastIndex(name, "-")
	if i <= 0 || i == len(name)-1 {
		return name
	}
	for _, c := range name[i+1:] {
		if c < '0' || c > '9' {
			return name
		}
	}
	return name[:i]
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
