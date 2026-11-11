package enroll

import (
	"context"
	"strings"
	"testing"
)

// Activity shows the email of a self-enrolled person in place of "oidc:<subject>" (#290), as actor and in the
// detail; the log keeps the identifier. Person and invitation actions are in Activity too.
func TestActivityShowsOwnerEmails(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	_ = f.self.users.RecordLogin("sub-1", "anna@example.test", true, "")
	_ = f.self.users.RecordLogin("sub-10", "", true, "") // no email: keeps the identifier
	for _, e := range [][3]string{
		{"oidc:sub-1", "device.self_enrol", "phone"},
		{"admin@", "person.lock", "oidc:sub-1: left"},
		{"oidc:sub-10", "invite.redeem", "invitation 3, device 4"},
		{"admin@", "tailnet.grant", "owner oidc:sub-1  on b: [nas]"},
		{"admin@", "invite.create", "invitation 5 for bob@example.test, until 2027-01-01"},
	} {
		if err := f.store.Audit(e[0], e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	m := &Module{store: f.store, admin: f.admin}
	got, err := m.Activity(context.Background(), "admin@", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 5 {
		t.Fatalf("want all five, person and invitation actions included: %+v", got)
	}
	by := map[string][2]string{}
	for _, e := range got {
		by[e.Action] = [2]string{e.Actor, e.Detail}
	}
	want := map[string][2]string{
		"device.self_enrol": {"anna@example.test", "phone"},
		"person.lock":       {"admin@", "anna@example.test: left"},
		"invite.redeem":     {"oidc:sub-10", "invitation 3, device 4"},
		"tailnet.grant":     {"admin@", "owner anna@example.test  on b: [nas]"},
	}
	for a, w := range want {
		if by[a] != w {
			t.Errorf("%s: got %q, want %q", a, by[a], w)
		}
	}
	log, _ := f.store.AuditLog(10)
	for _, e := range log {
		if strings.Contains(e.Actor+e.Detail, "anna@") {
			t.Errorf("the stored log must keep the identifier: %+v", e)
		}
	}
}

// One identifier that starts another ("oidc:sub-1" in "oidc:sub-10") is not cut in two.
func TestReplaceOwnersLongestFirst(t *testing.T) {
	emails := map[string]string{"oidc:sub-1": "a@x", "oidc:sub-10": "b@x"}
	if got := replaceOwners("oidc:sub-10 and oidc:sub-1", emails); got != "b@x and a@x" {
		t.Errorf("got %q", got)
	}
	// a subject with a colon (some issuers use "provider:id"): the longest known identifier wins
	colon := map[string]string{"oidc:google:1": "g@x", "oidc:google": "short@x"}
	if got := replaceOwners("person oidc:google:1: left", colon); got != "person g@x: left" {
		t.Errorf("colon subject: got %q", got)
	}
	if got := replaceOwners("person oidc:google: left", colon); got != "person short@x: left" {
		t.Errorf("colon subject prefix: got %q", got)
	}
	if got := replaceOwners("no owner here", emails); got != "no owner here" {
		t.Errorf("got %q", got)
	}
	if got := replaceOwners("oidc:sub-1", nil); got != "oidc:sub-1" {
		t.Errorf("self-enrolment off: got %q", got)
	}
}
