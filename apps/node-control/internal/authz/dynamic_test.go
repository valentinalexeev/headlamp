package authz

import (
	"strings"
	"testing"
)

// #123: the admin feature exists, only owner holds it among the built-in roles.
func TestAdminFeatureOnlyForOwner(t *testing.T) {
	if !BuiltinRoles["owner"].Grants(P(FeatureAdmin, LevelAdmin)) {
		t.Fatal("owner must hold admin.admin")
	}
	for _, r := range []string{"viewer", "operator"} {
		if BuiltinRoles[r].Grants(P(FeatureAdmin, LevelView)) {
			t.Errorf("%s must not see roles and grants", r)
		}
	}
	if BuiltinRoles["viewer"].Grants(P(FeatureNodes, LevelView)) == false {
		t.Fatal("viewer still views nodes")
	}
}

func TestDynamicLayerAddsToFileGrants(t *testing.T) {
	p, err := NewPolicy(Config{Grants: []Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetDynamic(Config{
		Roles:  map[string][]string{"netops": {"nodes.operate"}},
		Grants: []Grant{{Subject: "bob@", Roles: []string{"viewer"}}, {Subject: "group:net", Roles: []string{"netops"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if !p.Can(Subject{Login: "bob@"}, P(FeatureNodes, LevelView)) || p.Can(Subject{Login: "bob@"}, P(FeatureNodes, LevelOperate)) {
		t.Fatal("a stored viewer grant must apply as viewer")
	}
	if !p.Can(Subject{Login: "carol@", Groups: []string{"net"}}, P(FeatureNodes, LevelOperate)) {
		t.Fatal("a stored group grant must apply")
	}
	if !p.Can(Subject{Login: "owner@"}, P(FeatureAdmin, LevelAdmin)) {
		t.Fatal("the file grant must still apply")
	}
	if !AnyoneCanIn(p, P(FeatureNodes, LevelOperate)) {
		t.Fatal("AnyoneCan must see stored grants")
	}
	// Replacing the layer removes the old grants at once.
	if err := p.SetDynamic(Config{}); err != nil {
		t.Fatal(err)
	}
	if p.Can(Subject{Login: "bob@"}, P(FeatureNodes, LevelView)) {
		t.Fatal("a removed stored grant must stop at once")
	}
	if !p.Can(Subject{Login: "owner@"}, P(FeatureAdmin, LevelAdmin)) {
		t.Fatal("the break-glass owner must survive an empty store")
	}
}

func TestDynamicMakesEmptyPolicyNonEmpty(t *testing.T) {
	p, _ := NewPolicy(Config{})
	if err := p.SetDynamic(Config{Grants: []Grant{{Subject: "bob@", Roles: []string{"viewer"}}}}); err != nil {
		t.Fatal(err)
	}
	if p.Empty() {
		t.Fatal("stored grants count")
	}
}

func TestSetDynamicRejects(t *testing.T) {
	p, _ := NewPolicy(Config{Roles: map[string][]string{"filerole": {"nodes.view"}}})
	p.AddLegacy(FeatureTasks, ParseAllowed("alice@"))
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"builtin", Config{Roles: map[string][]string{"owner": {"nodes.view"}}}, "built-in"},
		{"file role", Config{Roles: map[string][]string{"filerole": {"nodes.view"}}}, "ADMIN_CONFIG"},
		{"legacy role", Config{Roles: map[string][]string{"legacy-x": {"nodes.view"}}}, "reserved"},
		{"grant legacy role", Config{Grants: []Grant{{Subject: "bob@", Roles: []string{"legacy-tasks"}}}}, "unknown role"},
		{"unknown role", Config{Grants: []Grant{{Subject: "bob@", Roles: []string{"god"}}}}, "unknown role"},
	} {
		if err := p.SetDynamic(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// A stored grant may use a role from the file.
	if err := p.SetDynamic(Config{Grants: []Grant{{Subject: "bob@", Roles: []string{"filerole"}}}}); err != nil {
		t.Fatal(err)
	}
}

func TestDescribe(t *testing.T) {
	p, _ := NewPolicy(Config{Grants: []Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
	p.AddOwners(ParseAllowed("boss@"))
	_ = p.SetDynamic(Config{Roles: map[string][]string{"r1": {"nodes.operate"}}, Grants: []Grant{{Subject: "bob@", Roles: []string{"r1"}}}})
	d := p.Describe()
	if len(d.FileGrants) != 2 || d.FileGrants[0].Subject != "boss" || d.FileGrants[1].Subject != "owner" {
		t.Fatalf("file grants: %+v", d.FileGrants)
	}
	var r1 *RoleInfo
	for i := range d.Roles {
		if d.Roles[i].Name == "r1" {
			r1 = &d.Roles[i]
		}
	}
	if r1 == nil || r1.Source != "store" || strings.Join(r1.Perms, ",") != "nodes.operate" {
		t.Fatalf("r1: %+v", r1)
	}
}

func TestCovers(t *testing.T) {
	op := BuiltinRoles["operator"]
	if !Covers(op, BuiltinRoles["viewer"]) {
		t.Fatal("operator covers viewer")
	}
	if Covers(op, BuiltinRoles["owner"]) || Covers(BuiltinRoles["viewer"], op) {
		t.Fatal("no escalation")
	}
}
