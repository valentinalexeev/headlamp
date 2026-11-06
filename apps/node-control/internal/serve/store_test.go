package serve

import (
	"errors"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) Store {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "serve.db"), func() time.Time { return time.Unix(1_800_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ver(t *testing.T, s Store) int64 {
	t.Helper()
	v, err := s.Version()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var ip1, ip2 = netip.MustParseAddr("100.64.0.10"), netip.MustParseAddr("100.64.0.11")

func reg(name, node string, ip netip.Addr) Registration {
	return Registration{Name: name, Namespace: "cluster", NodeID: node, NodeName: "n-" + node, IP: ip}
}

func TestRegisterHoldsTheNameForItsNode(t *testing.T) {
	s := openTest(t)
	if err := s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Register(reg("grafana.cluster.vpn.example.com", "1", ip1)); err != nil {
		t.Fatal(err)
	}
	// Another node of the same namespace cannot take it over.
	if err := s.Register(reg("grafana.cluster.vpn.example.com", "2", ip2)); !errors.Is(err, ErrTaken) {
		t.Fatalf("take-over: %v", err)
	}
	v := ver(t, s)
	// The holder refreshing with the same address changes nothing headscale serves.
	if err := s.Register(reg("grafana.cluster.vpn.example.com", "1", ip1)); err != nil || ver(t, s) != v {
		t.Fatalf("same refresh: %v, version %d -> %d", err, v, ver(t, s))
	}
	// A new address does.
	if err := s.Register(reg("grafana.cluster.vpn.example.com", "1", ip2)); err != nil || ver(t, s) != v+1 {
		t.Fatalf("new address: %v, version %d -> %d", err, v, ver(t, s))
	}
	rs, _ := s.Registrations()
	if len(rs) != 1 || rs[0].IP != ip2 || rs[0].NodeID != "1" {
		t.Fatalf("%+v", rs)
	}
	// Only the holder unregisters it; an admin drop works for anyone.
	if err := s.Unregister("grafana.cluster.vpn.example.com", "2"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign unregister: %v", err)
	}
	if err := s.DropName("grafana.cluster.vpn.example.com"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Registrations(); len(rs) != 0 {
		t.Fatalf("not dropped: %+v", rs)
	}
}

func TestNarrowingSuffixesDropsNames(t *testing.T) {
	s := openTest(t)
	s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com", "old.vpn.example.com"}})
	s.Register(reg("a.cluster.vpn.example.com", "1", ip1))
	s.Register(reg("b.old.vpn.example.com", "1", ip1))
	if err := s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com"}}); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Registrations()
	if len(rs) != 1 || rs[0].Name != "a.cluster.vpn.example.com" {
		t.Fatalf("names outside the new suffixes must go: %+v", rs)
	}
}

func TestUnderAnyIsOnLabelBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"a.cluster.vpn.example.com", true},
		{"x.y.cluster.vpn.example.com", true},
		{"cluster.vpn.example.com", false}, // the suffix itself
		{"a.evilcluster.vpn.example.com", false},
	} {
		if got := underAny(tc.name, []string{"cluster.vpn.example.com"}); got != tc.want {
			t.Errorf("underAny(%s) = %v", tc.name, got)
		}
	}
}

func TestDeleteNamespaceRemovesItsNames(t *testing.T) {
	s := openTest(t)
	s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com"}})
	s.Register(reg("a.cluster.vpn.example.com", "1", ip1))
	v := ver(t, s)
	if err := s.DeleteNamespace("cluster"); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Registrations(); len(rs) != 0 || ver(t, s) <= v {
		t.Fatalf("names left %+v, version %d", rs, ver(t, s))
	}
	if err := s.DeleteNamespace("cluster"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete: %v", err)
	}
}

func TestRegisterRefusesIncomplete(t *testing.T) {
	s := openTest(t)
	s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com"}})
	if err := s.Register(Registration{Name: "a.cluster.vpn.example.com", Namespace: "cluster", IP: ip1}); err == nil {
		t.Fatal("no node id accepted")
	}
	if err := s.Register(Registration{Name: "a.cluster.vpn.example.com", Namespace: "cluster", NodeID: "1"}); err == nil {
		t.Fatal("no address accepted")
	}
	if err := s.PutNamespace(Namespace{Name: "Bad Name"}); err == nil {
		t.Fatal("bad namespace name accepted")
	}
}

func TestSecretsAndExpiry(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetSecret("ca-key"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	s.PutSecret("ca-key", []byte{1, 2, 3})
	s.PutSecret("ca-key", []byte{4})
	if v, err := s.GetSecret("ca-key"); err != nil || len(v) != 1 || v[0] != 4 {
		t.Fatalf("%v %v", v, err)
	}
	s.PutNamespace(Namespace{Name: "cluster", Suffixes: []string{"cluster.vpn.example.com"}})
	s.Register(reg("a.cluster.vpn.example.com", "1", ip1))
	exp := time.Unix(1_900_000_000, 0).UTC()
	if err := s.SetCertExpiry("a.cluster.vpn.example.com", exp); err != nil {
		t.Fatal(err)
	}
	if rs, _ := s.Registrations(); !rs[0].CertExpiry.Equal(exp) {
		t.Fatalf("expiry %v", rs[0].CertExpiry)
	}
	if err := s.SetCertExpiry("nope", exp); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}
