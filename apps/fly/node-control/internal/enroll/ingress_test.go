package enroll

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

const fakeKey = "FAKEKEY-distinctive-0123456789abcdefABCDEF"
const fakeSID = "fakesid99"

// ing builds one ingress object for the new shape; extra is appended verbatim.
func ing(name, host, extra string) string {
	s := fmt.Sprintf(`{"name":%q,"host":%q,"public_key":%q,"short_id":%q`, name, host, fakeKey, fakeSID)
	if !strings.Contains(extra, `"uuids"`) {
		s += `,"uuids":"derived"`
	}
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func cfgOf(top string, ingresses ...string) []byte {
	s := `{"ingresses":[` + strings.Join(ingresses, ",") + `]`
	if top != "" {
		s += "," + top
	}
	return []byte(s + "}")
}

func TestParseOldSingleHostShape(t *testing.T) {
	cases, errs := loadProfileExpected(t)
	t.Run("valid fixtures equal ParseIngressCfg", func(t *testing.T) {
		for name, c := range cases {
			t.Run(name, func(t *testing.T) {
				want := c.ingress()
				raw, _ := json.Marshal(c.Cfg)
				got, err := ParseIngressConfig(raw)
				if err != nil {
					t.Fatal(err)
				}
				if len(got.Ingresses) != 1 || got.Ingresses[0].Name != "default" {
					t.Fatalf("ingresses = %+v", got.Ingresses)
				}
				if got.LinkHost != want.Host || got.ProfileName != want.ProfileName {
					t.Errorf("link/profile = %q %q", got.LinkHost, got.ProfileName)
				}
				if got.Link() != want {
					t.Errorf("Link() = %+v, want %+v", got.Link(), want)
				}
			})
		}
	})
	t.Run("error text unchanged", func(t *testing.T) {
		for _, e := range errs {
			_, err := ParseIngressConfig([]byte(e.JSON))
			if err == nil || err.Error() != e.Error {
				t.Errorf("%s: err = %v, want %q", e.JSON, err, e.Error)
			}
		}
	})
	t.Run("not an object", func(t *testing.T) {
		for _, bad := range []string{`[]`, `"x"`, `null`, `{`, ``} {
			if _, err := ParseIngressConfig([]byte(bad)); err == nil {
				t.Errorf("%q accepted", bad)
			}
		}
	})
	t.Run("old shape requires sni", func(t *testing.T) {
		_, err := ParseIngressConfig([]byte(`{"host":"h.example.com","public_key":"P","short_id":"ab"}`))
		if err == nil || err.Error() != "ENROLL_CONFIG: missing or empty: sni" {
			t.Errorf("err = %v", err)
		}
	})
}

func TestParseNewShape(t *testing.T) {
	t.Run("two ingresses with defaults", func(t *testing.T) {
		c, err := ParseIngressConfig(cfgOf(`"link_host":"links.example.com"`,
			ing("ingress-1", "ingress-1.example.com", ""),
			ing("ingress-2", "ingress-2.example.com", `"sni":"front.example.com","port":8443,"agent_url":"http://10.0.0.2:9000","token_env":"AGENT_TOKEN_INGRESS_2"`)))
		if err != nil {
			t.Fatal(err)
		}
		if c.ProfileName != "tunnel" || c.LinkHost != "links.example.com" || len(c.Ingresses) != 2 {
			t.Fatalf("%+v", c)
		}
		a, b := c.Ingresses[0], c.Ingresses[1]
		if a.Port != 443 || a.SNI != "ingress-1.example.com" || a.AgentURL != "" || a.TokenEnv != "" {
			t.Errorf("first: %+v", a)
		}
		if b.Port != 8443 || b.SNI != "front.example.com" || b.AgentURL != "http://10.0.0.2:9000" ||
			b.TokenEnv != "AGENT_TOKEN_INGRESS_2" || b.PublicKey != fakeKey || b.ShortID != fakeSID {
			t.Errorf("second: %+v", b)
		}
		if c.Single() {
			t.Error("Single() true for two")
		}
	})
	t.Run("one ingress defaults link host to its host", func(t *testing.T) {
		c, err := ParseIngressConfig(cfgOf(`"profile_name":"vpn"`, ing("ingress-1", "ingress-1.example.com", "")))
		if err != nil {
			t.Fatal(err)
		}
		if c.LinkHost != "ingress-1.example.com" || c.ProfileName != "vpn" || !c.Single() {
			t.Errorf("%+v", c)
		}
	})
}

func TestParseRejectsMixedShapes(t *testing.T) {
	for _, k := range []string{"host", "public_key", "short_id", "sni", "port"} {
		t.Run(k, func(t *testing.T) {
			val := `"x"`
			if k == "port" {
				val = `443`
			}
			_, err := ParseIngressConfig(cfgOf(fmt.Sprintf(`%q:%s`, k, val), ing("ingress-1", "ingress-1.example.com", "")))
			if err == nil || err.Error() != "ENROLL_CONFIG: use either ingresses or the single-host keys, not both" {
				t.Errorf("err = %v", err)
			}
		})
	}
}

func TestParseRequiresLinkHostWithManyIngresses(t *testing.T) {
	_, err := ParseIngressConfig(cfgOf("", ing("ingress-1", "ingress-1.example.com", ""), ing("ingress-2", "ingress-2.example.com", "")))
	if err == nil || !strings.Contains(err.Error(), "link_host") {
		t.Errorf("err = %v", err)
	}
}

func TestParseValidation(t *testing.T) {
	ok1 := ing("ingress-1", "ingress-1.example.com", "")
	link := `"link_host":"links.example.com"`
	many := func(n int) []byte {
		var l []string
		for i := 1; i <= n; i++ {
			l = append(l, ing(fmt.Sprintf("ingress-%d", i), fmt.Sprintf("ingress-%d.example.com", i), ""))
		}
		return cfgOf(link, l...)
	}
	longLabel := strings.Repeat("a", 64) + ".example.com"
	longHost := strings.Repeat("a.", 130) + "com"
	cases := []struct {
		name string
		data []byte
		want string // substring of the error
	}{
		{"duplicate name", cfgOf(link, ok1, ing("ingress-1", "ingress-2.example.com", "")), "ingress-1: duplicate name"},
		{"duplicate host", cfgOf(link, ok1, ing("ingress-2", "ingress-1.example.com", "")), "ingress-2: duplicate host"},
		{"bad name upper", cfgOf(link, ing("Ingress", "a.example.com", "")), "name"},
		{"bad name underscore", cfgOf(link, ing("in_1", "a.example.com", "")), "name"},
		{"bad name leading hyphen", cfgOf(link, ing("-in", "a.example.com", "")), "name"},
		{"empty name", cfgOf(link, ing("", "a.example.com", "")), "name"},
		{"host scheme", cfgOf("", ing("ingress-1", "https://a.example.com", "")), "ingress-1: host"},
		{"host port", cfgOf("", ing("ingress-1", "a.example.com:443", "")), "ingress-1: host"},
		{"host path", cfgOf("", ing("ingress-1", "a.example.com/x", "")), "ingress-1: host"},
		{"host uppercase", cfgOf("", ing("ingress-1", "A.example.com", "")), "ingress-1: host"},
		{"host underscore", cfgOf("", ing("ingress-1", "a_b.example.com", "")), "ingress-1: host"},
		{"host label too long", cfgOf("", ing("ingress-1", longLabel, "")), "ingress-1: host"},
		{"host too long", cfgOf("", ing("ingress-1", longHost, "")), "ingress-1: host"},
		{"host empty label", cfgOf("", ing("ingress-1", "a..example.com", "")), "ingress-1: host"},
		{"link host scheme", cfgOf(`"link_host":"https://links.example.com"`, ok1), "link_host"},
		{"link host uppercase", cfgOf(`"link_host":"Links.example.com"`, ok1), "link_host"},
		{"link host empty", cfgOf(`"link_host":""`, ok1), "link_host"},
		{"no ingresses", cfgOf(""), "at least one"},
		{"nine ingresses", many(9), "at most 8"},
		{"agent_url credentials", cfgOf("", ing("ingress-1", "a.example.com", `"agent_url":"http://u:p@10.0.0.2:9000"`)), "ingress-1: agent_url"},
		{"agent_url scheme", cfgOf("", ing("ingress-1", "a.example.com", `"agent_url":"ftp://10.0.0.2"`)), "ingress-1: agent_url"},
		{"agent_url no host", cfgOf("", ing("ingress-1", "a.example.com", `"agent_url":"http://"`)), "ingress-1: agent_url"},
		{"token_env lower", cfgOf("", ing("ingress-1", "a.example.com", `"token_env":"agent_token"`)), "ingress-1: token_env"},
		{"token_env digit first", cfgOf("", ing("ingress-1", "a.example.com", `"token_env":"1TOKEN"`)), "ingress-1: token_env"},
		{"empty public_key", cfgOf("", `{"name":"ingress-1","host":"a.example.com","public_key":"","short_id":"ab"}`), "ingress-1: public_key"},
		{"missing short_id", cfgOf("", `{"name":"ingress-1","host":"a.example.com","public_key":"P"}`), "ingress-1: short_id"},
		{"bad port", cfgOf("", ing("ingress-1", "a.example.com", `"port":70000`)), "ingress-1: port"},
		{"ingress not object", []byte(`{"ingresses":[1]}`), "ingresses"},
		{"ingresses not array", []byte(`{"ingresses":"x"}`), "ingresses"},
		{"bad profile_name", cfgOf(`"profile_name":""`, ok1), "profile_name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ParseIngressConfig(c.data)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want containing %q", err, c.want)
			}
		})
	}
	t.Run("eight ingresses accepted", func(t *testing.T) {
		if _, err := ParseIngressConfig(many(8)); err != nil {
			t.Error(err)
		}
	})
}

