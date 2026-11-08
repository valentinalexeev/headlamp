package funnel

import (
	"errors"
	"strings"
	"testing"
)

const grpBase = `{"tagOwners":{"tag:ingress":["e2e@"]},"acls":[{"action":"accept","src":["group:a"],"dst":["*:*"]}]}`

func TestComposeGroupRulesAddChangeRemove(t *testing.T) {
	want := map[string][]string{"tag:ingress-g-aa": {"tag:nas:445", "tag:nas:22"}, "tag:ingress-g-bb": {"100.64.0.20:631"}}
	got, err := ComposeGroupRules(grpBase, "tag:ingress-g-", "tag:ingress", want)
	if err != nil {
		t.Fatal(err)
	}
	have, _ := GroupRules(got, "tag:ingress-g-")
	if len(have) != 2 || strings.Join(have["tag:ingress-g-aa"], ",") != "tag:nas:22,tag:nas:445" {
		t.Fatalf("rules %v\n%s", have, got)
	}
	if !strings.Contains(got, `"tag:ingress-g-bb": [`) || !strings.Contains(got, "group:a") {
		t.Errorf("owners or other rules:\n%s", got)
	}
	if again, _ := ComposeGroupRules(got, "tag:ingress-g-", "tag:ingress", want); again != got {
		t.Error("not idempotent")
	}
	delete(want, "tag:ingress-g-bb")
	got2, _ := ComposeGroupRules(got, "tag:ingress-g-", "tag:ingress", want)
	if strings.Contains(got2, "ingress-g-bb") || !strings.Contains(got2, "ingress-g-aa") {
		t.Errorf("remove one:\n%s", got2)
	}
	got3, _ := ComposeGroupRules(got2, "tag:ingress-g-", "tag:ingress", nil)
	if strings.Contains(got3, "ingress-g-") || !strings.Contains(got3, `"tag:ingress"`) {
		t.Errorf("remove all:\n%s", got3)
	}
}

func TestComposeGroupRulesRefusals(t *testing.T) {
	w := map[string][]string{"tag:ingress-g-aa": {"tag:nas:22"}}
	if _, err := ComposeGroupRules(`{"acls":[]}`, "tag:ingress-g-", "tag:ingress", w); !errors.Is(err, ErrNoTagOwner) {
		t.Errorf("no owner: %v", err)
	}
	if _, err := ComposeGroupRules(grpBase, "tag:ingress-g-", "tag:ingress", map[string][]string{"tag:x": {"a"}}); !errors.Is(err, ErrPolicyShape) {
		t.Errorf("foreign tag: %v", err)
	}
	if _, err := ComposeGroupRules(`{"acls":[]} // c`, "tag:ingress-g-", "tag:ingress", nil); !errors.Is(err, ErrNotPlainJSON) {
		t.Errorf("hujson: %v", err)
	}
	// the phase 1 rule (src tag:ingress) is not a group rule
	p1 := `{"acls":[{"action":"accept","src":["tag:ingress"],"dst":["tag:lan:*"]}]}`
	if out, _ := ComposeGroupRules(p1, "tag:ingress-g-", "tag:ingress", nil); out != p1 {
		t.Error("phase 1 rule touched")
	}
}
