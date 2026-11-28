package enroll

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

// Tailnet grants to group:<name> (#461).

// groupIDP is a Refresher that answers with fixed groups.
type groupIDP struct {
	groups []string
	known  bool
}

func (g *groupIDP) Refresh(_ context.Context, _, sub, _ string) (oidc.Identity, error) {
	return oidc.Identity{Subject: sub, Groups: g.groups, GroupsKnown: g.known}, nil
}

type groupFx struct {
	now  int64
	self *Self
	idp  *groupIDP
}

func newGroupFx(t *testing.T, cfg SelfConfig) *groupFx {
	t.Helper()
	f := &groupFx{now: adminNow, idp: &groupIDP{}}
	clock := func() time.Time { return time.Unix(f.now, 0) }
	store, err := OpenStore(filepath.Join(t.TempDir(), "e.db"), testKey, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	users, err := OpenSelfStore(filepath.Join(t.TempDir(), "oidc.db"), testKey, clock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { users.Close() })
	cfg.defaults()
	f.self = &Self{cfg: cfg, users: users, store: store, idp: f.idp, push: func(context.Context) {}}
	return f
}

func (f *groupFx) login(t *testing.T, sub string, groups ...string) {
	t.Helper()
	id := oidc.Identity{Subject: sub, Groups: groups, GroupsKnown: true, RefreshToken: "rt-" + sub}
	if err := f.self.Login(id); err != nil {
		t.Fatal(err)
	}
}

// resolve resolves one group grant (group:vpn-users -> nas) for the devices of two people.
func (f *groupFx) resolve() BridgeGrants {
	doc := TailnetDoc{Grants: []TailnetGrant{grant(SubjectGroup, "group:vpn-users", "nas")}}
	devs := []Device{dev(1, ownerOf("sub-a")), dev(2, ownerOf("sub-b")), dev(3, "local")}
	return ResolveGrants(doc, gTargets, "b", devs, f.self.OwnerGroups())
}

func TestGroupGrantMatchesTheOwnerGroups(t *testing.T) {
	f := newGroupFx(t, SelfConfig{AllowedGroups: []string{"vpn-users", "staff"}, GroupGrantAge: 24 * time.Hour})
	f.login(t, "sub-a", "vpn-users", "other")
	f.login(t, "sub-b", "staff")
	r := f.resolve()
	if r.ByDevice[1] != GroupID([]string{"nas"}) {
		t.Errorf("device of a group member: %v", r.ByDevice)
	}
	if _, ok := r.ByDevice[2]; ok {
		t.Error("a device whose owner is not in the group got the target")
	}
	if _, ok := r.ByDevice[3]; ok {
		t.Error("a device without an OIDC owner got the target")
	}
	if len(r.Unresolved) != 0 || fmt.Sprint(r.Resolved) != "[{group:vpn-users 1}]" {
		t.Errorf("unresolved %v, resolved %v", r.Unresolved, r.Resolved)
	}
	// Only the allowed groups are stored.
	if u, _ := f.self.users.User("sub-a"); !slices.Equal(u.Groups, []string{"vpn-users"}) {
		t.Errorf("stored groups %v", u.Groups)
	}
	// The next sign-in has another claim: the access is gone.
	f.login(t, "sub-a", "staff")
	r = f.resolve()
	if len(r.ByDevice) != 0 || fmt.Sprint(r.Resolved) != "[{group:vpn-users 0}]" {
		t.Errorf("after the claim changed: %v %v", r.ByDevice, r.Resolved)
	}
}

func TestGroupGrantAgeSignOutAndLock(t *testing.T) {
	f := newGroupFx(t, SelfConfig{GroupGrantAge: 2 * time.Hour})
	f.login(t, "sub-a", "vpn-users")
	f.now += int64(2 * time.Hour / time.Second)
	if r := f.resolve(); r.ByDevice[1] == "" {
		t.Fatal("groups of exactly the max age stopped counting")
	}
	f.now++
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("groups older than the max age still count")
	}

	// A re-check with groups refreshes them; one without an ID token does not.
	f.login(t, "sub-a", "vpn-users")
	f.now += int64(time.Hour/time.Second) + 1
	f.idp.groups, f.idp.known = []string{"vpn-users"}, true
	f.self.Recheck(context.Background())
	f.now += int64(time.Hour/time.Second) + 1
	if r := f.resolve(); r.ByDevice[1] == "" {
		t.Fatal("a re-check did not refresh the groups")
	}
	f.idp.known = false
	f.self.Recheck(context.Background())
	f.now += int64(time.Hour / time.Second)
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("a re-check without known groups refreshed them")
	}

	// A re-check that removes the person from the group removes the access.
	f.login(t, "sub-a", "vpn-users")
	f.now += int64(time.Hour/time.Second) + 1
	f.idp.groups, f.idp.known = []string{"staff"}, true
	f.self.Recheck(context.Background())
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("the groups of a re-check were not stored")
	}

	// Sign-out: the groups stop counting, and a re-check does not bring them back.
	f.login(t, "sub-a", "vpn-users")
	f.self.SignOut("sub-a", "")
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("groups count after the sign-out")
	}
	f.now += int64(time.Hour/time.Second) + 1
	f.idp.groups = []string{"vpn-users"}
	f.self.Recheck(context.Background())
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("a re-check after the sign-out stored the groups again")
	}

	// Lock: no groups.
	f.login(t, "sub-a", "vpn-users")
	if err := f.self.users.SetLocked("sub-a", true, "admin", "test"); err != nil {
		t.Fatal(err)
	}
	if r := f.resolve(); len(r.ByDevice) != 0 {
		t.Fatal("groups of a locked person count")
	}
}

