// Package fleet reports which version of each part runs (#195): node-control, headscale, the
// tailscale client of every node, and the agent and xray of every bridge. It is read only: it
// changes nothing and asks no bridge.
package fleet

import (
	"context"
	_ "embed"
	"encoding/json"
	"net/http"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/enroll"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

//go:embed static/fleet.html
var pageHTML []byte

//go:embed static/fleet.js
var pageJS []byte

// Deps are the sources of the report. Each may be nil: its part is then left out.
type Deps struct {
	Nodes     headscale.NodeSource
	Headscale interface {
		Version(ctx context.Context) (string, error)
	}
	Bridges func(ctx context.Context) []enroll.IngressView
	// Others lists the other modules, for the navigation rule; nil: no nav entry.
	Others func() []core.Module
	// Funnel is the status of the funnel ingress from its last report; false when there is none.
	Funnel func() (funnel.IngressStatus, bool)
	// TailscalePin is the tailscale version the fleet should run. Default: the tailscale.com
	// module node-control is built with, which the image build checks against the repo's
	// TAILSCALE_VERSION.
	TailscalePin string
}

type module struct {
	d    Deps
	peer core.PeerVersions
}

// New returns the fleet module.
func New(d Deps) core.Module {
	if d.TailscalePin == "" {
		d.TailscalePin = builtTailscale()
	}
	return &module{d: d}
}

func (m *module) Name() string                { return "fleet" }
func (m *module) Start(context.Context) error { return nil }
func (m *module) Register(r *core.Routes) error {
	if pv, ok := r.Tailnet.(core.PeerVersions); ok {
		m.peer = pv
	}
	r.Admin.HandleFunc("GET /fleet.json", m.report)
	r.Admin.HandleFunc("GET /fleet", asset("text/html; charset=utf-8", pageHTML))
	r.Admin.HandleFunc("GET /fleet/fleet.js", asset("text/javascript; charset=utf-8", pageJS))
	return nil
}

// navOrder places Fleet after Namespaces & keys (60) and before Access (90).
const navOrder = 80

// Nav puts Fleet in the configure group, for anyone who may view the overview. Like Overview it
// needs at least two other pages: a smaller console is the compact one-page console, with no
// sidebar to hold it. The page and /fleet.json are served either way.
func (m *module) Nav() []core.NavItem {
	if m.d.Others == nil {
		return nil
	}
	if items, err := core.CollectNav(m.d.Others()); err != nil || len(items) < 2 {
		return nil
	}
	return []core.NavItem{{Label: "Fleet", Path: "/fleet", Order: navOrder, Group: "configure",
		Icon: "system", Allow: authz.NavAllow(authz.P(authz.FeatureOverview, authz.LevelView))}}
}

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any
// script or style that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", ui.PageCSP)
		_, _ = w.Write(body)
	}
}

// builtTailscale is the version of the tailscale.com module in this binary, without the "v".
func builtTailscale() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, dep := range bi.Deps {
		if dep.Path == "tailscale.com" {
			return strings.TrimPrefix(dep.Version, "v")
		}
	}
	return ""
}

// Report is the answer of GET /fleet.json.
type Report struct {
	NodeControl    string   `json:"node_control"`
	Headscale      string   `json:"headscale,omitempty"`
	HeadscaleError string   `json:"headscale_error,omitempty"`
	TailscalePin   string   `json:"tailscale_pin,omitempty"`
	Nodes          []Node   `json:"nodes"`
	NodesError     string   `json:"nodes_error,omitempty"`
	Bridges        []Bridge `json:"bridges"`
	Funnel         *Funnel  `json:"funnel,omitempty"`
	// Problems need action: a part that does not answer, a bridge out of sync or offline, a funnel
	// ingress that does not report, a server on an older tailscale than the pin. Notes are worth a
	// look but are not failures (an agent that reports no version). scripts/fleet-check.sh exits
	// non-zero on a problem.
	Problems []string `json:"problems"`
	Notes    []string `json:"notes"`
}

// Funnel is the funnel ingress, from its last report to node-control.
type Funnel struct {
	Version    string    `json:"version,omitempty"`
	ReportedAt time.Time `json:"reported_at"`
	Stale      bool      `json:"stale"`
	Errors     int       `json:"errors"`
}

// Node is one tailnet node and its tailscale client.
type Node struct {
	Name      string     `json:"name"`
	Online    bool       `json:"online"`
	LastSeen  *time.Time `json:"last_seen,omitempty"`
	Tags      []string   `json:"tags,omitempty"`
	Tailscale string     `json:"tailscale,omitempty"` // e.g. "1.104.5"; "" when unknown
	// Status compares Tailscale with the pin: current, older, newer, or unknown.
	Status string `json:"status"`
}

