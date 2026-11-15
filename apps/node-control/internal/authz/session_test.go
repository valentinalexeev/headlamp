package authz

import (
	"testing"
	"time"
)

// #366: a stored grant to an OIDC group applies to a login only while it has a live session with that
// group, everywhere Can is asked, and ends at the expiry or at sign-out.
func TestSessionGroupsGrant(t *testing.T) {
	p, err := NewPolicy(Config{Grants: []Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetDynamic(Config{Grants: []Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}}); err != nil {
		t.Fatal(err)
	}
	old := SetDefault(p)
	t.Cleanup(func() { SetDefault(old) })
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	groupsNow = func() time.Time { return now }
	t.Cleanup(func() { groupsNow = time.Now; SetSessionGroups("alice@", nil, time.Time{}) })

	view, start := P(FeatureNodes, LevelView), P(FeatureNodes, LevelOperate)
	if Can("alice@", view, Allowed{}) {
		t.Fatal("no session: no group grant")
	}
	SetSessionGroups("Alice@", []string{"netops", "other"}, now.Add(8*time.Hour))
	if !Can("alice@", view, Allowed{}) || Can("alice@", start, Allowed{}) {
		t.Fatal("viewer through the group: view yes, start no")
	}
	if Can("bob@", view, Allowed{}) {
		t.Fatal("another login gets nothing from alice's session")
	}

	// removing the grant takes effect at once, with no restart
	if err := p.SetDynamic(Config{}); err != nil {
		t.Fatal(err)
	}
	if Can("alice@", view, Allowed{}) {
		t.Fatal("a removed grant must stop at once")
	}
	_ = p.SetDynamic(Config{Grants: []Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}})

	now = now.Add(8*time.Hour + time.Second)
	if Can("alice@", view, Allowed{}) || SessionGroups("alice@") != nil {
		t.Fatal("the groups end with the session")
	}
	now = now.Add(-time.Hour)
	SetSessionGroups("alice@", []string{"netops"}, now.Add(time.Hour))
	SetSessionGroups("alice@", nil, time.Time{})
	if Can("alice@", view, Allowed{}) {
		t.Fatal("sign-out removes the groups")
	}
	if !Can("owner@", P(FeatureNodes, LevelAdmin), Allowed{}) {
		t.Fatal("file grants apply as before")
	}

	many := make([]string, 100)
	for i := range many {
		many[i] = "g"
	}
	SetSessionGroups("alice@", many, now.Add(time.Hour))
	if len(SessionGroups("alice@")) != maxSessionGroups {
		t.Fatal("groups are capped")
	}
}
