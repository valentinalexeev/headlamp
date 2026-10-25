package enroll

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

func TestDeriveUUIDFormatAndDeterminism(t *testing.T) {
	base := "11111111-1111-4111-8111-111111111111"
	t.Run("canonical form with version and variant bits", func(t *testing.T) {
		for i := 0; i < 200; i++ {
			u := DeriveUUID(testKey, "ingress-2", "auto", fmt.Sprintf("%s-%d", base, i))
			if !uuidRe.MatchString(u) {
				t.Fatalf("not a canonical v4 uuid: %q", u)
			}
		}
	})
	t.Run("same inputs same output", func(t *testing.T) {
		a := DeriveUUID(testKey, "ingress-2", "auto", base)
		b := DeriveUUID(append([]byte(nil), testKey...), "ingress-2", "auto", base)
		if a == "" || a != b {
			t.Fatalf("%q vs %q", a, b)
		}
	})
	t.Run("never equals the base", func(t *testing.T) {
		if DeriveUUID(testKey, "ingress-2", "auto", base) == base {
			t.Fatal("derived equals base")
		}
	})
}

func TestDeriveUUIDDiffersPerInputs(t *testing.T) {
	base := "11111111-1111-4111-8111-111111111111"
	ref := DeriveUUID(testKey, "ingress-1", "auto", base)
	t.Run("each input changes the output", func(t *testing.T) {
		for name, got := range map[string]string{
			"ingress": DeriveUUID(testKey, "ingress-2", "auto", base),
			"mode":    DeriveUUID(testKey, "ingress-1", "direct", base),
			"base":    DeriveUUID(testKey, "ingress-1", "auto", "11111111-1111-4111-8111-111111111112"),
			"key":     DeriveUUID([]byte("another-fake-key-0123456789abcd"), "ingress-1", "auto", base),
		} {
			if got == ref {
				t.Errorf("changing %s does not change the output", name)
			}
		}
	})
	t.Run("field boundaries are unambiguous", func(t *testing.T) {
		a := DeriveUUID(testKey, "ab", "c", base)
		b := DeriveUUID(testKey, "a", "bc", base)
		if a == b {
			t.Error("ingress/mode boundary collides")
		}
	})
	t.Run("no collisions across a matrix", func(t *testing.T) {
		seen := map[string]string{}
		for _, ing := range []string{"ingress-1", "ingress-2", "ingress-3"} {
			for _, mode := range Modes {
				for d := 0; d < 5; d++ {
					b := fmt.Sprintf("%08d-0000-4000-8000-000000000000", d)
					u := DeriveUUID(testKey, ing, mode, b)
					id := fmt.Sprintf("%s/%s/%d", ing, mode, d)
					if prev, dup := seen[u]; dup {
						t.Fatalf("%s collides with %s", id, prev)
					}
					seen[u] = id
				}
			}
		}
		if len(seen) != 45 {
			t.Fatalf("%d values", len(seen))
		}
	})
}

func TestDeriveUUIDShortKey(t *testing.T) {
	for _, k := range [][]byte{nil, {}, []byte("short"), bytes.Repeat([]byte("k"), 15)} {
		if got := DeriveUUID(k, "ingress-1", "auto", "x"); got != "" {
			t.Errorf("key of %d bytes gave %q", len(k), got)
		}
	}
	if DeriveUUID(bytes.Repeat([]byte("k"), 16), "ingress-1", "auto", "x") == "" {
		t.Error("16-byte key refused")
	}
}

func uuidsCfg(extra ...string) []byte {
	return cfgOf(`"link_host":"links.example.com"`, extra...)
}

func uuidsTwo(a, b string) []byte {
	return uuidsCfg(ing("ingress-1", "ingress-1.example.com", a), ing("ingress-2", "ingress-2.example.com", b))
}

func uuidsOf(t *testing.T, data []byte) []string {
	t.Helper()
	c, err := ParseIngressConfig(data)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, i := range c.Ingresses {
		out = append(out, i.UUIDs)
	}
	return out
}

