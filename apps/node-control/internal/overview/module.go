// Package overview serves the console's front page: what needs attention now, one line per system and
// the latest activity. The page is static; it reads /_attention.json and /_activity.json, which the core
// builds from the modules that implement core.AttentionSource and core.ActivitySource.
package overview

import (
	"context"
	_ "embed"
	"net/http"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

//go:embed static/overview.html
var pageHTML []byte

//go:embed static/overview.js
var pageJS []byte

//go:embed static/activity.html
var activityHTML []byte

//go:embed static/activity.js
var activityJS []byte

// Module is the Overview page. It owns no data of its own.
type Module struct {
	others func() []core.Module
}

// New returns the module. others lists every other module; it is called after all of them registered, so
// the navigation it counts is complete.
func New(others func() []core.Module) core.Module { return &Module{others: others} }

func (m *Module) Name() string                { return "overview" }
func (m *Module) Start(context.Context) error { return nil }

// pages are the navigation entries of the other modules. A collection error is left for the core, which
// reports it when it collects everything.
func (m *Module) pages() []core.NavItem {
	if m.others == nil {
		return nil
	}
	items, err := core.CollectNav(m.others())
	if err != nil {
		return nil
	}
	return items
}

// hasActivity reports whether some other module keeps an activity log: it implements ActivitySource and, when it
// can say so, is active and has a log to read.
func (m *Module) hasActivity() bool {
	if m.others == nil {
		return false
	}
	for _, o := range m.others() {
		if _, ok := o.(core.ActivitySource); !ok {
			continue
		}
		if g, ok := o.(core.ActivityGate); !ok || g.HasActivity() {
			return true
		}
	}
	return false
}

// activityNav is the Activity entry, or false when the page does not exist. Like Overview it needs at least two
// other pages: with fewer the console is the compact one-page console, which has no sidebar, so there is nothing to
// hold the entry (and "/" redirects to the one page).
func (m *Module) activityNav() (core.NavItem, bool) {
	if len(m.pages()) < 2 || !m.hasActivity() {
		return core.NavItem{}, false
	}
	return core.NavItem{Label: "Activity", Path: "/activity", Order: 900, Icon: "activity", Allow: authz.NavAllow(authz.P(authz.FeatureAudit, authz.LevelView))}, true
}

// Nav lists Overview first, with no group, but only when there are at least two other pages: with one
// the console opens straight on that page, and with none there is nothing to overview. Activity comes last, with
// no group, when some module keeps an activity log.
func (m *Module) Nav() []core.NavItem {
	if len(m.pages()) < 2 {
		return nil
	}
	items := []core.NavItem{{Label: "Overview", Path: "/", Order: 1, Icon: "overview", Allow: authz.NavAllow(authz.P(authz.FeatureOverview, authz.LevelView))}}
	if a, ok := m.activityNav(); ok {
		items = append(items, a)
	}
	return items
}

func (m *Module) Register(r *core.Routes) error {
	r.Admin.HandleFunc("GET /{$}", m.page)
	r.Admin.HandleFunc("GET /overview/overview.js", asset("text/javascript; charset=utf-8", pageJS))
	r.Admin.HandleFunc("GET /activity", m.activityPage)
	r.Admin.HandleFunc("GET /activity/activity.js", asset("text/javascript; charset=utf-8", activityJS))
	return nil
}

func (m *Module) activityPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := m.activityNav(); !ok {
		http.NotFound(w, r)
		return
	}
	asset("text/html; charset=utf-8", activityHTML)(w, r)
}

func (m *Module) page(w http.ResponseWriter, r *http.Request) {
	switch pages := m.pages(); len(pages) {
	case 0:
		http.NotFound(w, r)
	case 1:
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, pages[0].Path, http.StatusFound)
	default:
		asset("text/html; charset=utf-8", pageHTML)(w, r)
	}
}

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or
// style that is not from this origin.
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
