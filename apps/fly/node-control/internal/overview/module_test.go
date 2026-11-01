package overview

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/ui"
)

// stub is a module with navigation entries.
type stub struct {
	name  string
	items []core.NavItem
}

func (s stub) Name() string                { return s.name }
func (s stub) Start(context.Context) error { return nil }
func (s stub) Register(*core.Routes) error { return nil }
func (s stub) Nav() []core.NavItem         { return s.items }

func twoPages() []core.Module {
	return []core.Module{
		stub{"nodes", []core.NavItem{{Label: "Nodes", Path: "/nodes", Order: 10}}},
		stub{"tasks", []core.NavItem{{Label: "Tasks", Path: "/tasks", Order: 40}}},
	}
}

func serve(t *testing.T, others []core.Module, path string) (*httptest.ResponseRecorder, core.Module) {
	t.Helper()
	m := New(func() []core.Module { return others })
	mux := http.NewServeMux()
	if err := m.Register(&core.Routes{Admin: mux}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec, m
}

func TestOverviewPageSecurityHeaders(t *testing.T) {
	for _, p := range []string{"/", "/overview/overview.js"} {
		rec, _ := serve(t, twoPages(), p)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s CSP: %q", p, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", p, h)
		}
	}
	rec, _ := serve(t, twoPages(), "/")
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content type %q", ct)
	}
}

func TestOverviewNav(t *testing.T) {
	m := New(func() []core.Module { return twoPages() })
	nav := m.(core.Navigator).Nav()
	if len(nav) != 1 || nav[0].Label != "Overview" || nav[0].Path != "/" || nav[0].Order != 1 || nav[0].Group != "" || nav[0].Icon == "" {
		t.Fatalf("%+v", nav)
	}
	if err := core.ValidateNavItem(nav[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := core.CollectNav(append(twoPages(), m)); err != nil {
		t.Fatal(err)
	}
}

func TestOverviewOneModuleRedirects(t *testing.T) {
	one := []core.Module{stub{"nodes", []core.NavItem{{Label: "Nodes", Path: "/nodes", Order: 10}}}}
	rec, m := serve(t, one, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/nodes" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	if nav := m.(core.Navigator).Nav(); len(nav) != 0 {
		t.Fatalf("Nav() must be empty with one other page: %+v", nav)
	}
}

func TestOverviewNoPagesIsNotFound(t *testing.T) {
	rec, m := serve(t, []core.Module{stub{"quiet", nil}}, "/")
	if rec.Code != http.StatusNotFound || len(m.(core.Navigator).Nav()) != 0 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestOverviewDoesNotShadowOtherPaths(t *testing.T) {
	rec, _ := serve(t, twoPages(), "/nodes")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("%d", rec.Code)
	}
}

func TestOverviewNoInlineScriptOrExternalRefs(t *testing.T) {
	html := string(pageHTML)
	for _, m := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", "//cdn", " onclick=", " onload=", " onerror="} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	for _, m := range regexp.MustCompile(`(?i)\s(?:src|href)="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//") {
			t.Errorf("reference %q is not a local path", m[1])
		}
	}
	js := string(pageJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function"} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	if strings.Contains(js, "XMLHttpRequest") || regexp.MustCompile(`https?://`).MatchString(js) {
		t.Error("script reaches outside the origin")
	}
}

func TestOverviewSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/overview_selftest.js", "../ui/static/hl.js", "static/overview.js", "../ui/static/activity-text.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/overview.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
	if _, err := os.Stat("../ui/static/hl.js"); err != nil {
		t.Fatal(err)
	}
}

// logged is a module that keeps an activity log, with a page of its own.
type logged struct {
	stub
	gate *bool // nil: the module cannot say; else what HasActivity answers
}

func (l logged) Activity(context.Context, string, int) ([]core.ActivityEntry, error) { return nil, nil }

type gatedLogged struct{ logged }

func (g gatedLogged) HasActivity() bool { return *g.gate }

func pagesWith(extra ...core.Module) []core.Module {
	return append([]core.Module{
		stub{"nodes", []core.NavItem{{Label: "Nodes", Path: "/nodes", Order: 10}}},
		stub{"tasks", []core.NavItem{{Label: "Tasks", Path: "/tasks", Order: 40}}},
	}, extra...)
}

