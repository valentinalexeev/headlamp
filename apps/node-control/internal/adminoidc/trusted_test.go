package adminoidc

import (
	"reflect"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

func TestParseDomains(t *testing.T) {
	got := ParseDomains(" Example.test, ,@corp.test ,")
	want := []string{"example.test", "corp.test"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ParseDomains = %v, want %v", got, want)
	}
	if ParseDomains("") != nil {
		t.Fatal("empty setting must give no domains")
	}
}

func TestTrustedDomain(t *testing.T) {
	trusted := []string{"example.test"}
	cases := []struct {
		email string
		want  bool
	}{
		{"alice@example.test", true},
		{"alice@EXAMPLE.Test", true},
		{"a@b@example.test", true},
		{"alice@sub.example.test", false},
		{"alice@example.test.evil", false},
		{"alice@other.test", false},
		{"alice@", false},
		{"alice", false},
	}
	for _, tc := range cases {
		if got := trustedDomain(tc.email, trusted); got != tc.want {
			t.Errorf("trustedDomain(%q) = %v, want %v", tc.email, got, tc.want)
		}
	}
}

// A trusted domain replaces only a false or missing email_verified (#447).
func TestCheckTrustedDomains(t *testing.T) {
	trusted := []string{"example.test"}
	unverified := func(email string) oidc.Identity {
		return oidc.Identity{Email: email, EmailUnverified: true}
	}
	missing := func(email string) oidc.Identity { return oidc.Identity{Email: email} }
	cases := []struct {
		name    string
		login   string
		id      oidc.Identity
		trusted []string
		want    refusal
	}{
		{"claim true", "alice@other.test",
			oidc.Identity{Email: "alice@other.test", EmailVerified: true}, trusted, refusedNone},
		{"false, trusted", "alice@example.test", unverified("alice@example.test"), trusted,
			refusedNone},
		{"false, other domain", "alice@other.test", unverified("alice@other.test"), trusted,
			refusedUnverified},
		{"missing, trusted", "alice@example.test", missing("Alice@Example.test"), trusted,
			refusedNone},
		{"missing, untrusted", "alice@other.test", missing("alice@other.test"), trusted,
			refusedUnverified},
		{"missing, setting off", "alice@example.test", missing("alice@example.test"), nil,
			refusedUnverified},
		{"subdomain", "alice@sub.example.test", missing("alice@sub.example.test"), trusted,
			refusedUnverified},
		{"trusted, other account", "alice@example.test", missing("bob@example.test"), trusted,
			refusedOtherAccount},
		{"verified other account", "alice@example.test",
			oidc.Identity{Email: "bob@example.test", EmailVerified: true}, trusted,
			refusedOtherAccount},
	}
	for _, tc := range cases {
		if got := check(tc.login, tc.id, tc.trusted); got != tc.want {
			t.Errorf("%s: check = %v, want %v", tc.name, got, tc.want)
		}
	}
}
