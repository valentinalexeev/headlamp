package authz

import (
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func withDefault(t *testing.T, p *Policy) {
	t.Helper()
	old := SetDefault(p)
	t.Cleanup(func() { SetDefault(old) })
}

func TestCanFallsBackToLegacyWithoutPolicy(t *testing.T) {
	withDefault(t, nil)
	legacy := ParseAllowed("alice@")
	if !Can("alice@", P(FeaturePolicy, LevelAdmin), legacy) {
		t.Fatal("without a policy, the legacy list decides")
	}
	if Can("bob@", P(FeaturePolicy, LevelView), legacy) {
		t.Fatal("a login not on the legacy list must be refused")
	}
	if !AnyoneCan(P(FeaturePolicy, LevelAdmin), legacy) || AnyoneCan(P(FeaturePolicy, LevelAdmin), ParseAllowed("")) {
		t.Fatal("AnyoneCan without a policy must follow the legacy list")
	}
}

// With a policy installed, an ADMIN_CONFIG grant counts in a module even if the login is not on the module's old list,
// and the old list alone no longer counts unless it was folded in with AddLegacy.
func TestCanUsesPolicyWhenSet(t *testing.T) {
	p, _ := NewPolicy(Config{Grants: []Grant{{Subject: "carol@", Roles: []string{"viewer"}}}})
	withDefault(t, p)
	legacy := ParseAllowed("alice@")
	if !Can("carol@", P(FeatureTasks, LevelView), legacy) {
		t.Fatal("a policy grant must count in the module")
	}
	if Can("carol@", P(FeatureTasks, LevelAdmin), legacy) {
		t.Fatal("a viewer must not be admin")
	}
	if Can("alice@", P(FeatureTasks, LevelView), legacy) {
		t.Fatal("with a policy, the module list counts only through AddLegacy")
	}
	p.AddLegacy(FeatureTasks, legacy)
	if !Can("alice@", P(FeatureTasks, LevelAdmin), legacy) {
		t.Fatal("a list folded in with AddLegacy must count")
	}
	if !AnyoneCan(P(FeatureTasks, LevelAdmin), ParseAllowed("")) || AnyoneCan(P(FeaturePolicy, LevelAdmin), ParseAllowed("")) {
		t.Fatal("AnyoneCan must follow the policy")
	}
}

func TestGate(t *testing.T) {
	p, _ := NewPolicy(Config{Grants: []Grant{{Subject: "owner@", Roles: []string{"owner"}}, {Subject: "viewer@", Roles: []string{"viewer"}}}})
	withDefault(t, p)
	tn := NewFakeTailnet(map[string]Who{
		ipOwner:  {Login: "owner@"},
		ipViewer: {Login: "viewer@"},
		ipTagged: {Login: "tagged-devices", Tags: []string{"tag:x"}},
	})
	var got string
	h := Gate(tn, P(FeaturePolicy, LevelAdmin), ParseAllowed(""), slog.New(slog.NewTextHandler(io.Discard, nil)))(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = LoginFrom(r.Context()) }))
	m := core.NewMux()
	m.Handle("POST /x", h)
	if c := call(m, "POST", "/x", ipOwner); c != 200 || got != "owner" {
		t.Fatalf("owner: %d %q", c, got)
	}
	if c := call(m, "POST", "/x", ipViewer); c != 403 {
		t.Fatalf("viewer must not pass an admin gate: %d", c)
	}
	if c := call(m, "POST", "/x", ipTagged); c != 403 {
		t.Fatalf("tagged: %d", c)
	}
}
