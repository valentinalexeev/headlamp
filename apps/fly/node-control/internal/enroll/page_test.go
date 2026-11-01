package enroll

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"example.com/node-control/internal/ui"
)

func TestEnrollPageHeaders(t *testing.T) {
	f := newAdminFx(t)
	for path, ctype := range map[string]string{"/enroll": "text/html", "/enroll/static/enroll.js": "text/javascript", "/enroll/qrcode.js": "text/javascript"} {
		rec := f.do("GET", path, "", "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s CSP: %q", path, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, h)
		}
		if !strings.HasPrefix(h.Get("Content-Type"), ctype) {
			t.Errorf("%s content type %q", path, h.Get("Content-Type"))
		}
	}
	if rec := f.do("GET", "/enroll?enrol=1&filter=expiring", "", ""); rec.Code != 200 {
		t.Errorf("page with parameters: %d", rec.Code)
	}
}

// The page and its scripts carry no inline code and no markup built from strings, and the enrolment link is
// never an anchor or a confirm()/prompt() dialog.
func TestEnrollPageNoInlineScript(t *testing.T) {
	html := string(enrollHTML)
	scripts := regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1)
	if len(scripts) != 2 {
		t.Errorf("want the QR library and the page script, got %v", scripts)
	}
	for _, m := range scripts {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	if !strings.Contains(html, `src="/enroll/qrcode.js"`) || !strings.Contains(html, `src="/enroll/static/enroll.js"`) {
		t.Error("page lacks its scripts")
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", " onload=", " onsubmit=", "<template"} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	for _, m := range regexp.MustCompile(`(?i)\s(?:src|href)="([^"]*)"`).FindAllStringSubmatch(html, -1) {
		if !strings.HasPrefix(m[1], "/") || strings.HasPrefix(m[1], "//") {
			t.Errorf("reference %q is not a local path", m[1])
		}
	}
	js := string(enrollJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "confirm(", "prompt(", "XMLHttpRequest", "alert("} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	// the SVG namespace is an identifier, not a request
	if regexp.MustCompile(`https?://`).MatchString(strings.ReplaceAll(js, "http://www.w3.org/2000/svg", "")) {
		t.Error("script reaches outside the origin")
	}
}

// The bridge table moved to Nodes: the page no longer renders it, but the data stays on /enroll/ingresses.
func TestEnrollPageHasNoBridgeTable(t *testing.T) {
	page := string(enrollHTML)
	for _, gone := range []string{`id="ingresses"`, "<th>Bridge</th>", "Approve", "headscale nodes list"} {
		if strings.Contains(page, gone) || strings.Contains(string(enrollJS), gone) {
			t.Errorf("the Devices page still holds %q", gone)
		}
	}
	f := newAdminFx(t)
	if rec := f.do("GET", "/enroll/ingresses", "", ""); rec.Code != 200 {
		t.Errorf("GET /enroll/ingresses = %d", rec.Code)
	}
}

func TestPageDrawsTheLinkAsQR(t *testing.T) {
	if !strings.Contains(string(enrollJS), "q.isDark(") || !strings.Contains(string(enrollHTML), `src="/enroll/qrcode.js"`) {
		t.Error("the page does not draw the link with the QR encoder")
	}
	if len(qrcodeJS) == 0 || !strings.Contains(string(qrcodeJS), "Kazuhiko Arase") {
		t.Error("the embedded QR encoder (with its licence header) is missing")
	}
}

func TestApprovePageHeaders(t *testing.T) {
	f := newAdminFx(t)
	for path, ctype := range map[string]string{"/enroll/approve": "text/html", "/enroll/static/approve.js": "text/javascript"} {
		rec := f.do("GET", path, "", "")
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s CSP: %q", path, h.Get("Content-Security-Policy"))
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, h)
		}
		if !strings.HasPrefix(h.Get("Content-Type"), ctype) {
			t.Errorf("%s content type %q", path, h.Get("Content-Type"))
		}
	}
	if rec := f.do("GET", "/enroll/approve?name=bridge-3", "", ""); rec.Code != 200 {
		t.Errorf("approve with a name: %d", rec.Code)
	}
}

func TestApprovePageHasNoInlineCodeOrInjection(t *testing.T) {
	html := string(approveHTML)
	for _, m := range regexp.MustCompile(`(?i)<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", " onload="} {
		if strings.Contains(strings.ToLower(html), bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	js := string(approveJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "confirm(", "prompt(", "XMLHttpRequest"} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
}

// The enrolment scripts run under node against a small DOM stand-in (skipped when node is missing).
func TestEnrollPagesSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	args := []string{"testdata/enroll_selftest.js", "../ui/static/hl.js", "static/approve.js", "static/enroll.js"}
	if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	for _, f := range []string{"static/approve.js", "static/enroll.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", f, err, out)
		}
	}
}
