package funnel

import (
	"reflect"
	"strings"
	"testing"

	"example.com/node-control/internal/core"
)

func goodEnv() map[string]string {
	return map[string]string{
		"FUNNEL_DB":            "/tmp/funnel.db",
		"FUNNEL_PUBLIC_DOMAIN": "example.test",
		"FUNNEL_KEY_USER":      "svc",
		"FUNNEL_ADMIN_USERS":   "alice",
		"FUNNEL_TAG_OWNERS":    "svc@",
	}
}

func with(kv ...string) map[string]string {
	m := goodEnv()
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func TestUnsetDBReturnsNil(t *testing.T) {
	s, err := SettingsFromEnv(core.MapEnv(map[string]string{"FUNNEL_PUBLIC_DOMAIN": "bad domain"}))
	if s != nil || err != nil {
		t.Fatalf("got %v, %v", s, err)
	}
}

func TestDefaults(t *testing.T) {
	s, err := SettingsFromEnv(core.MapEnv(goodEnv()))
	if err != nil {
		t.Fatal(err)
	}
	if s.FunnelTag != "tag:funnel" || s.NSPrefix != "tag:ns-" || s.IngressTag != "tag:funnel-ingress" {
		t.Fatalf("tags: %+v", s)
	}
	if s.DBPath != "/tmp/funnel.db" || s.PublicDomain != "example.test" || s.KeyUser != "svc" || s.IngressHostname != "" {
		t.Fatalf("settings: %+v", s)
	}
}

func TestRejectsMissingRequiredWhenEnabled(t *testing.T) {
	for _, k := range []string{"FUNNEL_PUBLIC_DOMAIN", "FUNNEL_KEY_USER", "FUNNEL_ADMIN_USERS", "FUNNEL_TAG_OWNERS"} {
		t.Run(k, func(t *testing.T) {
			_, err := SettingsFromEnv(core.MapEnv(with(k, "")))
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("want error naming %s, got %v", k, err)
			}
		})
	}
	// Blank-only lists count as missing.
	for _, k := range []string{"FUNNEL_ADMIN_USERS", "FUNNEL_TAG_OWNERS"} {
		if _, err := SettingsFromEnv(core.MapEnv(with(k, " , "))); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s blank: %v", k, err)
		}
	}
}

func TestTagValidation(t *testing.T) {
	cases := []struct {
		name string
		kv   []string
		want string
	}{
		{"funnel no prefix", []string{"FUNNEL_TAG", "funnel"}, "FUNNEL_TAG"},
		{"ns prefix no prefix", []string{"FUNNEL_NS_TAG_PREFIX", "ns-"}, "FUNNEL_NS_TAG_PREFIX"},
		{"ingress no prefix", []string{"FUNNEL_INGRESS_TAG", "x"}, "FUNNEL_INGRESS_TAG"},
		{"space", []string{"FUNNEL_TAG", "tag:a b"}, "FUNNEL_TAG"},
		{"empty remainder", []string{"FUNNEL_INGRESS_TAG", "tag:"}, "FUNNEL_INGRESS_TAG"},
		{"too long", []string{"FUNNEL_TAG", "tag:" + strings.Repeat("a", 61)}, "FUNNEL_TAG"},
		{"equal funnel ingress", []string{"FUNNEL_TAG", "tag:same", "FUNNEL_INGRESS_TAG", "tag:same"}, "different"},
		{"equal funnel prefix", []string{"FUNNEL_TAG", "tag:ns-"}, "different"},
		{"funnel under prefix", []string{"FUNNEL_TAG", "tag:ns-core"}, "FUNNEL_TAG"},
		{"ingress under prefix", []string{"FUNNEL_INGRESS_TAG", "tag:ns-edge"}, "FUNNEL_INGRESS_TAG"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := SettingsFromEnv(core.MapEnv(with(c.kv...)))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want error containing %q, got %v", c.want, err)
			}
		})
	}
	if _, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_TAG", "tag:example", "FUNNEL_NS_TAG_PREFIX", "tag:space-", "FUNNEL_INGRESS_TAG", "tag:edge"))); err != nil {
		t.Fatalf("valid custom tags: %v", err)
	}
}

func TestPublicDomainValidation(t *testing.T) {
	for _, bad := range []string{"Example.test", "https://example.test", "example.test:443", "example.test.", "example", "a..test", "-a.test", "a b.test", "a_b.test"} {
		_, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_PUBLIC_DOMAIN", bad)))
		if err == nil || !strings.Contains(err.Error(), "FUNNEL_PUBLIC_DOMAIN") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, bad := range []string{"Edge.example.test", "https://edge.example.test", "edge"} {
		_, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_INGRESS_HOSTNAME", bad)))
		if err == nil || !strings.Contains(err.Error(), "FUNNEL_INGRESS_HOSTNAME") {
			t.Errorf("ingress %q: %v", bad, err)
		}
	}
	s, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_INGRESS_HOSTNAME", "edge.example.test")))
	if err != nil || s.IngressHostname != "edge.example.test" {
		t.Fatalf("%v %+v", err, s)
	}
}

func TestCSVTrimmedAndNormalised(t *testing.T) {
	s, err := SettingsFromEnv(core.MapEnv(with(
		"FUNNEL_TAG_OWNERS", " svc@ , ,alice@ ,",
		"FUNNEL_KEY_USER", "  svc ",
		"FUNNEL_PUBLIC_DOMAIN", " example.test ")))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"svc@", "alice@"}; !reflect.DeepEqual(s.TagOwners, want) {
		t.Fatalf("owners %q", s.TagOwners)
	}
	if s.KeyUser != "svc" || s.PublicDomain != "example.test" {
		t.Fatalf("%+v", s)
	}
	if _, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_TAG_OWNERS", "a b"))); err == nil {
		t.Fatal("space in owner accepted")
	}
}

func TestAdminsParsedWithAuthz(t *testing.T) {
	s, err := SettingsFromEnv(core.MapEnv(with("FUNNEL_ADMIN_USERS", " Alice@ , bob@example.test")))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Admins.Has("alice") || !s.Admins.Has("BOB@example.test") || s.Admins.Has("carol") {
		t.Fatalf("admins: %+v", s.Admins)
	}
}
