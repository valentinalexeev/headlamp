package enroll

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixed fake data of the bridge selection cases (#313). Nothing here is a real host or key. The
// groups have neutral names.
func groupedCfg() IngressConfig {
	tol, cfg := 120, multiCfg
	cfg.Ingresses = []Ingress{
		{Name: "ingress-1", Host: "ingress-1.example.com", PublicKey: "KEY-ONE-fake", ShortID: "0a1b",
			SNI: "sni-1.example.com", Port: 443, UUIDs: "base", Group: "group-a"},
		{Name: "ingress-2", Host: "ingress-2.example.com", PublicKey: "KEY-TWO-fake", ShortID: "2c3d",
			SNI: "sni-2.example.com", Port: 8443, UUIDs: "derived", Group: "group-b"},
		{Name: "ingress-3", Host: "ingress-3.example.com", PublicKey: "KEY-THREE-fake", ShortID: "4e5f",
			SNI: "sni-3.example.com", Port: 443, UUIDs: "derived", Group: "group-a"},
		{Name: "ingress-4", Host: "ingress-4.example.com", PublicKey: "KEY-FOUR-fake", ShortID: "6a7b",
			SNI: "sni-4.example.com", Port: 443, UUIDs: "derived"},
	}
	cfg.Selection = Selection{Tolerance: &tol, Interval: 90 * time.Second, URL: "https://probe.example.com/204"}
	return cfg
}

func tunedCfg() IngressConfig {
	tol, cfg := 0, multiCfg
	cfg.Selection = Selection{Tolerance: &tol, Interval: 10 * time.Minute, URL: "https://probe.example.com/204",
		Strategy: strategyConsistentHash}
	return cfg
}

// selectionGoldens are the new shapes. Set UPDATE_GOLDEN=1 to write them.
var selectionGoldens = []struct {
	file   string
	render func() ([]byte, error)
}{
	{"profile_tuned_singbox.golden", func() ([]byte, error) { return RenderProfile(tunedCfg(), multiDevice()) }},
	{"profile_tuned_clash.golden", func() ([]byte, error) { return RenderClashProfile(tunedCfg(), multiDevice()) }},
	{"profile_grouped_singbox.golden", func() ([]byte, error) { return RenderProfile(groupedCfg(), multiDevice()) }},
	{"profile_grouped_clash.golden", func() ([]byte, error) {
		return RenderClashProfile(groupedCfg(), multiDevice())
	}},
}

func TestSelectionGoldens(t *testing.T) {
	for _, g := range selectionGoldens {
		t.Run(g.file, func(t *testing.T) {
			got, err := g.render()
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join("..", "..", "testdata", g.file)
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(path, got, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if want := golden(t, g.file); !bytes.Equal(got, want) {
				t.Fatalf("mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

// The tuned values reach both formats; the defaults stay elsewhere.
func TestSelectionValuesInProfiles(t *testing.T) {
	sb, _ := RenderProfile(tunedCfg(), multiDevice())
	cl, _ := RenderClashProfile(tunedCfg(), multiDevice())
	for _, want := range []string{`"interval":"10m"`, `"tolerance":0`, `"url":"https://probe.example.com/204"`} {
		if !strings.Contains(string(sb), want) {
			t.Errorf("sing-box lacks %s", want)
		}
	}
	// sing-box has no strategy: it stays urltest.
	if strings.Contains(string(sb), "consistent-hashing") || strings.Contains(string(sb), "load-balance") {
		t.Error("sing-box got a Clash strategy")
	}
	for _, want := range []string{`"interval": 600`, `"type": "load-balance"`, `"strategy": "consistent-hashing"`} {
		if !strings.Contains(string(cl), want) {
			t.Errorf("clash lacks %s", want)
		}
	}
}

// With groups, each mode has a top-level group over its groups, and each group lists only its
// bridges. A bridge without a group is in "default".
func TestGroupedShape(t *testing.T) {
	m := decodeObj(t, mustRender(t, RenderProfile, groupedCfg()))
	byTag := map[string]map[string]any{}
	for _, o := range m["outbounds"].([]any) {
		ob := o.(map[string]any)
		byTag[ob["tag"].(string)] = ob
	}
	want := map[string][]string{
		"auto":         {"auto@group-a", "auto@group-b", "auto@default"},
		"auto@group-a": {"auto-ingress-1", "auto-ingress-3"},
		"auto@group-b": {"auto-ingress-2"},
		"auto@default": {"auto-ingress-4"},
	}
	for tag, members := range want {
		ob, ok := byTag[tag]
		if !ok || ob["type"] != "urltest" {
			t.Fatalf("%s: %v", tag, ob)
		}
		got := ob["outbounds"].([]any)
		if len(got) != len(members) {
			t.Fatalf("%s: %v, want %v", tag, got, members)
		}
		for i := range got {
			if got[i] != members[i] {
				t.Fatalf("%s: %v, want %v", tag, got, members)
			}
		}
	}
	// Every member names an outbound that exists.
	for tag, ob := range byTag {
		if ms, ok := ob["outbounds"].([]any); ok {
			for _, x := range ms {
				if _, ok := byTag[x.(string)]; !ok {
					t.Errorf("%s names %v, which is not an outbound", tag, x)
				}
			}
		}
	}
}

func mustRender(t *testing.T, f func(IngressConfig, Device) ([]byte, error), cfg IngressConfig) []byte {
	t.Helper()
	b, err := f(cfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// A Hysteria 2 outbound stays in the group of its bridge. No golden: a Hysteria 2 password in a
// golden file trips the secret scan, as for the other Hysteria 2 tests.
func TestGroupedKeepsHy2InItsGroup(t *testing.T) {
	cfg := groupedCfg()
	cfg.Ingresses[2].Hy2Port = 8443
	m := decodeObj(t, mustRender(t, RenderProfile, cfg))
	for _, o := range m["outbounds"].([]any) {
		ob := o.(map[string]any)
		if ob["tag"] != "auto@group-a" {
			continue
		}
		got := ob["outbounds"].([]any)
		if len(got) != 3 || got[2] != "auto-ingress-3-hy2" {
			t.Fatalf("auto@group-a: %v", got)
		}
		return
	}
	t.Fatal("no auto@group-a")
}

// Phase 1 is opt-in: a config with groups or urltest options must say so; one without them gives
// the same profile as before (the existing goldens).
func TestParseSelectionOptions(t *testing.T) {
	base := `{"link_host":"links.example.com","ingresses":[` +
		`{"name":"ingress-1","host":"ingress-1.example.com","public_key":"k","short_id":"0a","uuids":"base"%s},` +
		`{"name":"ingress-2","host":"ingress-2.example.com","public_key":"k","short_id":"0b","uuids":"derived"}]%s}`
	ok := []struct{ ingress, top string }{
		{`,"group":"group-a"`, `,"urltest":{"tolerance":0,"interval":"30s","url":"https://probe.example.com/x"}`},
		{``, `,"urltest":{"interval":"24h","strategy":"round-robin"}`},
		{``, `,"urltest":{"tolerance":1000}`},
	}
	for _, c := range ok {
		cfg, err := ParseIngressConfig([]byte(strings.Replace(strings.Replace(base, "%s", c.ingress, 1), "%s", c.top, 1)))
		if err != nil {
			t.Errorf("%s %s: %v", c.ingress, c.top, err)
			continue
		}
		if c.ingress != "" && cfg.Ingresses[0].Group != "group-a" {
			t.Error("group not read")
		}
	}
	bad := map[string]struct{ ingress, top string }{
		"urltest.tolerance":       {``, `,"urltest":{"tolerance":1001}`},
		"urltest.tolerance ":      {``, `,"urltest":{"tolerance":-1}`},
		"urltest.interval":        {``, `,"urltest":{"interval":"10s"}`},
		"urltest.interval ":       {``, `,"urltest":{"interval":"25h"}`},
		"urltest.interval  ":      {``, `,"urltest":{"interval":"90.5s"}`},
		"urltest.url":             {``, `,"urltest":{"url":"ftp://x.example.com"}`},
		"urltest.url ":            {``, `,"urltest":{"url":"https://user:pw@x.example.com"}`},
		"urltest.strategy":        {``, `,"urltest":{"strategy":"random"}`},
		"urltest.url  ":           {``, `,"urltest":{"url":"https://[::1"}`},
		"unknown key \"timeout\"": {``, `,"urltest":{"timeout":5}`},
		"urltest must be":         {``, `,"urltest":[]`},
		"group must match":        {`,"group":"Group A"`, ``},
	}
	for want, c := range bad {
		_, err := ParseIngressConfig([]byte(strings.Replace(strings.Replace(base, "%s", c.ingress, 1), "%s", c.top, 1)))
		if err == nil || !strings.Contains(err.Error(), strings.TrimSpace(want)) {
			t.Errorf("%s %s: got %v, want an error about %s", c.ingress, c.top, err, want)
		}
	}
}

// sing-box itself accepts the new shapes (when the binary exists, as for the multi golden).
func TestSelectionGoldensPassSingBoxCheck(t *testing.T) {
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box is not on PATH")
	}
	for _, f := range []string{"profile_tuned_singbox.golden", "profile_grouped_singbox.golden"} {
		p := filepath.Join("..", "..", "testdata", f)
		if out, err := exec.Command(bin, "check", "-c", p).CombinedOutput(); err != nil {
			t.Errorf("%s: sing-box check: %v\n%s", f, err, out)
		}
	}
}