func TestUUIDsFieldValidation(t *testing.T) {
	t.Run("explicit values are kept", func(t *testing.T) {
		got := uuidsOf(t, uuidsTwo(`"uuids":"base"`, `"uuids":"derived"`))
		if got[0] != "base" || got[1] != "derived" {
			t.Errorf("%v", got)
		}
	})
	t.Run("at most one base", func(t *testing.T) {
		_, err := ParseIngressConfig(uuidsTwo(`"uuids":"base"`, `"uuids":"base"`))
		if err == nil || !strings.Contains(err.Error(), "ingress-2") || !strings.Contains(err.Error(), "base") {
			t.Errorf("err = %v", err)
		}
	})
	t.Run("unknown value names the ingress and not the key", func(t *testing.T) {
		for _, bad := range []string{`"uuids":"shared"`, `"uuids":""`, `"uuids":1`, `"uuids":"Base"`} {
			_, err := ParseIngressConfig(uuidsTwo("", bad))
			if err == nil || !strings.Contains(err.Error(), "ingress-2") || !strings.Contains(err.Error(), "uuids") {
				t.Errorf("%s: err = %v", bad, err)
				continue
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), fakeSID) {
				t.Errorf("%s: error echoes key material: %v", bad, err)
			}
		}
	})
}

// rawIng builds an ingress object without the helper's uuids handling.
func rawIng(name, host, extra string) string {
	s := fmt.Sprintf(`{"name":%q,"host":%q,"public_key":%q,"short_id":%q`, name, host, fakeKey, fakeSID)
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func TestNewShapeRequiresExplicitUUIDs(t *testing.T) {
	want := func(t *testing.T, data []byte, idx, name string) {
		t.Helper()
		_, err := ParseIngressConfig(data)
		exp := "ingresses[" + idx + "] " + name + `: uuids must be "base" or "derived"`
		if err == nil || !strings.Contains(err.Error(), exp) {
			t.Fatalf("err = %v, want it to contain %q", err, exp)
		}
		if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), fakeSID) {
			t.Errorf("error echoes key material: %v", err)
		}
	}
	t.Run("missing on one of two entries", func(t *testing.T) {
		want(t, uuidsCfg(rawIng("ingress-1", "ingress-1.example.com", `"uuids":"base"`),
			rawIng("ingress-2", "ingress-2.example.com", "")), "1", "ingress-2")
	})
	t.Run("missing on the single entry", func(t *testing.T) {
		want(t, cfgOf(``, rawIng("ingress-1", "ingress-1.example.com", "")), "0", "ingress-1")
	})
	t.Run("invalid value", func(t *testing.T) {
		want(t, cfgOf(``, rawIng("ingress-1", "ingress-1.example.com", `"uuids":"shared"`)), "0", "ingress-1")
	})
}

func TestOldShapeStillBase(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	n := 0
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(c.Cfg)
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := ParseIngressConfig(raw)
			if err != nil {
				t.Fatal(err)
			}
			if len(cfg.Ingresses) != 1 || cfg.Ingresses[0].UUIDs != "base" {
				t.Errorf("%+v", cfg.Ingresses)
			}
			n++
		})
	}
	if n == 0 {
		t.Fatal("no fixtures")
	}
}

func TestUUIDForUnsetErrors(t *testing.T) {
	for _, c := range []IngressConfig{multiCfg, singleOf(IngressCfg{Host: "ingress-1.example.com"}).WithKey(testKey)} {
		in := c.Ingresses[0]
		in.UUIDs = ""
		_, err := c.UUIDFor(in, "auto", "11111111-1111-4111-8111-111111111111")
		if err == nil || !strings.Contains(err.Error(), "uuids is not set for ingress "+in.Name) {
			t.Errorf("err = %v", err)
		}
	}
}

func TestRemovingAnEntryCannotFlipTheSurvivor(t *testing.T) {
	first := rawIng("ingress-1", "ingress-1.example.com", `"uuids":"base"`)
	second := rawIng("ingress-2", "ingress-2.example.com", `"uuids":"derived"`)
	t.Run("two entries parse", func(t *testing.T) {
		got := uuidsOf(t, uuidsCfg(first, second))
		if got[0] != "base" || got[1] != "derived" {
			t.Errorf("%v", got)
		}
	})
	t.Run("survivor keeps its explicit value", func(t *testing.T) {
		got := uuidsOf(t, cfgOf(``, second))
		if len(got) != 1 || got[0] != "derived" {
			t.Errorf("%v", got)
		}
	})
	t.Run("survivor without an explicit value is rejected", func(t *testing.T) {
		_, err := ParseIngressConfig(cfgOf(``, rawIng("ingress-2", "ingress-2.example.com", "")))
		if err == nil {
			t.Fatal("accepted")
		}
	})
}

