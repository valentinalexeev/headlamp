package funnel

import (
	"strings"
	"testing"
)

func TestComposeRule(t *testing.T) {
	pol := `{"groups":{"group:a":["x@"]},"acls":[{"action":"accept","src":["group:a"],"dst":["*:*"]}]}`
	got, err := ComposeRule(pol, "tag:ingress", []string{"tag:lan:*", "tag:db:5432"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, `"group:a"`) || strings.Index(got, "groups") > strings.Index(got, "acls") {
		t.Errorf("other entries or key order lost:\n%s", got)
	}
	if !strings.Contains(got, `"src": [
        "tag:ingress"
      ],
      "dst": [
        "tag:db:5432",
        "tag:lan:*"
      ]`) {
		t.Errorf("rule:\n%s", got)
	}
	// idempotent: the same set returns the text unchanged
	if again, _ := ComposeRule(got, "tag:ingress", []string{"tag:db:5432", "tag:lan:*"}); again != got {
		t.Error("not idempotent")
	}
	// a changed set replaces the one rule
	got2, _ := ComposeRule(got, "tag:ingress", []string{"tag:lan:*"})
	if strings.Count(got2, "tag:ingress") != 1 || strings.Contains(got2, "5432") {
		t.Errorf("replace:\n%s", got2)
	}
	// empty removes it and keeps the rest
	got3, _ := ComposeRule(got2, "tag:ingress", nil)
	if strings.Contains(got3, "tag:ingress") || !strings.Contains(got3, "group:a") {
		t.Errorf("remove:\n%s", got3)
	}
	// a rule with tag:ingress among other sources is not ours
	mixed := `{"acls":[{"action":"accept","src":["tag:ingress","group:a"],"dst":["*:*"]}]}`
	if out, _ := ComposeRule(mixed, "tag:ingress", nil); out != mixed {
		t.Error("a mixed-src rule was touched")
	}
	if _, err := ComposeRule(`{"acls":[], // c
}`, "tag:ingress", nil); err != ErrNotPlainJSON {
		t.Errorf("hujson: %v", err)
	}
	if _, err := ComposeRule(`{"acls":{}}`, "tag:ingress", nil); err != ErrPolicyShape {
		t.Errorf("shape: %v", err)
	}
}
