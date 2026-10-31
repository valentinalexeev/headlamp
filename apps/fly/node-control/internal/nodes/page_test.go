package nodes

import (
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"example.com/node-control/internal/ui"
)

func TestNodesPageHeaders(t *testing.T) {
	h := newHandler(t, testConfig(t), nil, nil, 0)
	for _, p := range []string{"/nodes", "/nodes/nodes.js"} {
		rec := do(h, "GET", p)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		hd := rec.Header()
		if hd.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s CSP: %q", p, hd.Get("Content-Security-Policy"))
		}
		if hd.Get("X-Content-Type-Options") != "nosniff" || hd.Get("X-Frame-Options") != "DENY" || hd.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", p, hd)
		}
	}
	if ct := do(h, "GET", "/nodes").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("page content type %q", ct)
	}
	if ct := do(h, "GET", "/nodes/nodes.js").Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
		t.Errorf("script content type %q", ct)
	}
}

func TestNodesPageNoInjectionOrExternalRefs(t *testing.T) {
	html := string(pageHTML)
	for _, m := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", "//cdn", " onclick=", " onload=", " onerror=", "<template"} {
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

// The page owns /nodes only: the root belongs to Overview, and the JSON and action routes stay.
func TestNodesRootRouteMoved(t *testing.T) {
	h := newHandler(t, testConfig(t), nil, nil, 0)
	if rec := do(h, "GET", "/"); rec.Code != http.StatusNotFound {
		t.Errorf("GET / = %d", rec.Code)
	}
	for _, p := range []string{"/nodes", "/nodes.json", "/status"} {
		if rec := do(h, "GET", p); rec.Code != 200 {
			t.Errorf("GET %s = %d", p, rec.Code)
		}
	}
	if rec := do(h, "GET", "/nodes/nodes.js"); rec.Code != 200 {
		t.Errorf("script = %d", rec.Code)
	}
	if rec := do(h, "GET", "/nodes/up/status"); rec.Code != 200 {
		t.Errorf("node status = %d", rec.Code)
	}
}

func TestNodesPageSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/nodes_selftest.js", "../ui/static/hl.js", "static/nodes.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/nodes.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}
