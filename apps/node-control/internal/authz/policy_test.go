package authz

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuiltinRoles(t *testing.T) {
	for _, tc := range []struct {
		role string
		perm Perm
		want bool
	}{
		{"viewer", P(FeatureNodes, LevelView), true},
		{"viewer", P(FeatureNodes, LevelOperate), false},
		{"viewer", P(FeaturePolicy, LevelAdmin), false},
		{"operator", P(FeatureNodes, LevelOperate), true},
		{"operator", P(FeatureTasks, LevelOperate), true},
		{"operator", P(FeatureEnroll, LevelOperate), true},
		{"operator", P(FeatureNodes, LevelAdmin), false},
		{"operator", P(FeaturePolicy, LevelOperate), false},
		{"operator", P(FeatureFunnel, LevelView), true},
		{"owner", P(FeaturePolicy, LevelAdmin), true},
		{"owner", P(FeatureAudit, LevelView), true},
	} {
		if got := BuiltinRoles[tc.role].Grants(tc.perm); got != tc.want {
			t.Errorf("%s grants %s = %v, want %v", tc.role, tc.perm, got, tc.want)
		}
	}
}

func TestPolicyGrantsByLoginAndGroup(t *testing.T) {
	p, err := NewPolicy(Config{
		Roles: map[string][]string{"netops": {"nodes.operate", "funnel.admin"}},
		Grants: []Grant{
			{Subject: "Alice@", Roles: []string{"owner"}},
			{Subject: "bob@example.com", Roles: []string{"viewer"}},
			{Subject: "group:net", Roles: []string{"netops"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	alice := Subject{Login: "alice@"}
	bob := Subject{Login: "BOB@example.com"}
	bobInNet := Subject{Login: "bob@example.com", Groups: []string{"net"}}
	nobody := Subject{Login: "mallory@"}

	check := func(name string, s Subject, perm Perm, want bool) {
		t.Helper()
		if got := p.Can(s, perm); got != want {
			t.Errorf("%s can %s = %v, want %v", name, perm, got, want)
		}
	}
	check("alice", alice, P(FeaturePolicy, LevelAdmin), true)
	check("bob", bob, P(FeatureNodes, LevelView), true)
	check("bob", bob, P(FeatureNodes, LevelOperate), false)
	// Roles from the login and from a group add up.
	check("bob+net", bobInNet, P(FeatureNodes, LevelOperate), true)
	check("bob+net", bobInNet, P(FeatureFunnel, LevelAdmin), true)
	check("bob+net", bobInNet, P(FeaturePolicy, LevelView), true) // from viewer
	check("bob+net", bobInNet, P(FeaturePolicy, LevelAdmin), false)
	check("nobody", nobody, P(FeatureOverview, LevelView), false)
	check("empty", Subject{}, P(FeatureOverview, LevelView), false)
	// A group name is not a login: a user called "group:net" gets nothing from the group grant.
	check("login named like a group", Subject{Login: "net"}, P(FeatureNodes, LevelOperate), false)
}

func TestNilAndEmptyPolicyGrantNothing(t *testing.T) {
	var p *Policy
	if p.Can(Subject{Login: "alice@"}, P(FeatureOverview, LevelView)) || !p.Empty() {
		t.Fatal("a nil policy must grant nothing and be empty")
	}
	e, _ := NewPolicy(Config{})
	if e.Can(Subject{Login: "alice@"}, P(FeatureOverview, LevelView)) || !e.Empty() {
		t.Fatal("an empty policy must grant nothing and be empty")
	}
}

func TestNewPolicyRejects(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{"redefine builtin", Config{Roles: map[string][]string{"owner": {"nodes.view"}}}, "built-in"},
		{"unknown feature", Config{Roles: map[string][]string{"x": {"rockets.view"}}}, "unknown feature"},
		{"unknown level", Config{Roles: map[string][]string{"x": {"nodes.root"}}}, "unknown level"},
		{"no dot", Config{Roles: map[string][]string{"x": {"nodes"}}}, "feature.level"},
		{"empty role", Config{Roles: map[string][]string{"x": {}}}, "no permissions"},
		{"bad role name", Config{Roles: map[string][]string{"a b": {"nodes.view"}}}, "invalid role name"},
		{"unknown role", Config{Grants: []Grant{{Subject: "alice@", Roles: []string{"god"}}}}, `unknown role "god"`},
		{"empty subject", Config{Grants: []Grant{{Subject: " ", Roles: []string{"viewer"}}}}, "empty subject"},
		{"no roles", Config{Grants: []Grant{{Subject: "alice@"}}}, "no roles"},
	} {
		if _, err := NewPolicy(tc.cfg); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

func TestAddLegacyMapsListToFeatureAdmin(t *testing.T) {
	p, _ := NewPolicy(Config{})
	p.AddLegacy(FeaturePolicy, ParseAllowed("alice@, Bob@"))
	if !p.Can(Subject{Login: "bob@"}, P(FeaturePolicy, LevelAdmin)) {
		t.Fatal("a legacy list member must be admin of that feature")
	}
	if p.Can(Subject{Login: "alice@"}, P(FeatureNodes, LevelView)) {
		t.Fatal("a legacy list must not grant other features")
	}
	if p.Empty() {
		t.Fatal("a policy with legacy grants is not empty")
	}
}

func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "admin.json")
	os.WriteFile(good, []byte(`{"roles":{"ops":["*.view","nodes.operate"]},"grants":[{"subject":"alice@","roles":["ops"]}]}`), 0o600)
	p, err := LoadPolicy(good)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Can(Subject{Login: "alice@"}, P(FeatureNodes, LevelOperate)) || p.Can(Subject{Login: "alice@"}, P(FeatureNodes, LevelAdmin)) {
		t.Fatal("ops role not applied as written")
	}
	typo := filepath.Join(dir, "typo.json")
	os.WriteFile(typo, []byte(`{"grant":[{"subject":"alice@","roles":["owner"]}]}`), 0o600)
	if _, err := LoadPolicy(typo); err == nil || !strings.Contains(err.Error(), "ADMIN_CONFIG") {
		t.Fatalf("a misspelt field must be refused, not ignored: %v", err)
	}
	if _, err := LoadPolicy(filepath.Join(dir, "missing.json")); err == nil {
		t.Fatal("a missing file must be an error")
	}
}
