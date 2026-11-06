package serve

import (
	"errors"
	"net/netip"
	"testing"
)

func pol() Policy {
	return Policy{
		BaseDomain: "vpn.example.com",
		Suffixes:   map[string][]string{"cluster": {"cluster.vpn.example.com"}, "lab": {"lab.vpn.example.com", "v2.apps.vpn.example.com"}, "stray": {"other.example.org"}},
		NodeNames:  func(n string) bool { return n == "web.cluster.vpn.example.com" },
	}
}

func TestCheckAcceptsNamesInTheNamespaceZone(t *testing.T) {
	for _, tc := range []struct{ ns, name, want string }{
		{"cluster", "grafana.cluster.vpn.example.com", "grafana.cluster.vpn.example.com"},
		{"cluster", "Grafana.Cluster.VPN.example.com.", "grafana.cluster.vpn.example.com"},
		{"cluster", "api.v1.cluster.vpn.example.com", "api.v1.cluster.vpn.example.com"}, // any sub-domain depth
		{"lab", "x.v2.apps.vpn.example.com", "x.v2.apps.vpn.example.com"},
	} {
		got, err := pol().Check(tc.ns, tc.name)
		if err != nil || got != tc.want {
			t.Errorf("Check(%s, %s) = %q, %v; want %q", tc.ns, tc.name, got, err, tc.want)
		}
	}
}

func TestCheckRefuses(t *testing.T) {
	for _, tc := range []struct{ ns, name, why string }{
		{"cluster", "cluster.vpn.example.com", "the suffix itself is not claimable"},
		{"cluster", "grafana.lab.vpn.example.com", "another namespace's zone"},
		{"cluster", "grafana.cluster.example.com", "not under base_domain"},
		{"cluster", "evilcluster.vpn.example.com", "a label that only ends like the suffix"},
		{"cluster", "x.notcluster.vpn.example.com", "a label that only ends like the suffix"},
		{"nobody", "a.cluster.vpn.example.com", "a namespace with no suffix"},
		{"cluster", "web.cluster.vpn.example.com", "a node name"},
		{"cluster", "a_b.cluster.vpn.example.com", "underscore"},
		{"cluster", "-a.cluster.vpn.example.com", "leading hyphen"},
		{"cluster", "a..cluster.vpn.example.com", "empty label"},
		{"cluster", "a b.cluster.vpn.example.com", "space"},
		{"cluster", "*.cluster.vpn.example.com", "wildcard"},
		{"cluster", "", "empty"},
		{"stray", "a.other.example.org", "a stored zone outside the base domain"},
	} {
		if _, err := pol().Check(tc.ns, tc.name); !errors.Is(err, ErrName) {
			t.Errorf("Check(%s, %q) accepted (%s): %v", tc.ns, tc.name, tc.why, err)
		}
	}
	long := ""
	for len(long) < 260 {
		long += "abcdefgh."
	}
	if _, err := pol().Check("cluster", long+"cluster.vpn.example.com"); !errors.Is(err, ErrName) {
		t.Error("an over-long name must be refused")
	}
	if _, err := (Policy{Suffixes: pol().Suffixes}).Check("cluster", "a.cluster.vpn.example.com"); !errors.Is(err, ErrName) {
		t.Error("no base domain must refuse everything")
	}
}

func TestCheckSuffix(t *testing.T) {
	if s, err := CheckSuffix("Cluster.", "vpn.example.com"); err != nil || s != "cluster.vpn.example.com" {
		t.Fatalf("%q %v", s, err)
	}
	if _, err := CheckSuffix("cluster", ""); !errors.Is(err, ErrName) {
		t.Error("no base domain accepted")
	}
	for _, bad := range []string{"", "cluster.vpn.example.com", "vpn.example.com", "a_b", "-x"} {
		if _, err := CheckSuffix(bad, "vpn.example.com"); !errors.Is(err, ErrName) {
			t.Errorf("CheckSuffix(%q) accepted", bad)
		}
	}
}

func TestRecordForOnlyTailnetAddresses(t *testing.T) {
	if r, err := RecordFor("a.b", netip.MustParseAddr("100.64.1.2")); err != nil || r.Type != "A" {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err := RecordFor("a.b", netip.MustParseAddr("fd7a:115c:a1e0::5")); err != nil || r.Type != "AAAA" {
		t.Fatalf("%+v %v", r, err)
	}
	for _, ip := range []string{"192.168.1.1", "8.8.8.8", "100.128.0.1", "::1", "2001:db8::1"} {
		if _, err := RecordFor("a.b", netip.MustParseAddr(ip)); !errors.Is(err, ErrName) {
			t.Errorf("RecordFor(%s) accepted a non-tailnet address", ip)
		}
	}
}
