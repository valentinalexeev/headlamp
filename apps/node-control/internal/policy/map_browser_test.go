package policy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

// ADR 0005 section 5 (#429): the map page, with the vendored Cytoscape.js, runs in a real browser under ui.PageCSP
// with no CSP violation. The test needs Chrome or Chromium (HEADLAMP_CHROME, else the usual names on PATH); it is
// skipped without one, unless HEADLAMP_BROWSER_TEST=require.

// cspProbe records every CSP violation of the page on the root element, where --dump-dom shows it.
const cspProbe = `document.addEventListener('securitypolicyviolation', function (e) {
  var r = document.documentElement;
  r.setAttribute('data-csp-violation', (r.getAttribute('data-csp-violation') || '') + e.violatedDirective + ' ' + e.blockedURI + ';');
});`

// cspControl breaks the policy on purpose, so the test proves that the probe sees a violation.
const cspControl = `var s = document.createElement('style'); s.textContent = 'p{color:red}'; document.head.appendChild(s);`

func findChrome(t *testing.T) string {
	t.Helper()
	if p := os.Getenv("HEADLAMP_CHROME"); p != "" {
		return p
	}
	for _, name := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "chrome"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	if os.Getenv("HEADLAMP_BROWSER_TEST") == "require" {
		t.Fatal("HEADLAMP_BROWSER_TEST=require but no Chrome or Chromium was found (set HEADLAMP_CHROME)")
	}
	t.Skip("no Chrome or Chromium found (set HEADLAMP_CHROME)")
	return ""
}

// browserSite serves the policy module as the admin listener does: /_ui/ assets, and each module page with the
// assets that the shell adds (theme, hl.css, hl.js). The probe goes first in <head>. Requests come from the admin.
func browserSite(h *harness) http.Handler {
	js := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
			w.Header().Set("Content-Security-Policy", ui.PageCSP)
			w.Write([]byte(body))
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/_ui/", ui.Handler())
	mux.Handle("GET /_test/probe.js", js(cspProbe))
	mux.Handle("GET /_test/control.js", js(cspControl))
	mux.HandleFunc("GET /_test/control", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", ui.PageCSP)
		w.Write([]byte(`<!doctype html><html><head><script src="/_test/probe.js"></script></head>` +
			`<body><p>control</p><script src="/_test/control.js" defer></script></body></html>`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		r.RemoteAddr = adminAddr
		rec := httptest.NewRecorder()
		h.mux.ServeHTTP(rec, r)
		for k, v := range rec.Header() {
			w.Header()[k] = v
		}
		body := rec.Body.Bytes()
		if strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
			page := strings.Replace(string(body), "<head>", `<head><script src="/_test/probe.js"></script>`, 1)
			page = strings.Replace(page, "<body>", `<body><script src="/_ui/theme.js"></script><link rel="stylesheet" href="/_ui/hl.css">`+
				`<script src="/_ui/hl.js" defer></script>`, 1)
			body = []byte(page)
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(rec.Code)
		w.Write(body)
	})
	return mux
}

// dumpDOM loads url in headless Chrome, lets the page run for some virtual seconds, and returns the DOM.
func dumpDOM(t *testing.T, chrome, url string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, chrome, "--headless=new", "--no-sandbox", "--disable-gpu", "--no-first-run",
		"--no-default-browser-check", "--disable-extensions", "--user-data-dir="+t.TempDir(), "--window-size=1280,900",
		"--virtual-time-budget=10000", "--dump-dom", url)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("chrome %s: %v", url, err)
	}
	return string(out)
}

func TestMapPageRunsUnderPageCSP(t *testing.T) {
	chrome := findChrome(t)
	h := newHarness(t, baseEnv(t))
	h.setPolicy(mapPolicy)
	srv := httptest.NewServer(browserSite(h))
	t.Cleanup(srv.Close)

	control := dumpDOM(t, chrome, srv.URL+"/_test/control")
	if !strings.Contains(control, "data-csp-violation=") {
		t.Fatalf("the probe saw no violation on the control page, so it cannot prove anything:\n%s", control)
	}

	dom := dumpDOM(t, chrome, srv.URL+"/policy/map")
	if i := strings.Index(dom, "data-csp-violation="); i >= 0 {
		end, _, _ := strings.Cut(dom[i:], ">")
		t.Errorf("CSP violation on /policy/map: %s", end)
	}
	if !strings.Contains(dom, `data-map-state="ready"`) {
		t.Errorf("the map did not load:\n%s", dom)
	}
	if !strings.Contains(dom, "<canvas") {
		t.Error("Cytoscape.js drew no canvas")
	}
	if strings.Contains(dom, "The graph could not start") {
		t.Error("the page fell back to the table")
	}
	if !strings.Contains(dom, `data-edge="e0"`) {
		t.Error("the table has no row for the edge of the policy")
	}
}