func TestActivityHiddenWhenNoSource(t *testing.T) {
	rec, m := serve(t, twoPages(), "/activity")
	nav := m.(core.Navigator).Nav()
	if len(nav) != 1 || nav[0].Label != "Overview" {
		t.Fatalf("no module keeps a log, so there is no Activity entry: %+v", nav)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("/activity = %d", rec.Code)
	}

	// a source that says it has no log (nodes without the shared audit log) does not count
	off := false
	rec, m = serve(t, pagesWith(gatedLogged{logged{stub{"nodes2", nil}, &off}}), "/activity")
	if len(m.(core.Navigator).Nav()) != 1 || rec.Code != http.StatusNotFound {
		t.Fatalf("a source without a log: %d %+v", rec.Code, m.(core.Navigator).Nav())
	}
	on := true
	rec, m = serve(t, pagesWith(gatedLogged{logged{stub{"nodes2", nil}, &on}}), "/activity")
	if len(m.(core.Navigator).Nav()) != 2 || rec.Code != http.StatusOK {
		t.Fatalf("a source with a log: %d %+v", rec.Code, m.(core.Navigator).Nav())
	}
}

func TestActivityNavEntry(t *testing.T) {
	rec, m := serve(t, pagesWith(logged{stub{"enroll", []core.NavItem{{Label: "Devices", Path: "/enroll", Order: 20}}}, nil}), "/activity")
	nav := m.(core.Navigator).Nav()
	if len(nav) != 2 || nav[1].Label != "Activity" || nav[1].Path != "/activity" || nav[1].Order != 900 || nav[1].Group != "" || nav[1].Icon != "activity" {
		t.Fatalf("%+v", nav)
	}
	for _, n := range nav {
		if err := core.ValidateNavItem(n); err != nil {
			t.Fatal(err)
		}
	}
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Content-Security-Policy") != ui.PageCSP {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	js, _ := serve(t, pagesWith(logged{stub{"enroll", nil}, nil}), "/activity/activity.js")
	if js.Code != 200 || !strings.HasPrefix(js.Header().Get("Content-Type"), "text/javascript") {
		t.Fatalf("%d", js.Code)
	}
}

// With one other page the console has no sidebar and "/" opens that page, so Activity must not appear: it would
// make a second entry and a sidebar the compact console is meant not to have.
func TestActivityDoesNotBreakTheSinglePageConsole(t *testing.T) {
	one := []core.Module{logged{stub{"enroll", []core.NavItem{{Label: "Devices", Path: "/enroll", Order: 20}}}, nil}}
	rec, m := serve(t, one, "/")
	if rec.Code != http.StatusFound || rec.Header().Get("Location") != "/enroll" {
		t.Fatalf("%d %q", rec.Code, rec.Header().Get("Location"))
	}
	if nav := m.(core.Navigator).Nav(); len(nav) != 0 {
		t.Fatalf("the single-page console has no entries of its own: %+v", nav)
	}
	if rec, _ := serve(t, one, "/activity"); rec.Code != http.StatusNotFound {
		t.Fatalf("/activity = %d", rec.Code)
	}
}

func TestActivityPageHasNoInlineScriptOrExternalRefs(t *testing.T) {
	html := string(activityHTML)
	for _, m := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick="} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	js := string(activityJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "XMLHttpRequest"} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
}

func TestActivitySelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/activity_selftest.js", "../ui/static/hl.js", "static/activity.js", "../ui/static/activity-text.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/activity.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

// Both pages load the shared sentences before their own script.
func TestPagesLoadTheSharedActivityText(t *testing.T) {
	for name, html := range map[string]string{"overview": string(pageHTML), "activity": string(activityHTML)} {
		shared := strings.Index(html, `<script src="/_ui/activity-text.js" defer></script>`)
		own := strings.Index(html, `<script src="/`+map[string]string{"overview": "overview/overview.js", "activity": "activity/activity.js"}[name])
		if shared < 0 || own < shared {
			t.Errorf("%s: shared %d, own %d", name, shared, own)
		}
	}
}

// The headline may run to two lines on a phone: it needs the wrap class, which the stylesheet defines (see the ui tests).
func TestOverviewHeadlineMayWrap(t *testing.T) {
	m := regexp.MustCompile(`<div class="([^"]*)"><span[^>]*id="glyph"`).FindSubmatch(pageHTML)
	if m == nil || !strings.Contains(" "+string(m[1])+" ", " wrap ") {
		t.Errorf("the headline line has no wrap class: %q", m)
	}
}