func TestGroupGrantOffByDefault(t *testing.T) {
	f := newGroupFx(t, SelfConfig{})
	f.login(t, "sub-a", "vpn-users")
	if f.self.OwnerGroups() != nil {
		t.Fatal("owner groups without OIDC_GROUP_GRANT_HOURS")
	}
	if u, _ := f.self.users.User("sub-a"); u.GroupsAt != 0 || len(u.Groups) != 0 {
		t.Errorf("groups stored while off: %+v", u)
	}
	r := f.resolve()
	if len(r.ByDevice) != 0 || fmt.Sprint(r.Unresolved) != "[group:vpn-users]" || len(r.Resolved) != 0 {
		t.Errorf("%v %v %v", r.ByDevice, r.Unresolved, r.Resolved)
	}
}

func TestAdmittedGroups(t *testing.T) {
	f := newGroupFx(t, SelfConfig{})
	long := strings.Repeat("x", maxStoredGroupLen+1)
	got := f.self.admittedGroups([]string{"b", "a", "b", "", long})
	if !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("without allowed groups: %v", got)
	}
	var many []string
	for i := range maxStoredGroups + 5 {
		many = append(many, fmt.Sprintf("g%03d", i))
	}
	if n := len(f.self.admittedGroups(many)); n != maxStoredGroups {
		t.Errorf("stored %d groups", n)
	}
	// A grant subject matches with or without the group: prefix.
	if !inGroup("group:ops", []string{"ops"}) || !inGroup("ops", []string{"ops"}) || inGroup("group:ops", []string{"dev"}) {
		t.Error("inGroup")
	}
}

func TestGroupGrantConfigFromEnv(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "c", "OIDC_DB": "/x",
		"OIDC_REDIRECT_URL": "https://links.example/oidc/callback"}
	with := func(kv map[string]string) core.Env {
		m := maps.Clone(base)
		maps.Copy(m, kv)
		return core.MapEnv(m)
	}
	if _, sc, _, err := selfConfig(with(nil)); err != nil || sc.GroupGrantAge != 0 {
		t.Fatalf("default: %v %v", err, sc.GroupGrantAge)
	}
	if _, sc, _, err := selfConfig(with(map[string]string{"OIDC_GROUP_GRANT_HOURS": "24"})); err != nil ||
		sc.GroupGrantAge != 24*time.Hour {
		t.Fatalf("24: %v %v", err, sc.GroupGrantAge)
	}
	for _, kv := range []map[string]string{
		{"OIDC_GROUP_GRANT_HOURS": "721"},
		{"OIDC_GROUP_GRANT_HOURS": "x"},
		{"OIDC_GROUP_GRANT_HOURS": "1", "OIDC_RECHECK_MINUTES": "60"},
	} {
		if _, _, _, err := selfConfig(with(kv)); err == nil || !strings.Contains(err.Error(), "OIDC_GROUP_GRANT_HOURS") {
			t.Errorf("%v: %v", kv, err)
		}
	}
}

func TestGrantAPIGroupResolved(t *testing.T) {
	pol := &fakePolicy{text: `{"tagOwners":{"tag:ingress":["e2e@"]},"acls":[]}` + "\n"}
	cfg := adminCfg
	cfg.Ingresses = append([]Ingress(nil), cfg.Ingresses...)
	cfg.Ingresses[0].TailnetAccess = TailnetGrants
	cfg.Targets = gTargets
	cfg = cfg.WithOwnerGroups(func() map[string][]string {
		return map[string][]string{ownerOf("sub-a"): {"ops"}}
	})
	f := taclFxCfg(t, cfg, []string{"tag:lan:*"}, pol)
	d1, _, _ := f.store.CreateDevice("one", ownerOf("sub-a"), "sing-box", adminNow+3600)
	_, _, _ = f.store.CreateDevice("two", ownerOf("sub-b"), "sing-box", adminNow+3600)
	b := bridgeName(f)
	if rec := f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "group", "subject": "group:ops", "targets": []string{"nas"}}); rec.Code != 200 {
		t.Fatalf("group grant: %d", rec.Code)
	}
	v := decode(t, f.do("GET", "/enroll/tailnet-grants", "", ""))
	br := v["bridges"].([]any)[0].(map[string]any)
	if fmt.Sprint(br["unresolved"]) != "[]" || fmt.Sprint(br["resolved"]) != "[map[devices:1 subject:group:ops]]" {
		t.Fatalf("unresolved %v, resolved %v", br["unresolved"], br["resolved"])
	}
	groups := br["groups"].([]any)
	if len(groups) != 1 || fmt.Sprint(groups[0].(map[string]any)["devices"]) != fmt.Sprintf("[%d]", d1.ID) {
		t.Errorf("groups %v", groups)
	}
	last := f.agent.pushes[len(f.agent.pushes)-1].creds
	n := 0
	for _, c := range last {
		if c.Tailnet == GroupID([]string{"nas"}) {
			n++
		}
	}
	if n != len(d1.UUIDs) {
		t.Errorf("pushed %d credentials with the group, want %d", n, len(d1.UUIDs))
	}
}
