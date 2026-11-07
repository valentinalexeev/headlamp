package serve

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

// The Serve page (#129): one page in the console shell, built like the Funnel page. The shell adds the navigation,
// the shared stylesheet and hl.js; the page script draws everything from GET /serve/state.

//go:embed static/serve.html
var pageHTML []byte

//go:embed static/serve.js
var pageJS []byte

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or style
// that is not from this origin.
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

// keyWarn is how long before a key expires the console starts to say so. A proxy that restarts after expiry
// cannot join again.
const keyWarn = 14 * 24 * time.Hour

func (m *Module) canView(login string) bool {
	return authz.Can(login, authz.P(authz.FeatureServe, authz.LevelView), authz.Allowed{})
}

// Nav puts the page in the top bar, in the operate group beside Funnel. An inactive module shows nothing.
func (m *Module) Nav() []core.NavItem {
	if m.st == nil {
		return nil
	}
	// Exactly the route gate: serve has no open mode, so with no policy the page shows to nobody, as its routes do.
	return []core.NavItem{{Label: "Serve", Path: "/serve", Order: 35, Group: "operate", Icon: "link", Source: "serve", Allow: m.canView}}
}

// audit records an admin action. A failure is logged, never shown: the action itself already happened.
func (m *Module) audit(r *http.Request, action, detail string) {
	if err := m.st.Audit(m.now().UTC(), authz.LoginFrom(r.Context()), action, detail); err != nil {
		m.log.Warn("serve: audit not written", "action", action, "error", err)
	}
}

// HasActivity reports whether the module keeps an activity log: it does when it is active.
func (m *Module) HasActivity() bool { return m.st != nil }

// Activity lists service and key changes for the console's activity page.
func (m *Module) Activity(_ context.Context, login string, limit int) ([]core.ActivityEntry, error) {
	if m.st == nil {
		return nil, nil
	}
	if !m.canView(login) {
		return nil, core.ErrNotVisible
	}
	es, err := m.st.AuditLog(limit)
	if err != nil {
		return nil, fmt.Errorf("serve store: %w", err)
	}
	out := make([]core.ActivityEntry, 0, len(es))
	for _, e := range es {
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "Serve", Action: e.Action, Detail: e.Detail})
	}
	return out, nil
}

// Search lists the services; each opens the Serve page.
func (m *Module) Search(_ context.Context, login string) ([]core.SearchItem, error) {
	if m.st == nil {
		return nil, nil
	}
	if !m.canView(login) {
		return nil, core.ErrNotVisible
	}
	svcs, err := m.st.Services()
	if err != nil {
		return nil, fmt.Errorf("serve store: %w", err)
	}
	out := make([]core.SearchItem, 0, len(svcs))
	for _, s := range svcs {
		out = append(out, core.SearchItem{Label: s.Name, Kind: "Serve service", Path: "/serve?service=" + s.ID})
	}
	return out, nil
}

// Attention reports a service whose newest key expires within keyWarn (or has expired), a service with no online
// proxy, and DNS answers that could not be refreshed. The system line counts services and online ones.
func (m *Module) Attention(ctx context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.st == nil {
		return nil, nil, nil
	}
	if !m.canView(login) {
		return nil, nil, core.ErrNotVisible
	}
	svcs, err := m.st.Services()
	if err != nil {
		return nil, nil, fmt.Errorf("serve store: %w", err)
	}
	nodes, nerr := m.hs.ListNodes(ctx)
	now := m.now()
	items, online := attention(svcs, nodes, nerr == nil, now)
	m.last.Lock()
	dnsErr, dnsAt := m.last.err, m.last.at
	m.last.Unlock()
	line := core.SystemLine{Name: "Serve", Path: "/serve", State: "ok",
		Summary: fmt.Sprintf("%d %s, %d online, zone %s", len(svcs), plural(len(svcs), "service", "services"), online, m.cfg.zone)}
	switch {
	case nerr != nil:
		line.State, line.Summary = "unknown", "headscale did not answer: online state unknown"
	case len(svcs) == 0:
		line.State, line.Summary = "off", "No services yet, zone "+m.cfg.zone
	case online < len(svcs):
		line.State = "waiting"
	}
	if dnsErr != "" {
		line.State = "waiting"
		items = append(items, core.AttentionItem{ID: "serve-dns", Source: "serve", Level: core.LevelWaiting, Since: dnsAt,
			Title: "Serve DNS answers are not refreshed", Detail: "The last refresh failed: " + clipText(dnsErr, 200) + ". Names keep their last answers.",
			ActionLabel: "Open Serve", ActionPath: "/serve", AckKey: "serve-dns"})
	}
	return items, []core.SystemLine{line}, nil
}

