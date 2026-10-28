package enroll

import (
	"regexp"
	"strings"
	"testing"
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
