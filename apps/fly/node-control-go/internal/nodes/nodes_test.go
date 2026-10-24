package nodes

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"example.com/node-control/internal/headscale"
)

func mustParse(t *testing.T, s string) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(s))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return cfg
}

func ids(ts []Target) []string {
	var out []string
	for _, t := range ts {
		out = append(out, t.ID)
	}
	return out
}

func byID(ts []Target, id string) (Target, bool) {
	for _, t := range ts {
		if t.ID == id {
			return t, true
		}
	}
	return Target{}, false
}

func TestValidateRejectsBadID(t *testing.T) {
	for _, bad := range []string{
		`{"nodes":{"X!":{"provider":"fly"}}}`,
		`{"nodes":{"-x":{"provider":"fly"}}}`,
		`{"nodes":{"a/b":{"provider":"fly"}}}`,
		`{"aliases":{"Bad!":"x"}}`,
	} {
		_, err := ParseConfig([]byte(bad))
		if err == nil || !strings.Contains(err.Error(), "must match") {
			t.Errorf("%s: err = %v", bad, err)
		}
	}
}

func TestValidateRejectsUnknownProvider(t *testing.T) {
	_, err := ParseConfig([]byte(`{"nodes":{"x":{"provider":"aws"}}}`))
	if err == nil || !strings.Contains(err.Error(), "provider must be one of yandex, oci, selectel, fly") {
		t.Fatalf("err = %v", err)
	}
	for _, bad := range []string{`[]`, `{"nodes":[]}`, `{"nodes":{"x":{"provider":"fly","fly":"app"}}}`, `not json`} {
		if _, err := ParseConfig([]byte(bad)); err == nil {
			t.Errorf("%s: want error", bad)
		}
	}
}

func TestLoadConfigMissingFileIsEmpty(t *testing.T) {
	cfg, err := LoadConfig(t.TempDir() + "/absent.json")
	if err != nil || cfg == nil || len(cfg.Nodes) != 0 {
		t.Fatalf("cfg=%v err=%v", cfg, err)
	}
	p := t.TempDir() + "/bad.json"
	os.WriteFile(p, []byte("{"), 0o600)
	if _, err := LoadConfig(p); err == nil {
		t.Fatal("malformed file must error")
	}
}

func TestUIViewMatchesPythonFixture(t *testing.T) {
	cfg, err := LoadConfig("../../testdata/nodes.example.json")
	if err != nil {
		t.Fatal(err)
	}
	view := UIView(cfg, Discover(cfg, nil))
	gotB, _ := json.Marshal(view)
	var got, want any
	json.Unmarshal(gotB, &got)
	wb, err := os.ReadFile("../../testdata/nodes_view.golden.json")
	if err != nil {
		t.Fatal(err)
	}
	json.Unmarshal(wb, &want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %s\nwant %s", gotB, wb)
	}
	if strings.Contains(string(gotB), "instance_id") || strings.Contains(string(gotB), "oci-compartment") {
		t.Fatal("connection details leaked")
	}
}

