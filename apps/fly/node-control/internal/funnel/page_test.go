package funnel

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/ui"
)

// pages lists every page of the module with its script.
var pages = []struct {
	name, path, js string
	html, script   []byte
}{
	{"funnel", "/funnel", "/funnel/static/funnel.js", pageHTML, pageJS},
	{"config", "/funnel/config", "/funnel/static/config.js", configHTML, configJS},
}

func TestPagesServedToAdminWithSecurityHeaders(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range pages {
		for path, ctype := range map[string]string{p.path: "text/html", p.js: "text/javascript"} {
			res := r.admin("GET", path, nil)
			if res.code != 200 || !strings.HasPrefix(res.hdr.Get("Content-Type"), ctype) {
				t.Fatalf("%s: %d %s", path, res.code, res.hdr.Get("Content-Type"))
			}
			if res.hdr.Get("X-Frame-Options") != "DENY" || res.hdr.Get("X-Content-Type-Options") != "nosniff" || res.hdr.Get("Cache-Control") != "no-store" {
				t.Errorf("%s headers: %v", path, res.hdr)
			}
			if csp := res.hdr.Get("Content-Security-Policy"); csp != ui.PageCSP {
				t.Errorf("%s CSP: %q", path, csp)
			}
		}
	}
	if res := r.admin("GET", "/funnel/static/funnel.css", nil); res.code == 200 {
		t.Error("the old stylesheet is still served")
	}
}

func TestPagesHaveNoInlineCodeOrExternalReferences(t *testing.T) {
	for _, p := range pages {
		html := string(p.html)
		for _, m := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
			if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
				t.Errorf("%s: script tag %q must be a same-origin deferred file", p.name, m)
			}
		}
		for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", " onload=", " onsubmit=", "<template", "<link"} {
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

func TestPages403ForNonAdmin(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range pages {
		for _, path := range []string{p.path, p.js} {
			if res := r.do("GET", path, bobIP, nil); res.code != 403 || res.body != "" {
				t.Fatalf("%s: %d %q", path, res.code, res.body)
			}
		}
	}
}

// The scripts run under node against a small DOM stand-in (skipped when node is missing).
func TestFunnelPagesSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	args := []string{"testdata/funnel_selftest.js", "../ui/static/hl.js", "static/funnel.js", "static/config.js"}
	if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	for _, f := range args[2:] {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", f, err, out)
		}
	}
}

func TestFunnelConfigPage403ForNonAdmin(t *testing.T) {
	r := newRig(t, nil)
	for _, path := range []string{"/funnel/config", "/funnel/static/config.js"} {
		for name, ip := range map[string]string{"other user": bobIP, "ingress node": taggedIP, "no login": nobodyIP} {
			if res := r.do("GET", path, ip, nil); res.code != 403 || res.body != "" {
				t.Errorf("%s %s: %d %q", name, path, res.code, res.body)
			}
		}
		res := r.admin("GET", path, nil)
		if res.code != 200 || res.hdr.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s as admin: %d CSP %q", path, res.code, res.hdr.Get("Content-Security-Policy"))
		}
	}
}
