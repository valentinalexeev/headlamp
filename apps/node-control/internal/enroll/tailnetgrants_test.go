package enroll

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

var gTargets = []TailnetTarget{{Name: "nas", Dst: []string{"tag:nas:445,22"}}, {Name: "printer", Dst: []string{"100.64.0.20:631"}}}

func dev(id int64, owner string) Device { return Device{ID: id, Owner: owner} }

func grant(kind, subj, target string) TailnetGrant {
	return TailnetGrant{Bridge: "b", Kind: kind, Subject: subj, Target: target}
}

func TestGroupIDStableAndOrderFree(t *testing.T) {
	a, b := GroupID([]string{"nas", "printer"}), GroupID([]string{"printer", "nas", "nas"})
	if a != b || !groupIDRe.MatchString(a) || GroupID([]string{"nas"}) == a {
		t.Fatalf("%s %s", a, b)
	}
	if tag := GroupTag(a); !strings.HasPrefix(tag, GroupTagPrefix) || len(tag) != len(GroupTagPrefix)+8 {
		t.Errorf("tag %s", tag)
	}
	if strings.Contains(GroupTag(a), "nas") {
		t.Error("the tag must not carry target names")
	}
}

func TestResolveGrantsSubjects(t *testing.T) {
	doc := TailnetDoc{Grants: []TailnetGrant{
		grant(SubjectDevice, "1", "nas"), grant(SubjectDevice, "2", "printer"),
		grant(SubjectOwner, "carol", "nas"), grant(SubjectOwner, "carol", "printer"),
		grant(SubjectGroup, "group:ops", "nas"), grant(SubjectDevice, "4", "gone"),
		{Bridge: "other", Kind: SubjectDevice, Subject: "4", Target: "nas"},
	}}
	devs := []Device{dev(1, "alice"), dev(2, "bob"), dev(3, "carol"), dev(4, "dave"), dev(5, "carol")}
	r := ResolveGrants(doc, gTargets, "b", devs)
	if len(r.Groups) != 3 {
		t.Fatalf("groups %+v", r.Groups)
	}
	if r.ByDevice[1] == r.ByDevice[2] || r.ByDevice[3] != r.ByDevice[5] || r.ByDevice[3] == "" {
		t.Errorf("by device %v", r.ByDevice)
	}
	if _, ok := r.ByDevice[4]; ok {
		t.Error("device 4: a grant on an unknown target or another bridge gave access")
	}
	both := r.Groups[2]
	if strings.Join(both.Targets, ",") != "nas,printer" || strings.Join(both.Dst, ",") != "100.64.0.20:631,tag:nas:445,22" {
		t.Errorf("owner group %+v", both)
	}
	if len(r.Unresolved) != 1 || r.Unresolved[0] != "group:ops" {
		t.Errorf("unresolved %v", r.Unresolved)
	}
}

func TestResolveGrantsLimit(t *testing.T) {
	var ts []TailnetTarget
	var doc TailnetDoc
	var devs []Device
	for i := 1; i <= MaxGroupsPerBridge+2; i++ {
		n := fmt.Sprintf("t%d", i)
		ts = append(ts, TailnetTarget{Name: n, Dst: []string{"tag:x:1"}})
		doc.Grants = append(doc.Grants, grant(SubjectDevice, fmt.Sprint(i), n))
		devs = append(devs, dev(int64(i), ""))
	}
	r := ResolveGrants(doc, ts, "b", devs)
	if len(r.Groups) != MaxGroupsPerBridge+2 || r.Error == "" {
		t.Fatalf("%d %q", len(r.Groups), r.Error)
	}
	for i, g := range r.Groups {
		if over := i >= MaxGroupsPerBridge; g.OverLimit != over {
			t.Errorf("group %d over=%v", i, g.OverLimit)
		}
	}
	if _, ok := r.ByDevice[int64(MaxGroupsPerBridge+1)]; ok {
		t.Error("an over-limit group has a route")
	}
	if len(WantedGroupRules([]BridgeGrants{r})) != MaxGroupsPerBridge {
		t.Error("over-limit groups in the ACL")
	}
}