func TestTailnetNames(t *testing.T) {
	cfg := mustParse(t, `{"nodes":{"a":{"provider":"yandex","tailnetName":"ta"},"b-1":{"provider":"oci"}},"tailnetTrack":["extra"]}`)
	got := TailnetNames(cfg, Discover(cfg, nil))
	want := map[string]bool{"ta": true, "b-1": true, "extra": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func hs(name string, online bool, tags ...string) headscale.Node {
	return headscale.Node{Name: name, Online: online, Tags: tags}
}

func TestDiscoverFiltersTagExit(t *testing.T) {
	ts := Discover(&Config{}, []headscale.Node{
		hs("laptop", true, "tag:user"),
		hs("node-a", true, "tag:exit", "tag:cloud-oci"),
		hs("plain", true),
	})
	if !reflect.DeepEqual(ids(ts), []string{"node-a"}) {
		t.Fatalf("got %v", ids(ts))
	}
}

func TestDiscoverProviderFromCloudTag(t *testing.T) {
	ts := Discover(&Config{}, []headscale.Node{
		hs("a", true, "tag:exit", "tag:cloud-yandex"),
		hs("b", true, "tag:exit", "tag:cloud-oci"),
		hs("c", true, "tag:exit", "tag:cloud-selectel"),
		hs("d", true, "tag:exit", "tag:cloud-fly"),
		hs("e", true, "tag:exit", "tag:cloud-unknown"),
	})
	want := map[string]string{"a": "yandex", "b": "oci", "c": "selectel", "d": "fly", "e": ""}
	for id, p := range want {
		tg, ok := byID(ts, id)
		if !ok || tg.Provider != p {
			t.Errorf("%s: %+v ok=%v want provider %q", id, tg, ok, p)
		}
	}
}

func TestStatusOnlyWithoutCloudTag(t *testing.T) {
	cfg := &Config{}
	ts := Discover(cfg, []headscale.Node{hs("relay-1", true, "tag:exit")})
	if len(ts) != 1 || ts[0].ID != "relay-1" || ts[0].Provider != "" || ts[0].Name != "relay-1" {
		t.Fatalf("got %+v", ts)
	}
	v := UIView(cfg, ts)["nodes"].(map[string]any)["relay-1"].(map[string]any)
	// index.html disables a button when the uppercased status equals the
	// button's target status; the status of a status-only node is "unmanaged",
	// so both buttons are disabled by advertising UNMANAGED for both.
	if v["startStatus"] != "UNMANAGED" || v["stopStatus"] != "UNMANAGED" {
		t.Fatalf("status-only node must disable both buttons: %v", v)
	}
	if st := (&Module{}).statusOf(context.Background(), ts[0]); !strings.EqualFold(st["status"].(string), v["startStatus"].(string)) {
		t.Fatalf("status %v does not match the advertised vocabulary %v", st, v)
	}
	if v["tailnetName"] != "relay-1" {
		t.Fatalf("view %v", v)
	}
}

func TestDiscoverDuplicateOnlineWins(t *testing.T) {
	ts := Discover(&Config{}, []headscale.Node{
		{ID: 1, Name: "x", Online: false, Tags: []string{"tag:exit", "tag:cloud-oci"}},
		{ID: 2, Name: "x-1", Online: true, Tags: []string{"tag:exit", "tag:cloud-oci"}},
		// no node named "relay": relay-1 is a real name
		{ID: 3, Name: "relay-1", Online: false, Tags: []string{"tag:exit"}},
		{ID: 4, Name: "Y", Online: true, Tags: []string{"tag:exit"}},
		{ID: 5, Name: "y-2", Online: false, Tags: []string{"tag:exit"}},
	})
	if !reflect.DeepEqual(ids(ts), []string{"relay-1", "x", "y"}) {
		t.Fatalf("got %v", ids(ts))
	}
	x, _ := byID(ts, "x")
	if x.Name != "x" || x.TailnetName != "x" {
		t.Fatalf("collapsed target must carry the canonical name: %+v", x)
	}
	y, _ := byID(ts, "y")
	if y.Name != "Y" {
		t.Fatalf("got %+v", y)
	}
}

func TestDiscoverSkipsEmptyAndInvalidNames(t *testing.T) {
	ts := Discover(&Config{}, []headscale.Node{
		hs("", true, "tag:exit"),
		hs("bad name!", true, "tag:exit"),
		hs("-lead", true, "tag:exit"),
		hs("Node-OK", true, "tag:exit"),
	})
	if !reflect.DeepEqual(ids(ts), []string{"node-ok"}) {
		t.Fatalf("got %v", ids(ts))
	}
}

func TestExplicitConfigWinsAndAliasResolves(t *testing.T) {
	cfg := mustParse(t, `{
	  "nodes": {
	    "vm-a": {"provider":"yandex","tailnetName":"node-a","yandex":{"instance_id":"i"}},
	    "ghost": {"provider":"fly","fly":{"app":"g"}},
	    "node-b": {"provider":"selectel","label":"Mine"},
	    "hid": {"hidden": true}
	  },
	  "aliases": {"old-a": "node-a"}
	}`)
	ts := Discover(cfg, []headscale.Node{
		hs("node-a", true, "tag:exit", "tag:cloud-oci"),
		hs("node-b", false, "tag:exit", "tag:cloud-oci"),
		hs("hid", true, "tag:exit"),
		hs("node-c", true, "tag:exit"),
	})
	if !reflect.DeepEqual(ids(ts), []string{"ghost", "node-b", "node-c", "vm-a"}) {
		t.Fatalf("got %v", ids(ts))
	}
	a, _ := byID(ts, "vm-a")
	if a.Provider != "yandex" || a.Name != "node-a" || a.TailnetName != "node-a" || a.Block["instance_id"] != "i" {
		t.Fatalf("explicit must win: %+v", a)
	}
	g, _ := byID(ts, "ghost")
	if g.Provider != "fly" || g.Name != "ghost" || g.TailnetName != "ghost" {
		t.Fatalf("ghost: %+v", g)
	}
	if b, _ := byID(ts, "node-b"); b.Provider != "selectel" {
		t.Fatalf("override-by-id: %+v", b)
	}
	if label := UIView(cfg, ts)["nodes"].(map[string]any)["node-b"].(map[string]any)["provider"]; label != "Mine" {
		t.Fatalf("label %v", label)
	}
	if got, ok := Find(cfg, ts, "old-a"); !ok || got.ID != "vm-a" {
		t.Fatalf("alias: %+v %v", got, ok)
	}
	if got, ok := Find(cfg, ts, "node-c"); !ok || got.ID != "node-c" {
		t.Fatalf("direct: %+v %v", got, ok)
	}
	if _, ok := Find(cfg, ts, "nope"); ok {
		t.Fatal("unknown id must not resolve")
	}
}

func TestExplicitEntryTakesOnlineCollapsedRegistration(t *testing.T) {
	cfg := mustParse(t, `{"nodes":{"x":{"provider":"oci"}}}`)
	ts := Discover(cfg, []headscale.Node{
		{ID: 1, Name: "x", Online: false, Tags: []string{"tag:exit"}},
		{ID: 2, Name: "x-1", Online: true, Tags: []string{"tag:exit"}},
	})
	if len(ts) != 1 || ts[0].ID != "x" || ts[0].Name != "x" || ts[0].TailnetName != "x" || ts[0].Provider != "oci" {
		t.Fatalf("got %+v", ts)
	}
}

func TestExplicitEntryMatchesNodeThroughAlias(t *testing.T) {
	cfg := mustParse(t, `{"nodes":{"old-a":{"provider":"yandex"}},"aliases":{"old-a":"node-a"}}`)
	ts := Discover(cfg, []headscale.Node{hs("node-a", true, "tag:exit", "tag:cloud-oci")})
	if len(ts) != 1 || ts[0].ID != "old-a" || ts[0].Name != "node-a" || ts[0].TailnetName != "node-a" || ts[0].Provider != "yandex" {
		t.Fatalf("got %+v", ts)
	}
}

// The image must not ship a placeholder nodes.json: its subtitle would show on
// the page whenever no overlay replaces it. A missing file is a valid config.
func TestNoPlaceholderNodesJSONShipped(t *testing.T) {
	if _, err := os.Stat("../../app/nodes.json"); err == nil {
		t.Fatal("app/nodes.json must not be shipped")
	}
	if _, err := os.Stat("../../app/.gitkeep"); err != nil {
		t.Fatal("app/ must stay in the tree (the Dockerfile copies it)")
	}
}
