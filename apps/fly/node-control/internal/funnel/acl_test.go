package funnel

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func aclSettings() *Settings {
	return &Settings{FunnelTag: "tag:funnel", NSPrefix: "tag:ns-", IngressTag: "tag:funnel-ingress", TagOwners: []string{"group:admins", "alice@example.com"}}
}

const basePolicy = `{
  "groups": {"group:admins": ["alice@example.com"]},
  "tagOwners": {
    "tag:funnel": ["group:admins"],
    "tag:funnel-ingress": ["group:admins"]
  },
  "acls": [
    {"action": "accept", "src": ["tag:funnel"], "dst": ["tag:funnel-ingress:9100"]},
    {"action": "accept", "src": ["tag:funnel-ingress"], "dst": ["tag:node-control:80"]}
  ],
  "ssh": []
}
`

var pdl = map[string][]Range{"pdl": {{8080, 8080}, {9000, 9010}}}

func decodeAny(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, s)
	}
	return m
}

func TestFormatPortsMergesAndSorts(t *testing.T) {
	cases := []struct {
		in   []Range
		want string
	}{
		{nil, ""},
		{[]Range{{8080, 8080}}, "8080"},
		{[]Range{{9000, 9010}, {8080, 8080}}, "8080,9000-9010"},
		{[]Range{{1, 2}, {3, 4}}, "1-4"},
		{[]Range{{10, 20}, {15, 18}, {19, 30}}, "10-30"},
		{[]Range{{5, 5}, {5, 5}}, "5"},
	}
	for _, c := range cases {
		if got := FormatPorts(c.in); got != c.want {
			t.Errorf("FormatPorts(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAddsOwnerAndRulePerNamespace(t *testing.T) {
	out, err := Compose(basePolicy, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	m := decodeAny(t, out)
	owners := m["tagOwners"].(map[string]any)["tag:ns-pdl"].([]any)
	if len(owners) != 2 || owners[0] != "group:admins" || owners[1] != "alice@example.com" {
		t.Fatalf("owners = %v", owners)
	}
	acls := m["acls"].([]any)
	last := acls[len(acls)-1].(map[string]any)
	if last["action"] != "accept" || last["src"].([]any)[0] != "tag:funnel-ingress" || last["dst"].([]any)[0] != "tag:ns-pdl:8080,9000-9010" {
		t.Fatalf("rule = %v", last)
	}
	if len(acls) != 3 {
		t.Fatalf("acls = %d entries", len(acls))
	}
}

func TestIsIdempotent(t *testing.T) {
	once, err := Compose(basePolicy, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Compose(once, pdl, aclSettings())
	if err != nil || twice != once {
		t.Fatalf("second run changed the text (err %v)", err)
	}
	// An unmanaged rule appended after the managed ones must not make the next run reshuffle.
	withExtra := strings.Replace(once, `"ssh": []`, `"ssh": [], "extra": true`, 1)
	if got, _ := Compose(withExtra, pdl, aclSettings()); got != withExtra {
		t.Fatal("unchanged managed entries must return the original text")
	}
}

func TestUnchangedPolicyReturnsOriginalText(t *testing.T) {
	out, err := Compose(basePolicy, nil, aclSettings())
	if err != nil || out != basePolicy {
		t.Fatalf("no namespaces and none present must be a no-op: %v", err)
	}
}

func TestRemovesEntriesForDeletedNamespace(t *testing.T) {
	two := map[string][]Range{"pdl": {{8080, 8080}}, "docs": {{3000, 3000}}}
	with, _ := Compose(basePolicy, two, aclSettings())
	out, err := Compose(with, map[string][]Range{"docs": {{3000, 3000}}}, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "tag:ns-pdl") || !strings.Contains(out, "tag:ns-docs") {
		t.Fatalf("pdl should be gone and docs kept:\n%s", out)
	}
	none, err := Compose(out, nil, aclSettings())
	if err != nil || strings.Contains(none, "tag:ns-") {
		t.Fatalf("empty namespaces must remove every managed entry (err %v):\n%s", err, none)
	}
	m := decodeAny(t, none)
	if len(m["acls"].([]any)) != 2 {
		t.Fatalf("unmanaged rules lost: %v", m["acls"])
	}
}

func TestLeavesUnmanagedEntriesAndKeyOrderUntouched(t *testing.T) {
	out, err := Compose(basePolicy, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, k := range []string{`"groups"`, `"tagOwners"`, `"acls"`, `"ssh"`} {
		order = append(order, k)
		if i := strings.Index(out, k); i < 0 {
			t.Fatalf("key %s missing", k)
		}
	}
	last := -1
	for _, k := range order {
		i := strings.Index(out, k)
		if i < last {
			t.Fatalf("key order changed:\n%s", out)
		}
		last = i
	}
	m := decodeAny(t, out)
	if _, ok := m["ssh"]; !ok {
		t.Fatal("ssh lost")
	}
	if !strings.Contains(out, `"tag:node-control:80"`) || !strings.Contains(out, `"tag:funnel-ingress:9100"`) {
		t.Fatal("unmanaged rules lost")
	}
}

func TestPrefixCollisionPdlVsPdl2(t *testing.T) {
	both := map[string][]Range{"pdl": {{80, 80}}, "pdl2": {{81, 81}}}
	out, err := Compose(basePolicy, both, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	dst := []string{}
	for _, r := range decodeAny(t, out)["acls"].([]any) {
		for _, d := range r.(map[string]any)["dst"].([]any) {
			dst = append(dst, d.(string))
		}
	}
	got := strings.Join(dst, " ")
	if !strings.Contains(got, "tag:ns-pdl:80") || !strings.Contains(got, "tag:ns-pdl2:81") {
		t.Fatalf("dst = %s", got)
	}
	// Removing pdl must keep pdl2.
	out2, err := Compose(out, map[string][]Range{"pdl2": {{81, 81}}}, aclSettings())
	if err != nil || strings.Contains(out2, "tag:ns-pdl:") || !strings.Contains(out2, "tag:ns-pdl2:81") {
		t.Fatalf("pdl2 must survive removing pdl (err %v):\n%s", err, out2)
	}
}

func TestUnmanagedRuleWithSameSrcButOtherDstIsKept(t *testing.T) {
	mixed := strings.Replace(basePolicy, `"dst": ["tag:node-control:80"]`, `"dst": ["tag:node-control:80", "tag:ns-pdl:22"]`, 1)
	out, err := Compose(mixed, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"tag:ns-pdl:22"`) || !strings.Contains(out, `"tag:node-control:80"`) {
		t.Fatalf("a rule that is not purely namespace-bound must be kept:\n%s", out)
	}
}

func TestHandAddedManagedLookingRuleIsOverwritten(t *testing.T) {
	hand := strings.Replace(basePolicy, `"ssh": []`, `"ssh": []`, 1)
	hand = strings.Replace(hand, `"acls": [`, `"acls": [
    {"action": "accept", "src": ["tag:funnel-ingress"], "dst": ["tag:ns-pdl:1-65535"]},`, 1)
	out, err := Compose(hand, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "1-65535") || !strings.Contains(out, "tag:ns-pdl:8080,9000-9010") {
		t.Fatalf("hand edits to managed entries must be replaced:\n%s", out)
	}
}

func TestCreatesMissingTagOwnersAndAclsKeys(t *testing.T) {
	out, err := Compose(`{"groups": {}}`, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	m := decodeAny(t, out)
	if _, ok := m["tagOwners"].(map[string]any)["tag:ns-pdl"]; !ok {
		t.Fatalf("tagOwners not created: %s", out)
	}
	if len(m["acls"].([]any)) != 1 {
		t.Fatalf("acls not created: %s", out)
	}
	if _, ok := m["groups"]; !ok {
		t.Fatal("groups lost")
	}
}

func TestHuJSONReturnsErrNotPlainJSONAndNothingElse(t *testing.T) {
	for name, text := range map[string]string{
		"comment":        "{\n// a comment\n\"acls\": []}",
		"trailing comma": `{"acls": [],}`,
		"not json":       "hello",
		"empty":          "",
	} {
		out, err := Compose(text, pdl, aclSettings())
		if !errors.Is(err, ErrNotPlainJSON) || out != "" {
			t.Errorf("%s: out=%q err=%v", name, out, err)
		}
	}
}

func TestWrongShapesAreRefused(t *testing.T) {
	for name, text := range map[string]string{
		"array root":         `[]`,
		"tagOwners an array": `{"tagOwners": []}`,
		"acls an object":     `{"acls": {}}`,
	} {
		if _, err := Compose(text, pdl, aclSettings()); !errors.Is(err, ErrPolicyShape) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestNumbersAndEscapesRoundTrip(t *testing.T) {
	in := `{"a": 1.50, "b": 12345678901234567890, "c": "x & <y> \"q\"", "d": null, "e": true, "acls": []}`
	out, err := Compose(in, pdl, aclSettings())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`1.50`, `12345678901234567890`, `"x & <y> \"q\""`, `null`, `true`} {
		if !strings.Contains(out, want) {
			t.Errorf("round trip lost %s:\n%s", want, out)
		}
	}
}

func TestEmptyNamespacesRemovesAllManaged(t *testing.T) {
	with, _ := Compose(basePolicy, map[string][]Range{"a": {{1, 1}}, "b": {{2, 2}}}, aclSettings())
	out, err := Compose(with, map[string][]Range{}, aclSettings())
	if err != nil || strings.Contains(out, "tag:ns-") {
		t.Fatalf("%v\n%s", err, out)
	}
}
