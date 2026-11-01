package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
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
		"/_ui/hl.css":           "text/css",
		"/_ui/hl.js":            "text/javascript",
		"/_ui/theme.js":         "text/javascript",
		"/_ui/shell.js":         "text/javascript",
		"/_ui/palette.js":       "text/javascript",
		"/_ui/activity-text.js": "text/javascript",
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
	for name, body := range map[string][]byte{"css": hlCSS, "js": hlJS, "theme": themeJS, "shell": shellJS, "palette": paletteJS, "activity-text": activityTextJS} {
		if m := ext.Find(body); m != nil {
			t.Errorf("%s references something external: %q", name, m)
		}
	}
}

func TestJSForbidsMarkupInjection(t *testing.T) {
	for name, src := range map[string][]byte{"hl.js": hlJS, "theme.js": themeJS, "shell.js": shellJS, "palette.js": paletteJS, "activity-text.js": activityTextJS} {
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "setAttribute('style'", "setAttribute(\"style\""} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
	}
}

func TestShellSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/shell_selftest.js", "static/hl.js", "static/shell.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	for _, f := range []string{"static/shell.js", "static/theme.js", "static/activity-text.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", f, err, out)
		}
	}
}

func TestPaletteSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/palette_selftest.js", "static/hl.js", "static/palette.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/palette.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

// The palette asks for the pages and the search items and nothing else.
func TestPaletteRequestsOnlyNavAndSearch(t *testing.T) {
	src := string(paletteJS)
	if n := strings.Count(src, ".json'"); n != 2 || !strings.Contains(src, "'/_nav.json'") || !strings.Contains(src, "'/_search.json'") {
		t.Errorf("palette.js must name exactly /_nav.json and /_search.json, found %d quoted .json strings", n)
	}
	if strings.Count(src, "api(") != 2 || strings.Contains(src, "fetch(") || strings.Contains(src, "XMLHttpRequest") {
		t.Error("palette.js makes requests other than its two reads")
	}
}

func TestCSSHasFontFaces(t *testing.T) {
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

// A lowercase m after the first command is relative to the previous point and, after a
// closed shape, draws outside the viewBox. Only the first command may be lowercase.
func TestIconPathsUseAbsoluteMoves(t *testing.T) {
	for name, d := range iconPaths {
		if strings.Contains(d[1:], "m") {
			t.Errorf("%s: relative move after the first command: %q", name, d)
		}
	}
}

// The browser's default body margin shows white around every page, whichever theme is on.
func TestBodyMarginIsReset(t *testing.T) {
	css, err := os.ReadFile("static/hl.css")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(css), "body{margin:0}") {
		t.Error("hl.css must reset the body margin")
	}
}
