package ui

import (
	"net/http"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; font-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func TestPageCSPValue(t *testing.T) {
	if PageCSP != wantCSP {
		t.Fatalf("PageCSP = %q", PageCSP)
	}
}

func get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func TestHandlerServesAssets(t *testing.T) {
	for path, ctype := range map[string]string{
		"/_ui/hl.css": "text/css",
		"/_ui/hl.js":  "text/javascript",
		"/_ui/fonts/ibm-plex-sans-latin-400-normal.woff2": "font/woff2",
		"/_ui/fonts/ibm-plex-mono-latin-500-normal.woff2": "font/woff2",
	} {
		rec := get(path)
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ctype) || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %q (%d bytes)", path, rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
		}
		if rec.Header().Get("X-Content-Type-Options") != "nosniff" || rec.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, rec.Header())
		}
	}
	for _, path := range []string{"/_ui/nothing.js", "/_ui/", "/_ui/fonts/", "/_ui/../ui.go", "/_ui/fonts/OFL.txt/x"} {
		if rec := get(path); rec.Code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, rec.Code)
		}
	}
}

func TestHandlerOnlyGetAndHead(t *testing.T) {
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, httptest.NewRequest("POST", "/_ui/hl.css", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST = %d", rec.Code)
	}
}

func TestAssetsHaveNoExternalReferences(t *testing.T) {
	ext := regexp.MustCompile(`(?i)(src|href|action)\s*=\s*["']?(https?:)?//|url\(\s*["']?(https?:)?//|@import|<iframe|<object|<embed`)
	for name, body := range map[string][]byte{"css": hlCSS, "js": hlJS} {
		if m := ext.Find(body); m != nil {
			t.Errorf("%s references something external: %q", name, m)
		}
	}
}

func TestJSForbidsMarkupInjection(t *testing.T) {
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('style'", "setAttribute(\"style\""} {
		if strings.Contains(string(hlJS), bad) {
			t.Errorf("hl.js uses %s", bad)
		}
	}
}

func TestCSSHasNoInlineStyleDependency(t *testing.T) {
	if !strings.Contains(string(hlCSS), "@font-face") {
		t.Error("hl.css has no @font-face")
	}
}

var iconNames = strings.Fields("overview nodes devices funnel tasks sessions policy key activity search plus system sun moon play stop ban trash extend link copy close external menu check undo edit")

func TestIconsAreSafeAndComplete(t *testing.T) {
	bad := regexp.MustCompile(`(?i)<style|<script|\son[a-z]+\s*=|\sstyle\s*=|javascript:|href`)
	for _, n := range iconNames {
		s := Icon(n)
		if !strings.HasPrefix(s, "<svg ") || !strings.HasSuffix(s, "</svg>") || !strings.Contains(s, `aria-hidden="true"`) || !strings.Contains(s, `class="ico"`) {
			t.Errorf("%s: %q", n, s)
		}
		if bad.MatchString(s) {
			t.Errorf("%s: unsafe markup %q", n, s)
		}
	}
	for _, n := range []string{"", "nope", `"><script>`, "OVERVIEW"} {
		if Icon(n) != "" {
			t.Errorf("Icon(%q) should be empty", n)
		}
	}
	if len(iconNames) != 27 {
		t.Errorf("icon list changed: %d", len(iconNames))
	}
}

func TestHLSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/hl_selftest.js", "static/hl.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/hl.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

func TestIconPathsMatchScript(t *testing.T) {
	for name, d := range iconPaths {
		if !strings.Contains(string(hlJS), "'"+d+"'") {
			t.Errorf("hl.js has no matching path for %s", name)
		}
	}
	if len(iconPaths) != len(iconNames) {
		t.Errorf("%d paths for %d names", len(iconPaths), len(iconNames))
	}
}
