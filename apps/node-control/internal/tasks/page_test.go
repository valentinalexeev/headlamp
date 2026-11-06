package tasks

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

var pageAssets = []string{"/tasks/static/tasks.js"}

// The pages use the shared console stylesheet and no images, so the shared page policy is all they need.
const wantPageCSP = ui.PageCSP

func TestTasksPageServedBehindGate(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	for _, p := range append([]string{"/tasks"}, pageAssets...) {
		for _, addr := range []string{bobAddr, taggedAddr} {
			if rec := h.do("GET", p, addr, ""); rec.Code != 403 {
				t.Errorf("GET %s from %s = %d", p, addr, rec.Code)
			}
		}
		rec := h.alice("GET", p, "")
		if rec.Code != 200 {
			t.Fatalf("alice GET %s = %d", p, rec.Code)
		}
		hd := rec.Header()
		if hd.Get("Content-Security-Policy") != wantPageCSP {
			t.Errorf("%s CSP = %q", p, hd.Get("Content-Security-Policy"))
		}
		if hd.Get("Cache-Control") != "no-store" {
			t.Errorf("%s Cache-Control = %q", p, hd.Get("Cache-Control"))
		}
	}
}

func TestTasksPageAssetsServed(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	want := map[string]string{"/tasks/static/tasks.js": "text/javascript"}
	for p, ct := range want {
		rec := h.alice("GET", p, "")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) {
			t.Errorf("%s = %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Body.Len() < 200 {
			t.Errorf("%s nosniff=%q len=%d", p, rec.Header().Get("X-Content-Type-Options"), rec.Body.Len())
		}
	}
	if rec := h.alice("GET", "/tasks", ""); rec.Header().Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Errorf("page headers: %v", rec.Header())
	}
}

func TestTasksPageHasNoInlineScriptOrDataInMarkup(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	before := h.alice("GET", "/tasks", "").Body.String()
	h.pushSimple()
	after := h.alice("GET", "/tasks", "").Body.String()
	if before != after {
		t.Fatal("the markup changes with the task state")
	}
	if strings.Contains(after, "job") {
		t.Error("task data in the markup")
	}
	if !strings.Contains(after, "/tasks/static/tasks.js") {
		t.Error("script not linked")
	}
	if rec := h.alice("GET", "/tasks/static/tasks.css", ""); rec.Code == 200 {
		t.Error("the old stylesheet is still served")
	}
}

// Both pages are plain markup with one deferred same-origin script: no inline code, no styles of their own, no
// outside references. The scripts never build markup from strings.
func TestTasksAndSessionsPagesHaveNoInlineCodeOrExternalReferences(t *testing.T) {
	for _, p := range []struct {
		name         string
		html, script []byte
	}{{"tasks", pageHTML, pageJS}, {"sessions", sessionsHTML, sessionsJS}} {
		html := string(p.html)
		scripts := regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1)
		if len(scripts) != 1 {
			t.Errorf("%s: %d script tags", p.name, len(scripts))
		}
		for _, m := range scripts {
			if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
				t.Errorf("%s: script tag %q must be a same-origin deferred file", p.name, m)
			}
		}
		for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", " onload=", "<link", "<template"} {
			if strings.Contains(strings.ToLower(html), bad) {
				t.Errorf("%s: page contains %q", p.name, bad)
			}
		}
		for _, m := range regexp.MustCompile(`(?i)\s(?:src|href)="([^"]*)"`).FindAllStringSubmatch(html, -1) {
			if !strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//") {
				t.Errorf("%s: reference %q is not a local path", p.name, m[1])
			}
		}
		js := string(p.script)
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "confirm(", "prompt(", "alert(", "XMLHttpRequest", "localStorage", "sessionStorage"} {
			if strings.Contains(js, bad) {
				t.Errorf("%s: script contains %q", p.name, bad)
			}
		}
		if regexp.MustCompile(`https?://`).MatchString(js) {
			t.Errorf("%s: script reaches outside the origin", p.name)
		}
	}
}

func TestSessionsPageServedWithPagePolicy(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	for _, p := range []string{"/sessions", "/sessions/static/sessions.js"} {
		rec := h.do("GET", p, aliceAddr, "")
		if rec.Code != 200 || rec.Header().Get("Content-Security-Policy") != wantPageCSP || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s: %d CSP %q", p, rec.Code, rec.Header().Get("Content-Security-Policy"))
		}
	}
}

func TestTasksPageStillServedWhenDegraded(t *testing.T) {
	env := modEnv("redis://:pw-secret-123@" + closedPort(t))
	mux := http.NewServeMux()
	tm := New(core.MapEnv(env), testNodes()).(*Module)
	routes := &core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: gateTailnet(),
		Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))}
	if err := tm.Register(routes); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := tm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer scancel()
		tm.Stop(sctx)
	}()
	get := func(p string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", p, nil)
		req.RemoteAddr = aliceAddr
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/tasks.json"); rec.Code != 503 {
		t.Fatalf("tasks.json = %d, want 503", rec.Code)
	}
	for _, p := range append([]string{"/tasks"}, pageAssets...) {
		if rec := get(p); rec.Code != 200 {
			t.Errorf("degraded GET %s = %d", p, rec.Code)
		}
	}
}

func TestTasksStaticRouteDoesNotShadowTaskRoutes(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	h.pushSimple()
	for _, p := range []string{"/tasks/config", "/tasks/secrets"} {
		rec := h.alice("GET", p, "")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
			t.Errorf("GET %s = %d %q", p, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	for _, p := range []string{"/tasks/job", "/tasks/job/runs"} {
		rec := h.alice("GET", p, "")
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/json" {
			t.Errorf("GET %s = %d %q %s", p, rec.Code, rec.Header().Get("Content-Type"), rec.Body)
		}
	}
	// A task called "static" still resolves, as does an unknown file under the static path.
	if rec := h.alice("GET", "/tasks/static", ""); rec.Header().Get("Content-Type") != "application/json" {
		t.Errorf("GET /tasks/static = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec := h.alice("GET", "/tasks/static/nothing.js", ""); rec.Code != 404 {
		t.Errorf("unknown static file = %d", rec.Code)
	}
	_ = httptest.NewRecorder
}

// TestTasksPageScriptSelfTest checks both page scripts against a small DOM stand-in with node, when node exists.
func TestTasksPageScriptSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/tasks_page_selftest.js", "../ui/static/hl.js", "static/tasks.js", "static/sessions.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	for _, f := range []string{"static/tasks.js", "static/sessions.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", f, err, out)
		}
	}
}