// attention is the pure part of Attention, for tests. nodesKnown is false when headscale did not answer: then no
// "offline" item is raised, because nothing is known.
func attention(svcs []Service, nodes []headscale.Node, nodesKnown bool, now time.Time) ([]core.AttentionItem, int) {
	var items []core.AttentionItem
	online := 0
	for _, s := range svcs {
		up := false
		for _, n := range nodes {
			if n.Online && n.PreAuthKeyID != 0 && slices.ContainsFunc(s.Keys, func(k Key) bool { return k.ID == n.PreAuthKeyID }) {
				up = true
				break
			}
		}
		if up {
			online++
		} else if nodesKnown {
			items = append(items, core.AttentionItem{ID: "serve-offline-" + s.ID, Source: "serve", Level: core.LevelWaiting,
				Title: s.Name + " has no online proxy", Detail: "No node that joined with its key is online, so the name answers nothing.",
				ActionLabel: "Open Serve", ActionPath: "/serve?service=" + s.ID, AckKey: "offline:" + s.ID})
		}
		if len(s.Keys) == 0 {
			continue
		}
		newest := s.Keys[0] // newest first: during a rotation the new key decides
		left := newest.Expires.Sub(now)
		switch {
		case left <= 0:
			items = append(items, core.AttentionItem{ID: "serve-key-" + s.ID, Source: "serve", Level: core.LevelCritical,
				Title: "The key of " + s.Name + " has expired", Detail: "A proxy that restarts cannot join again. Add a key and give it to the proxies.",
				ActionLabel: "Add a key", ActionPath: "/serve?service=" + s.ID, Deadline: newest.Expires,
				AckKey: fmt.Sprintf("expired:%s:%d", s.ID, newest.ID)})
		case left <= keyWarn:
			items = append(items, core.AttentionItem{ID: "serve-key-" + s.ID, Source: "serve", Level: core.LevelTimed,
				Title: "The key of " + s.Name + " expires soon", Detail: "Add a key, give it to the proxies, then retire the old key.",
				ActionLabel: "Add a key", ActionPath: "/serve?service=" + s.ID, Deadline: newest.Expires,
				AckKey: fmt.Sprintf("expiring:%s:%d", s.ID, newest.ID)})
		}
	}
	return items, online
}

// selfAddr is the IPv4 tailnet address of node-control in headscale's list: the one node with the node-control
// tag, else the node whose given name is this node's hostname. Two tagged nodes are ambiguous: then by name only.
func selfAddr(nodes []headscale.Node, name string) string {
	v4 := func(n headscale.Node) string {
		for _, a := range n.IPAddresses {
			if strings.Count(a, ".") == 3 {
				return a
			}
		}
		return ""
	}
	var tagged []headscale.Node
	for _, n := range nodes {
		if slices.Contains(n.Tags, nodeControlTag) {
			tagged = append(tagged, n)
		}
	}
	if len(tagged) == 1 {
		return v4(tagged[0])
	}
	for _, n := range nodes {
		if n.GivenName == name {
			return v4(n)
		}
	}
	return ""
}

// nodeControlTag is the tag the node-control node joins with (see the README's first start).
const nodeControlTag = "tag:node-control"

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func clipText(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