// Fixed fake base UUIDs of a device: they must never reach a derived ingress.
func isolationDevice(n int) Device {
	u := func(c string) string {
		return strings.Repeat(c, 8) + fmt.Sprintf("-%d%d%d%d-4%d%d%d-8%d%d%d-", n, n, n, n, n, n, n, n, n, n) + strings.Repeat(c, 12)
	}
	return Device{ID: int64(n), Name: "d", Client: "sing-box", ExpiresAt: 2_000_000_000,
		UUIDs: map[string]string{"auto": u("1"), "direct": u("2"), "exit": u("3")}}
}

func TestBaseIngressKeepsStoredUUIDs(t *testing.T) {
	d := isolationDevice(5)
	sb, err := RenderProfile(multiCfg, d)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := RenderClashProfile(multiCfg, d)
	if err != nil {
		t.Fatal(err)
	}
	for name, out := range map[string][]byte{"sing-box": sb, "clash": cl} {
		t.Run(name, func(t *testing.T) {
			s := string(out)
			for _, m := range Modes {
				stored := d.UUIDs[m]
				if strings.Count(s, stored) != 1 {
					t.Errorf("stored uuid of %s appears %d times, want exactly once (base ingress)", m, strings.Count(s, stored))
				}
				derived := DeriveUUID(testKey, "ingress-2", m, stored)
				if derived == "" || derived == stored || strings.Count(s, derived) != 1 {
					t.Errorf("derived uuid of %s missing or equal to stored: %q", m, derived)
				}
			}
		})
	}
	t.Run("derived ingress carries no stored uuid", func(t *testing.T) {
		only := multiCfg
		only.Ingresses = multiCfg.Ingresses[1:]
		only.LinkHost = "links.example.com"
		out, err := RenderProfile(only.WithKey(testKey), d)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range Modes {
			if strings.Contains(string(out), d.UUIDs[m]) {
				t.Errorf("base uuid of %s reached a derived ingress", m)
			}
		}
	})
}

func TestUUIDForChoices(t *testing.T) {
	base := "11111111-1111-4111-8111-111111111111"
	t.Run("base returns base", func(t *testing.T) {
		got, err := multiCfg.UUIDFor(multiCfg.Ingresses[0], "auto", base)
		if err != nil || got != base {
			t.Errorf("%q %v", got, err)
		}
	})
	t.Run("derived returns the derivation", func(t *testing.T) {
		got, err := multiCfg.UUIDFor(multiCfg.Ingresses[1], "auto", base)
		if err != nil || got != DeriveUUID(testKey, "ingress-2", "auto", base) {
			t.Errorf("%q %v", got, err)
		}
	})
	t.Run("WithKey copies the key", func(t *testing.T) {
		k := append([]byte(nil), testKey...)
		c := multiCfg.WithKey(k)
		k[0] ^= 0xff
		got, _ := c.UUIDFor(c.Ingresses[1], "auto", base)
		if got != DeriveUUID(testKey, "ingress-2", "auto", base) {
			t.Error("caller's slice leaked into the config")
		}
	})
}

func TestRenderFailsWithoutKeyForDerived(t *testing.T) {
	nokey := IngressConfig{ProfileName: multiCfg.ProfileName, LinkHost: multiCfg.LinkHost, Ingresses: multiCfg.Ingresses}
	d := isolationDevice(5)
	t.Run("derived ingress without key", func(t *testing.T) {
		_, err := RenderProfile(nokey, d)
		if err == nil || !strings.Contains(err.Error(), "ENROLL_SECRET_KEY") {
			t.Errorf("sing-box err = %v", err)
		}
		_, err = RenderClashProfile(nokey, d)
		if err == nil || !strings.Contains(err.Error(), "ENROLL_SECRET_KEY") {
			t.Errorf("clash err = %v", err)
		}
	})
	t.Run("short key", func(t *testing.T) {
		if _, err := RenderProfile(nokey.WithKey([]byte("short")), d); err == nil {
			t.Error("short key accepted")
		}
	})
	t.Run("base only needs no key", func(t *testing.T) {
		c := nokey
		c.Ingresses = nokey.Ingresses[:1]
		if _, err := RenderProfile(c, d); err != nil {
			t.Error(err)
		}
	})
}