func TestParseErrorsDoNotEchoKeyMaterial(t *testing.T) {
	good := ing("ingress-1", "ingress-1.example.com", "")
	bad := []string{
		string(cfgOf(`"link_host":"links.example.com"`, good, ing("ingress-1", "ingress-2.example.com", ""))),
		string(cfgOf(`"link_host":"links.example.com"`, good, ing("ingress-2", "ingress-1.example.com", ""))),
		string(cfgOf("", ing("Bad", "ingress-1.example.com", ""))),
		string(cfgOf("", ing("ingress-1", "HOST", ""))),
		string(cfgOf("", ing("ingress-1", "a.example.com", `"port":0`))),
		string(cfgOf("", ing("ingress-1", "a.example.com", `"agent_url":"http://u:p@x"`))),
		string(cfgOf("", ing("ingress-1", "a.example.com", `"token_env":"bad"`))),
		string(cfgOf(`"host":"x"`, good)),
		string(cfgOf(`"link_host":"BAD"`, good)),
		string(cfgOf("", good, ing("ingress-2", "ingress-2.example.com", ""))),
		`{"ingresses":[{"name":"ingress-1","host":"a.example.com","public_key":"` + fakeKey + `","short_id":5}]}`,
		`{"ingresses":[{"name":"ingress-1","host":"a.example.com","public_key":5,"short_id":"` + fakeSID + `"}]}`,
		`{"host":"h.example.com","public_key":"` + fakeKey + `","short_id":"` + fakeSID + `","sni":"s.example.com","port":0}`,
	}
	for i, b := range bad {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, err := ParseIngressConfig([]byte(b))
			if err == nil {
				t.Fatal("accepted")
			}
			if strings.Contains(err.Error(), fakeKey) || strings.Contains(err.Error(), fakeSID) {
				t.Errorf("error echoes key material: %v", err)
			}
		})
	}
}

