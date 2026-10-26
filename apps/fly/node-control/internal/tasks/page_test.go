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

	"example.com/node-control/internal/core"
)

var pageAssets = []string{"/tasks/static/tasks.css", "/tasks/static/tasks.js"}

const wantPageCSP = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; " +
	"img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

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
	want := map[string]string{"/tasks/static/tasks.css": "text/css", "/tasks/static/tasks.js": "text/javascript"}
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
	scriptTag := regexp.MustCompile(`(?is)<script\b([^>]*)>(.*?)</script>`)
	for _, m := range scriptTag.FindAllStringSubmatch(after, -1) {
		if !strings.Contains(m[1], "src=") || strings.TrimSpace(m[2]) != "" {
			t.Errorf("inline script: %q", m[0])
		}
	}
	if regexp.MustCompile(`(?i)\son[a-z]+\s*=`).MatchString(after) {
		t.Error("inline event handler in the markup")
	}
	if regexp.MustCompile(`(?i)\sstyle\s*=`).MatchString(after) || strings.Contains(strings.ToLower(after), "<style") {
		t.Error("inline style in the markup")
	}
	if strings.Contains(after, "<h1>Tasks</h1></body>") || strings.Contains(after, "job") {
		t.Error("placeholder text or task data in the markup")
	}
	if !strings.Contains(after, "/tasks/static/tasks.js") || !strings.Contains(after, "/tasks/static/tasks.css") {
		t.Error("assets not linked")
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

// TestTasksPageScriptSelfTest checks the pure helpers of tasks.js with node, when node exists.
func TestTasksPageScriptSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/tasks_page_selftest.js", "static/tasks.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/tasks.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}
