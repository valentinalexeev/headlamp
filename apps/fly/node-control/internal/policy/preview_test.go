package policy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"example.com/node-control/internal/headscale"
)

func previewSvc(t *testing.T, current string) (*Service, *fakeAPI) {
	t.Helper()
	api := newFakeAPI(current)
	svc, _, _ := newTestService(t, api)
	return svc, api
}

func TestInvalidPolicyReportsHeadscaleMessage(t *testing.T) {
	svc, api := previewSvc(t, "{}")
	api.checkErr = &headscale.APIError{Status: 400, Message: "x"}
	p, err := svc.Preview(context.Background(), `{"acls":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Valid || p.Error != "x" {
		t.Fatalf("preview = %+v", p)
	}
}

func TestPreviewOtherCheckErrorIsReturned(t *testing.T) {
	svc, api := previewSvc(t, "{}")
	api.checkErr = errors.New("unreachable")
	if _, err := svc.Preview(context.Background(), `{}`); err == nil {
		t.Fatal("expected error")
	}
}

func TestIdenticalTextEmptyDiff(t *testing.T) {
	cur := `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`
	svc, _ := previewSvc(t, cur)
	p, err := svc.Preview(context.Background(), cur)
	if err != nil || !p.Valid || p.Diff != "" {
		t.Fatalf("preview = %+v %v", p, err)
	}
}

func TestDiffIsUnifiedWithContext(t *testing.T) {
	a := "l1\nl2\nl3\nl4\nl5\nl6\nl7\nl8\nl9\nl10\n"
	b := "l1\nl2\nl3\nl4\nX5\nl6\nl7\nl8\nl9\nl10\n"
	want := "--- current\n+++ proposed\n@@ -2,7 +2,7 @@\n l2\n l3\n l4\n-l5\n+X5\n l6\n l7\n l8\n"
	if got := unifiedDiff(a, b); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffSeparateHunksAndEnds(t *testing.T) {
	a := "a\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nl\n"
	b := "A\nb\nc\nd\ne\nf\ng\nh\ni\nj\nk\nL\n"
	want := "--- current\n+++ proposed\n@@ -1,4 +1,4 @@\n-a\n+A\n b\n c\n d\n@@ -9,4 +9,4 @@\n i\n j\n k\n-l\n+L\n"
	if got := unifiedDiff(a, b); got != want {
		t.Fatalf("diff:\n%s\nwant:\n%s", got, want)
	}
}

func TestDiffJSONIsReindented(t *testing.T) {
	d := policyDiff(`{"a":1,"b":2}`, `{"a":1,"b":3}`)
	if !strings.Contains(d, "-  \"b\": 2") || !strings.Contains(d, "+  \"b\": 3") {
		t.Fatalf("diff = %q", d)
	}
}

func TestDiffTooLargeOmitted(t *testing.T) {
	a := strings.Repeat("x\n", 2100)
	b := strings.Repeat("y\n", 2100)
	if got := policyDiff(a, b); got != "diff omitted: policies too large" {
		t.Fatalf("diff = %q", got)
	}
}

func TestWarningsEmptyAclsUnknownTagAndRemovedRules(t *testing.T) {
	cur := `{"tagOwners":{"tag:example":["alice@"]},"acls":[
	  {"action":"accept","src":["tag:example"],"dst":["*:*"]},
	  {"action":"accept","src":["alice@"],"dst":["tag:example:22"]}]}`
	after := `{"tagOwners":{"tag:example":["alice@"]},
	  "acls":[{"action":"accept","src":["tag:other","tag:example"],"dst":["tag:third:443"]}],
	  "autoApprovers":{"routes":{"10.0.0.0/8":["tag:approver"]},"exitNode":["tag:example"]}}`
	svc, _ := previewSvc(t, cur)
	p, err := svc.Preview(context.Background(), after)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"tag:approver is used but not listed in tagOwners",
		"tag:other is used but not listed in tagOwners",
		"tag:third is used but not listed in tagOwners",
		"2 rule(s) removed",
	}
	if !reflect.DeepEqual(p.Warnings, want) {
		t.Fatalf("warnings = %q", p.Warnings)
	}
	p, _ = svc.Preview(context.Background(), `{"tagOwners":{}}`)
	if p.Warnings[0] != "acls is empty or missing" {
		t.Fatalf("warnings = %q", p.Warnings)
	}
}

func TestNoWarningsForNonJSON(t *testing.T) {
	svc, _ := previewSvc(t, `{"acls":[{"action":"accept"}]}`)
	for _, in := range []string{"// comment\n{}", "[1,2]", "not json"} {
		p, err := svc.Preview(context.Background(), in)
		if err != nil || len(p.Warnings) != 0 {
			t.Fatalf("%q: preview = %+v %v", in, p, err)
		}
	}
}