func TestLinkView(t *testing.T) {
	c, err := ParseIngressConfig(cfgOf(`"link_host":"links.example.com","profile_name":"vpn"`,
		ing("ingress-1", "ingress-1.example.com", `"port":8443,"sni":"front.example.com"`),
		ing("ingress-2", "ingress-2.example.com", "")))
	if err != nil {
		t.Fatal(err)
	}
	want := IngressCfg{Host: "links.example.com", PublicKey: fakeKey, ShortID: fakeSID,
		SNI: "front.example.com", Port: 8443, ProfileName: "vpn"}
	if got := c.Link(); got != want {
		t.Errorf("Link() = %+v, want %+v", got, want)
	}
}

func TestUnknownKeysRejectedInNewShape(t *testing.T) {
	t.Run("unknown ingress key names the key and the ingress", func(t *testing.T) {
		_, err := ParseIngressConfig(cfgOf("", ing("ingress-1", "ingress-1.example.com", `"agent_ulr":"http://x.example.com"`)))
		if err == nil || !strings.Contains(err.Error(), "agent_ulr") || !strings.Contains(err.Error(), "ingress-1") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("unknown top-level key", func(t *testing.T) {
		_, err := ParseIngressConfig(cfgOf(`"link_hots":"links.example.com"`, ing("ingress-1", "ingress-1.example.com", "")))
		if err == nil || !strings.Contains(err.Error(), "link_hots") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a key that looks like a value is not echoed", func(t *testing.T) {
		odd := strings.Repeat("S3CRET", 10)
		_, err := ParseIngressConfig(cfgOf("", ing("ingress-1", "ingress-1.example.com", `"`+odd+`":1`)))
		if err == nil || strings.Contains(err.Error(), odd) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("every documented key is accepted", func(t *testing.T) {
		all := `"sni":"s.example.com","port":8443,"agent_url":"http://100.64.0.2:9000","token_env":"AGENT_TOKEN_X"`
		if _, err := ParseIngressConfig(cfgOf(`"profile_name":"p","link_host":"links.example.com"`, ing("ingress-1", "ingress-1.example.com", all))); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("the old shape stays lenient", func(t *testing.T) {
		raw := `{"host":"ingress.example.test","public_key":"P","short_id":"ab","sni":"s.example.test","extra":1}`
		if _, err := ParseIngressConfig([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	})
}
