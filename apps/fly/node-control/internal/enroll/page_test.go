package enroll

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"example.com/node-control/internal/ui"
)

func TestPageHasTheIngressTable(t *testing.T) {
	page := string(enrollHTML)
	t.Run("container, columns and buttons", func(t *testing.T) {
		for _, want := range []string{
			`id="ingresses"`, `id="ingrows"`, "<th>Bridge</th>", "<th>Online</th>", "<th>Sync</th>", "<th>Since</th>", "<th>Approval</th>",
			`"Approve"`, `"Remove approval"`, "/approve", "/unapprove", "fingerprint: v.fingerprint, node_id: v.node_id", "headscale nodes list", "v.address", "plain(v.hostname)", "plain(v.given_name)", "machine_key_prefix", "v.fingerprint.slice(0, 12)", "Fingerprint \" + v.fingerprint", "/enroll/ingresses",
		} {
			if !strings.Contains(page, want) {
				t.Errorf("page lacks %q", want)
			}
		}
	})
	t.Run("hidden unless there is something to show", func(t *testing.T) {
		if !regexp.MustCompile(`#ingresses\s*\{\s*display:none;`).MatchString(page) {
			t.Error("the table must be hidden by default")
		}
		if !strings.Contains(page, "(j.ingresses || []).length > 1 || (j.pending_ingresses || []).length > 0") {
			t.Error("the table rule (several ingresses, or one pending) is missing")
		}
	})
	t.Run("banner names every lagging ingress, single case unchanged", func(t *testing.T) {
		for _, want := range []string{"behind.map((r) => r.name)", "out of sync since", "bridge host out of sync since"} {
			if !strings.Contains(page, want) {
				t.Errorf("page lacks %q", want)
			}
		}
	})
	t.Run("partial traffic marker", func(t *testing.T) {
		for _, want := range []string{`id="trafficth"`, `j.stats_partial ? "Traffic*" : "Traffic"`, "th.title"} {
			if !strings.Contains(page, want) {
				t.Errorf("page lacks %q", want)
			}
		}
	})
	t.Run("server values are only ever set as text", func(t *testing.T) {
		if strings.Contains(page, "innerHTML") {
			t.Error("innerHTML would let a published value inject markup")
		}
	})
	t.Run("dark only", func(t *testing.T) {
		if strings.Contains(page, "prefers-color-scheme: light") {
			t.Error("the page is dark-only")
		}
	})
}

func TestPageDrawsTheLinkAsQR(t *testing.T) {
	page := string(enrollHTML)
	for _, want := range []string{`id="outqr"`, `src="/enroll/qrcode.js"`, "qr(text)", "q.isDark("} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
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
	args := []string{"testdata/enroll_selftest.js", "../ui/static/hl.js", "static/approve.js"}
	if out, err := exec.Command(node, args...).CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	for _, f := range []string{"static/approve.js"} {
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("node --check %s: %v\n%s", f, err, out)
		}
	}
}
