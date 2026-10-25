package controlsync

import (
	"errors"
	"reflect"
	"testing"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

type memBlob struct {
	b      []byte
	has    bool
	saves  int
	loadEr error
}

func (m *memBlob) Load() ([]byte, bool, error) { return m.b, m.has, m.loadEr }
func (m *memBlob) Save(b []byte) error {
	m.b, m.has = append([]byte(nil), b...), true
	m.saves++
	return nil
}

func sampleConfig() Config {
	return Config{
		Version: "v1", PublicDomain: "example.com",
		Namespaces: map[string][]config.PortRange{"pdl": {{Lo: 8080, Hi: 8080}, {Lo: 9000, Hi: 9010}}, "empty": {}},
		Routes: []routes.Route{
			{Key: routes.Key{NS: "pdl", Service: "docs"}, Target: "pdl-1", Port: 8080, Scheme: "https", InsecureSkipVerify: true, Kind: routes.Static},
			{Key: routes.Key{NS: "abc", Service: "x"}, Target: "10.0.0.1", Port: 80, Scheme: "http", Kind: routes.Static},
		},
		Blocked: []routes.Key{{NS: "pdl", Service: "admin"}},
	}
}

func TestCacheRoundTrip(t *testing.T) {
	b := &memBlob{}
	c := NewCache(b)
	if err := c.Save(sampleConfig()); err != nil {
		t.Fatal(err)
	}
	got, ok, err := c.Load()
	if err != nil || !ok {
		t.Fatalf("load: %v %v", ok, err)
	}
	want := sampleConfig()
	// Routes come back sorted by namespace and service.
	want.Routes[0], want.Routes[1] = want.Routes[1], want.Routes[0]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip:\n got %+v\nwant %+v", got, want)
	}
	if b.saves != 1 {
		t.Fatalf("saves = %d, want one blob write", b.saves)
	}
}

func TestCacheCorruptReturnsError(t *testing.T) {
	for name, raw := range map[string]string{"garbage": "not json", "no version": `{"public_domain":"example.com"}`} {
		b := &memBlob{b: []byte(raw), has: true}
		if _, ok, err := NewCache(b).Load(); err == nil || ok {
			t.Errorf("%s: accepted (ok=%v err=%v)", name, ok, err)
		}
		if string(b.b) != raw || b.saves != 0 {
			t.Errorf("%s: corrupt cache was modified", name)
		}
	}
}

func TestCacheAbsentIsNotAnError(t *testing.T) {
	cfg, ok, err := NewCache(&memBlob{}).Load()
	if err != nil || ok || cfg.Version != "" {
		t.Fatalf("%+v %v %v", cfg, ok, err)
	}
}

func TestCacheStoreErrorIsAnError(t *testing.T) {
	if _, _, err := NewCache(&memBlob{loadEr: errors.New("redis down: secret-detail")}).Load(); err == nil {
		t.Fatal("store failure reported as an empty cache")
	}
}