// Bridge is one bridge and what its agent last reported.
type Bridge struct {
	Name   string `json:"name"`
	Source string `json:"source"`
	Online *bool  `json:"online"`
	// Agent is "" for a configured bridge or an agent before 0.16.0.
	Agent  string     `json:"agent,omitempty"`
	Xray   string     `json:"xray,omitempty"`
	InfoAt *time.Time `json:"info_at,omitempty"`
	Active bool       `json:"active"` // used for profiles and sync
	// Sync is the last push of credentials to the agent: ok, pending, or the state the
	// enrolment module shows ("" when not known).
	Sync string `json:"sync,omitempty"`
}

func (m *module) report(w http.ResponseWriter, req *http.Request) {
	ctx := req.Context()
	rep := Report{NodeControl: core.DisplayVersion(), TailscalePin: m.d.TailscalePin,
		Nodes: []Node{}, Bridges: []Bridge{}, Problems: []string{}, Notes: []string{}}
	if m.d.Headscale != nil {
		if v, err := m.d.Headscale.Version(ctx); err != nil {
			rep.HeadscaleError = err.Error()
			rep.problem("headscale: " + err.Error())
		} else {
			rep.Headscale = v
		}
	}
	if m.d.Nodes != nil {
		nodes, err := m.d.Nodes.Nodes(ctx)
		if err != nil {
			rep.NodesError = err.Error()
			rep.problem("node list: " + err.Error())
		}
		for _, n := range nodes {
			e := Node{Name: n.GivenName, Online: n.Online, LastSeen: n.LastSeen, Tags: n.Tags}
			if e.Name == "" {
				e.Name = n.Name
			}
			if ip := firstIP(n.IPAddresses); ip != "" && m.peer != nil {
				if v, err := m.peer.PeerVersion(ctx, ip); err == nil {
					e.Tailscale = shortVersion(v)
				}
			}
			e.Status = compare(e.Tailscale, rep.TailscalePin)
			// Only servers (tagged nodes) are held to the pin; a person's device updates itself.
			if e.Status == "older" && len(e.Tags) > 0 {
				rep.problem(e.Name + ": tailscale " + e.Tailscale +
					" is older than the pin " + rep.TailscalePin)
			}
			rep.Nodes = append(rep.Nodes, e)
		}
		sort.Slice(rep.Nodes, func(i, j int) bool { return rep.Nodes[i].Name < rep.Nodes[j].Name })
	}
	if m.d.Bridges != nil {
		for _, v := range m.d.Bridges(ctx) {
			b := Bridge{Name: v.Name, Source: v.Source, Online: v.Online,
				Agent: v.AgentVersion, Xray: v.XrayVersion, InfoAt: v.InfoAt, Active: v.Active}
			if v.Sync != nil {
				b.Sync = v.Sync.State
				if v.Sync.Pending {
					b.Sync = "pending"
				}
			}
			rep.checkBridge(b)
			rep.Bridges = append(rep.Bridges, b)
		}
	}
	if m.d.Funnel != nil {
		if s, ok := m.d.Funnel(); ok {
			rep.Funnel = &Funnel{Version: s.Version, ReportedAt: s.ReportedAt, Stale: s.Stale,
				Errors: s.Errors}
			switch {
			case s.Stale:
				rep.problem("funnel ingress: no report since " +
					s.ReportedAt.UTC().Format(time.RFC3339))
			case s.Errors > 0:
				rep.problem("funnel ingress: its last report has errors")
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(rep)
}

func (r *Report) problem(s string) { r.Problems = append(r.Problems, s) }

// checkBridge adds the problems of an active bridge: offline, or credentials not pushed. A
// bridge that is not active (waiting for approval) is the enrolment page's business, not a
// fleet failure.
func (r *Report) checkBridge(b Bridge) {
	if !b.Active {
		return
	}
	if b.Online != nil && !*b.Online {
		r.problem("bridge " + b.Name + ": offline")
	}
	if b.Sync == "pending" {
		r.problem("bridge " + b.Name + ": credentials not in sync")
	}
	if b.Source == "discovered" && b.Agent == "" {
		r.Notes = append(r.Notes,
			"bridge "+b.Name+": the agent reports no version (before 0.16.0)")
	}
}

func firstIP(ips []string) string {
	for _, ip := range ips {
		if !strings.Contains(ip, ":") {
			return ip
		}
	}
	if len(ips) > 0 {
		return ips[0]
	}
	return ""
}

// shortVersion keeps the release part of a tailscale long version:
// "1.104.5-t0123abcd-g4567" is "1.104.5".
func shortVersion(v string) string {
	v, _, _ = strings.Cut(strings.TrimSpace(v), "-")
	if _, ok := parse(v); !ok {
		return ""
	}
	return v
}

// versionParts is the number of parts of a release version: major.minor.patch.
const versionParts = 3

func parse(v string) ([versionParts]int, bool) {
	var out [versionParts]int
	parts := strings.Split(strings.TrimPrefix(v, "v"), ".")
	if len(parts) != versionParts {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// compare says how version v relates to the pin: current, older, newer, or unknown.
func compare(v, pin string) string {
	a, ok1 := parse(v)
	b, ok2 := parse(pin)
	if !ok1 || !ok2 {
		return "unknown"
	}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return "older"
		case a[i] > b[i]:
			return "newer"
		}
	}
	return "current"
}
