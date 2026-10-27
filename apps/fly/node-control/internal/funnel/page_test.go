package funnel

import (
	"regexp"
	"strings"
	"testing"
)

func TestPageServedToAdminWithSecurityHeaders(t *testing.T) {
	r := newRig(t, nil)
	for path, ctype := range map[string]string{
		"/funnel": "text/html", "/funnel/static/funnel.js": "text/javascript", "/funnel/static/funnel.css": "text/css",
	} {
		res := r.admin("GET", path, nil)
		if res.code != 200 || !strings.HasPrefix(res.hdr.Get("Content-Type"), ctype) {
			t.Fatalf("%s: %d %s", path, res.code, res.hdr.Get("Content-Type"))
		}
		if res.hdr.Get("X-Frame-Options") != "DENY" || res.hdr.Get("X-Content-Type-Options") != "nosniff" || res.hdr.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, res.hdr)
		}
		csp := res.hdr.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s CSP: %q", path, csp)
		}
	}
}

func TestPageHasNoExternalReferences(t *testing.T) {
	ext := regexp.MustCompile(`(?i)(src|href|action)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|@import|<iframe|<object|<embed`)
	for name, body := range map[string][]byte{"html": pageHTML, "js": pageJS, "css": pageCSS} {
		if m := ext.Find(body); m != nil {
			t.Errorf("%s references something external: %q", name, m)
		}
	}
	if strings.Contains(string(pageHTML), "<script>") || regexp.MustCompile(`(?i)\son[a-z]+\s*=`).Match(pageHTML) {
		t.Error("the page has inline script")
	}
	// Server text goes in through textContent only.
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval("} {
		if strings.Contains(string(pageJS), bad) {
			t.Errorf("funnel.js uses %s", bad)
		}
	}
}

func TestPage403ForNonAdmin(t *testing.T) {
	r := newRig(t, nil)
	if res := r.do("GET", "/funnel", bobIP, nil); res.code != 403 || res.body != "" {
		t.Fatalf("%d %q", res.code, res.body)
	}
}
