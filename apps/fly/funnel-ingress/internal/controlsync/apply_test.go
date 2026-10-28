package controlsync

import (
	"sync/atomic"
	"testing"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

type nopPersister struct{}

func (nopPersister) Load() ([]byte, bool, error) { return nil, false, nil }
func (nopPersister) Save([]byte) error           { return nil }

func newApplier(t *testing.T) *Applier {
	t.Helper()
	table, err := routes.NewTable(nopPersister{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	return &Applier{Table: table, NS: auth.NewNamespaces(), Domain: new(atomic.Pointer[string]), ProxyPort: 8080, APIPort: 9100}
}

func goodConfig() Config {
	return Config{
		Version: "v1", PublicDomain: "Example.COM.",
		Namespaces: map[string][]config.PortRange{"pdl": {{Lo: 9000, Hi: 9010}}},
		Routes:     []routes.Route{{Key: routes.Key{NS: "pdl", Service: "docs"}, Target: "pdl-1", Port: 8081}},
		Blocked:    []routes.Key{{NS: "pdl", Service: "admin"}},
	}
}

func TestApplySetsNamespacesStaticBlockedAndDomain(t *testing.T) {
	a := newApplier(t)
	if _, err := a.Apply(goodConfig()); err != nil {
		t.Fatal(err)
	}
	if d := a.Domain.Load(); d == nil || *d != "example.com" {
		t.Fatalf("domain = %v", d)
	}
	if !a.NS.Allowed("pdl", 9005) || a.NS.Allowed("pdl", 8999) {
		t.Fatal("namespaces not installed")
	}
	if r, ok := a.Table.Lookup(routes.Key{NS: "pdl", Service: "docs"}); !ok || r.Kind != routes.Static || r.Scheme != "http" {
		t.Fatalf("static route: %+v %v", r, ok)
	}
	if bl := a.Table.Blocked(); len(bl) != 1 || bl[0].Service != "admin" {
		t.Fatalf("blocked: %v", bl)
	}
}

func TestApplyInvalidConfigChangesNothing(t *testing.T) {
	mutate := map[string]func(c *Config){
		"inverted range":        func(c *Config) { c.Namespaces["pdl"] = []config.PortRange{{Lo: 10, Hi: 5}} },
		"zero port range":       func(c *Config) { c.Namespaces["pdl"] = []config.PortRange{{Lo: 0, Hi: 5}} },
		"range with proxy port": func(c *Config) { c.Namespaces["pdl"] = []config.PortRange{{Lo: 8000, Hi: 8100}} },
		"range with api port":   func(c *Config) { c.Namespaces["pdl"] = []config.PortRange{{Lo: 9100, Hi: 9100}} },
		"bad namespace name":    func(c *Config) { c.Namespaces["PDL"] = nil },
		"bad route service":     func(c *Config) { c.Routes[0].Service = "Bad.Name" },
		"bad route port":        func(c *Config) { c.Routes[0].Port = 0 },
		"bad route scheme":      func(c *Config) { c.Routes[0].Scheme = "ftp" },
		"empty route target":    func(c *Config) { c.Routes[0].Target = "" },
		"duplicate route":       func(c *Config) { c.Routes = append(c.Routes, c.Routes[0]) },
		"empty domain":          func(c *Config) { c.PublicDomain = "" },
		"single label domain":   func(c *Config) { c.PublicDomain = "localhost" },
		"bad domain":            func(c *Config) { c.PublicDomain = "exa mple.com" },
		"bad blocked entry":     func(c *Config) { c.Blocked = []routes.Key{{NS: "", Service: "x"}} },
	}
	for name, m := range mutate {
		t.Run(name, func(t *testing.T) {
			a := newApplier(t)
			prev := goodConfig()
			prev.Namespaces["keep"] = []config.PortRange{{Lo: 7000, Hi: 7000}}
			prev.PublicDomain = "previous.example"
			if _, err := a.Apply(prev); err != nil {
				t.Fatal(err)
			}
			bad := goodConfig()
			m(&bad)
			if _, err := a.Apply(bad); err == nil {
				t.Fatal("accepted")
			}
			if d := a.Domain.Load(); d == nil || *d != "previous.example" {
				t.Errorf("domain changed to %v", d)
			}
			if !a.NS.Allowed("keep", 7000) {
				t.Error("namespaces changed")
			}
			if len(a.Table.All()) != 1 || len(a.Table.Blocked()) != 1 {
				t.Error("routes or blocked set changed")
			}
		})
	}
}

func TestApplyBlockedReturnsRemovedDynamicKeys(t *testing.T) {
	a := newApplier(t)
	put := func(svc string) {
		if err := a.Table.Put(routes.Route{Key: routes.Key{NS: "pdl", Service: svc}, Target: "100.64.0.5", Port: 9000, NodeID: "n1"}); err != nil {
			t.Fatal(err)
		}
	}
	put("admin")
	put("docs")
	put("keep")
	removed, err := a.Apply(goodConfig())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, k := range removed {
		got[k.Service] = true
	}
	if len(got) != 2 || !got["admin"] || !got["docs"] {
		t.Fatalf("removed = %v", removed)
	}
	if _, ok := a.Table.Lookup(routes.Key{NS: "pdl", Service: "keep"}); !ok {
		t.Fatal("unrelated dynamic route removed")
	}
}

func TestValidDomain(t *testing.T) {
	for in, want := range map[string]string{"example.com": "example.com", " Example.COM. ": "example.com", "a.b.example.org": "a.b.example.org"} {
		if got, ok := ValidDomain(in); !ok || got != want {
			t.Errorf("%q -> %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "com", ".", "a..b", "-a.com", "a_b.com", "*.example.com"} {
		if _, ok := ValidDomain(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
}
