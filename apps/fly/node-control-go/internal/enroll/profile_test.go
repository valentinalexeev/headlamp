package enroll

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type expectedCase struct {
	Cfg struct {
		Host        string `json:"host"`
		PublicKey   string `json:"public_key"`
		ShortID     string `json:"short_id"`
		SNI         string `json:"sni"`
		Port        int    `json:"port"`
		ProfileName string `json:"profile_name"`
	} `json:"cfg"`
	Name      string            `json:"name"`
	Owner     string            `json:"owner"`
	UUIDs     map[string]string `json:"uuids"`
	Secret    string            `json:"secret"`
	SubURL    string            `json:"sub_url"`
	DeepLinks map[string]string `json:"deep_links"`
}

type cfgErrCase struct {
	JSON  string `json:"json"`
	Error string `json:"error"`
}

func loadProfileExpected(t *testing.T) (map[string]expectedCase, []cfgErrCase) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "profile_expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	var errs []cfgErrCase
	if err := json.Unmarshal(all["cfg_errors"], &errs); err != nil {
		t.Fatal(err)
	}
	delete(all, "cfg_errors")
	cases := map[string]expectedCase{}
	for k, v := range all {
		var c expectedCase
		if err := json.Unmarshal(v, &c); err != nil {
			t.Fatal(err)
		}
		cases[k] = c
	}
	return cases, errs
}

func (c expectedCase) ingress() IngressCfg {
	return IngressCfg{Host: c.Cfg.Host, PublicKey: c.Cfg.PublicKey, ShortID: c.Cfg.ShortID, SNI: c.Cfg.SNI,
		Port: c.Cfg.Port, ProfileName: c.Cfg.ProfileName}
}

func (c expectedCase) device() Device {
	return Device{ID: 1, Name: c.Name, Owner: c.Owner, Client: "sing-box", ExpiresAt: 2_000_000_000, UUIDs: c.UUIDs}
}

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func goldenName(kind, c string) string {
	if c == "default" {
		return "profile_" + kind + ".golden"
	}
	return "profile_" + kind + "_" + c + ".golden"
}

func TestRenderSingBoxMatchesGolden(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RenderProfile(c.ingress(), c.device())
			if err != nil {
				t.Fatal(err)
			}
			if want := golden(t, goldenName("singbox", name)); !bytes.Equal(got, want) {
				t.Fatalf("mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestRenderClashMatchesGolden(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := RenderClashProfile(c.ingress(), c.device())
			if err != nil {
				t.Fatal(err)
			}
			if want := golden(t, goldenName("clash", name)); !bytes.Equal(got, want) {
				t.Fatalf("mismatch\n got: %s\nwant: %s", got, want)
			}
		})
	}
}

func TestProfilesCarryNoNameOrOwner(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	c := cases["unusual"]
	sb, err := RenderProfile(c.ingress(), c.device())
	if err != nil {
		t.Fatal(err)
	}
	cl, err := RenderClashProfile(c.ingress(), c.device())
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range [][]byte{sb, cl} {
		s := string(out)
		for _, bad := range []string{c.Name, c.Owner, "Brien", "<script>", "wn\\\\er", "Zo"} {
			if strings.Contains(s, bad) {
				t.Errorf("output contains %q", bad)
			}
		}
		// also the JSON-escaped forms
		for _, v := range []string{c.Name, c.Owner} {
			enc, _ := json.Marshal(v)
			if strings.Contains(s, strings.Trim(string(enc), `"`)) {
				t.Errorf("output contains escaped %q", enc)
			}
		}
	}
}