func TestParseConfigTailnetTargets(t *testing.T) {
	base := `{"link_host":"l.example.com","ingresses":[{"name":"b","host":"b.example.com","public_key":"P","short_id":"aa","uuids":"base","tailnet_access":"grants"}]`
	cfg, err := ParseIngressConfig([]byte(base + `,"tailnet_targets":[{"name":"nas","dst":["tag:nas:445,22"]},{"name":"printer","dst":["100.64.0.20:631"]}]}`))
	if err != nil || len(cfg.Targets) != 2 || cfg.Targets[0].Source != "config" || cfg.Ingresses[0].TailnetAccess != TailnetGrants {
		t.Fatalf("%+v %v", cfg, err)
	}
	for _, bad := range []string{
		`"tailnet_targets":{}`, `"tailnet_targets":[{"name":"Nas","dst":["tag:a:1"]}]`,
		`"tailnet_targets":[{"name":"a","dst":[]}]`, `"tailnet_targets":[{"name":"a","dst":["tag:a"]}]`,
		`"tailnet_targets":[{"name":"a","dst":["*:*"]}]`,
		`"tailnet_targets":[{"name":"a","dst":["tag:a:1"]},{"name":"a","dst":["tag:b:1"]}]`,
	} {
		if _, err := ParseIngressConfig([]byte(base + "," + bad + "}")); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := ParseIngressConfig([]byte(strings.Replace(base, "grants", "some", 1) + "}")); err == nil {
		t.Error("bad tailnet_access accepted")
	}
}

func TestSyncPushesGroupIDsInGrantsMode(t *testing.T) {
	e := newEnv(t)
	d1, _ := e.make("a", 0)
	e.make("b", 0)
	cfg := IngressConfig{Targets: gTargets, Ingresses: []Ingress{{Name: "b", UUIDs: uuidsBase, TailnetAccess: TailnetGrants}}}.WithKey(multiKey)
	if err := e.store.PutTailnetDoc(TailnetDoc{Grants: []TailnetGrant{grant(SubjectDevice, fmt.Sprint(d1.ID), "nas")}}); err != nil {
		t.Fatal(err)
	}
	a := &fakeAgent{}
	if !SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, a)) {
		t.Fatal("sync")
	}
	want := GroupID([]string{"nas"})
	n := 0
	for _, c := range a.pushes[0].creds {
		if c.Tailnet == want {
			n++
		} else if c.Tailnet != "" {
			t.Errorf("unexpected grant %q", c.Tailnet)
		}
	}
	if n != len(d1.UUIDs) {
		t.Errorf("granted %d, want %d", n, len(d1.UUIDs))
	}
	// the console overrides the mode: off removes every grant, deny by default
	_ = e.store.PutTailnetDoc(TailnetDoc{Modes: map[string]string{"b": TailnetOff},
		Grants: []TailnetGrant{grant(SubjectDevice, fmt.Sprint(d1.ID), "nas")}})
	SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, a))
	for _, c := range a.pushes[len(a.pushes)-1].creds {
		if c.Tailnet != "" {
			t.Fatal("mode off still grants")
		}
	}
}

func TestAgentReportsGroupsIntoState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"count":1,"tailnet_groups":["g-0a1b2c3d","bad"]}`))
	}))
	defer srv.Close()
	e := newEnv(t)
	e.make("a", 0)
	cfg := IngressConfig{Ingresses: []Ingress{{Name: "b", UUIDs: uuidsBase}}}.WithKey(multiKey)
	ts := targetsFor(cfg, NewAgent(srv.URL, "t", nil))
	SyncAll(context.Background(), e.store, cfg, ts)
	if g, ok := ts[0].State.Groups(); !ok || len(g) != 1 || g[0] != "g-0a1b2c3d" {
		t.Errorf("%v %v", g, ok)
	}
}