func TestDeepLink(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := c.ingress()
			if got := SubURL(cfg, c.Secret); got != c.SubURL {
				t.Errorf("SubURL = %q, want %q", got, c.SubURL)
			}
			for client, want := range c.DeepLinks {
				got, err := DeepLinkFor(client, c.SubURL, cfg.ProfileName)
				if err != nil || got != want {
					t.Errorf("DeepLinkFor(%s) = %q, %v; want %q", client, got, err, want)
				}
			}
			if got := DeepLink(cfg, c.Secret); got != c.DeepLinks["sing-box"] {
				t.Errorf("DeepLink = %q, want %q", got, c.DeepLinks["sing-box"])
			}
			if got := SingBoxDeepLink(cfg, c.Secret); got != c.DeepLinks["sing-box"] {
				t.Errorf("SingBoxDeepLink = %q", got)
			}
			if got := KaringDeepLink(cfg, c.Secret); got != c.DeepLinks["karing"] {
				t.Errorf("KaringDeepLink = %q", got)
			}
			if got := ClashDeepLink(cfg, c.Secret); got != c.DeepLinks["clash"] {
				t.Errorf("ClashDeepLink = %q", got)
			}
		})
	}
	t.Run("literals", func(t *testing.T) {
		for client, want := range map[string]string{
			"sing-box": "sing-box://import-remote-profile?url=https%3A%2F%2Fh%2Fsub%2Fx#n",
			"clash":    "clash://install-config?url=https%3A%2F%2Fh%2Fsub%2Fx&name=n",
			"karing":   "karing://install-config?url=https%3A%2F%2Fh%2Fsub%2Fx&name=n",
		} {
			if got, err := DeepLinkFor(client, "https://h/sub/x", "n"); err != nil || got != want {
				t.Errorf("%s: %q, %v", client, got, err)
			}
		}
		if got, _ := DeepLinkFor("karing", "https://h/s", "a b"); !strings.HasSuffix(got, "&name=a%20b") {
			t.Errorf("name not encoded: %q", got)
		}
		if _, err := DeepLinkFor("v2rayn", "https://h/sub/x", "n"); err == nil {
			t.Error("unknown client accepted")
		}
		cfg := IngressCfg{Host: "ingress.example.com"}
		if got := SubURL(cfg, "sec"); got != "https://ingress.example.com/sub/sec" {
			t.Errorf("SubURL = %q", got)
		}
	})
}

const baseCfg = `{"host":"h.example.com","public_key":"P","short_id":"ab","sni":"s.example.com"`

func TestConfigDefaults(t *testing.T) {
	cfg, err := ParseIngressCfg([]byte(baseCfg + `}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Port != 443 || cfg.ProfileName != "tunnel" || cfg.Host != "h.example.com" {
		t.Errorf("defaults: %+v", cfg)
	}
	cfg, err = ParseIngressCfg([]byte(baseCfg + `,"port":8443,"profile_name":"vpn"}`))
	if err != nil || cfg.Port != 8443 || cfg.ProfileName != "vpn" {
		t.Errorf("custom: %+v %v", cfg, err)
	}
}

func TestConfigInvalid(t *testing.T) {
	_, errs := loadProfileExpected(t)
	for _, e := range errs {
		_, err := ParseIngressCfg([]byte(e.JSON))
		if err == nil || err.Error() != e.Error {
			t.Errorf("%s: err = %v, want %q", e.JSON, err, e.Error)
		}
	}
	for _, bad := range []string{`[]`, `"x"`, `null`, `{`, ``} {
		if _, err := ParseIngressCfg([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, bad := range []string{`1e3`, `4.0`, `-1`, `99999999999999999999`} {
		if _, err := ParseIngressCfg([]byte(baseCfg + `,"port":` + bad + `}`)); err == nil {
			t.Errorf("port %s accepted", bad)
		}
	}
}

func TestMissingModeFails(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	c := cases["default"]
	d := c.device()
	d.UUIDs = map[string]string{"auto": "a", "direct": "d"}
	want := "device lacks uuids for modes: exit"
	if _, err := RenderProfile(c.ingress(), d); err == nil || err.Error() != want {
		t.Errorf("singbox: %v", err)
	}
	if _, err := RenderClashProfile(c.ingress(), d); err == nil || err.Error() != want {
		t.Errorf("clash: %v", err)
	}
	d.UUIDs = map[string]string{"auto": "a", "direct": "", "exit": "e"}
	if _, err := RenderProfile(c.ingress(), d); err == nil {
		t.Error("empty uuid accepted")
	}
}

func TestCustomPort(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	c := cases["custom"]
	out, _ := RenderProfile(c.ingress(), c.device())
	if n := strings.Count(string(out), `"server_port":8443`); n != 3 {
		t.Errorf("server_port count = %d", n)
	}
}
