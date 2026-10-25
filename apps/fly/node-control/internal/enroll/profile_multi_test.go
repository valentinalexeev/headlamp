package enroll

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// singleOf wraps a single-host fixture in the list shape: one ingress named "default".
func singleOf(g IngressCfg) IngressConfig {
	return IngressConfig{ProfileName: g.ProfileName, LinkHost: g.Host, Ingresses: []Ingress{{
		Name: "default", Host: g.Host, PublicKey: g.PublicKey, ShortID: g.ShortID, SNI: g.SNI, Port: g.Port}}}
}

func (c expectedCase) config() IngressConfig { return singleOf(c.ingress()) }

// Fixed fake data of the multi-ingress cases. Nothing here is a real host, key or identifier.
// ingress-1 keeps the stored (base) credentials, ingress-2 gets derived ones.
var multiCfg = IngressConfig{
	ProfileName: "tunnel",
	LinkHost:    "links.example.com",
	Ingresses: []Ingress{
		{Name: "ingress-1", Host: "ingress-1.example.com", PublicKey: "KEY-ONE-fake", ShortID: "0a1b", SNI: "sni-1.example.com", Port: 443, UUIDs: "base"},
		{Name: "ingress-2", Host: "ingress-2.example.com", PublicKey: "KEY-TWO-fake", ShortID: "2c3d", SNI: "sni-2.example.com", Port: 8443, UUIDs: "derived"},
	},
}.WithKey(testKey)

func multiDevice() Device {
	return Device{ID: 7, Name: "Secret Phone", Owner: "someone", Client: "sing-box", ExpiresAt: 2_000_000_000,
		UUIDs: map[string]string{
			"auto":   "11111111-1111-4111-8111-111111111111",
			"direct": "22222222-2222-4222-8222-222222222222",
			"exit":   "33333333-3333-4333-8333-333333333333",
		}}
}

func TestSingleIngressOutputUnchanged(t *testing.T) {
	cases, _ := loadProfileExpected(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := c.config()
			if !cfg.Single() {
				t.Fatal("fixture is not a single ingress")
			}
			sb, err := RenderProfile(cfg, c.device())
			if err != nil {
				t.Fatal(err)
			}
			if want := golden(t, goldenName("singbox", name)); !bytes.Equal(sb, want) {
				t.Fatalf("sing-box mismatch\n got: %s\nwant: %s", sb, want)
			}
			cl, err := RenderClashProfile(cfg, c.device())
			if err != nil {
				t.Fatal(err)
			}
			if want := golden(t, goldenName("clash", name)); !bytes.Equal(cl, want) {
				t.Fatalf("clash mismatch\n got: %s\nwant: %s", cl, want)
			}
		})
	}
}

func TestMultiIngressSingBoxGolden(t *testing.T) {
	got, err := RenderProfile(multiCfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "profile_multi_singbox.golden"); !bytes.Equal(got, want) {
		t.Fatalf("mismatch\n got: %s\nwant: %s", got, want)
	}
}

func TestMultiIngressClashGolden(t *testing.T) {
	got, err := RenderClashProfile(multiCfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "profile_multi_clash.golden"); !bytes.Equal(got, want) {
		t.Fatalf("mismatch\n got: %s\nwant: %s", got, want)
	}
}

func decodeObj(t *testing.T, b []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, b)
	}
	return m
}

func asList(t *testing.T, v any) []any {
	t.Helper()
	l, ok := v.([]any)
	if !ok {
		t.Fatalf("not a list: %#v", v)
	}
	return l
}

func wantUUID(t *testing.T, ing Ingress, mode string, d Device) string {
	t.Helper()
	u, err := multiCfg.UUIDFor(ing, mode, d.UUIDs[mode])
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestMultiIngressOutboundsPerIngressAndMode(t *testing.T) {
	d := multiDevice()
	sb, err := RenderProfile(multiCfg, d)
	if err != nil {
		t.Fatal(err)
	}
	outs := map[string]map[string]any{}
	for _, o := range asList(t, decodeObj(t, sb)["outbounds"]) {
		m := o.(map[string]any)
		outs[m["tag"].(string)] = m
	}
	t.Run("sing-box vless outbounds", func(t *testing.T) {
		n := 0
		for _, mode := range Modes {
			for _, ing := range multiCfg.Ingresses {
				o := outs[mode+"-"+ing.Name]
				if o == nil {
					t.Fatalf("missing outbound %s-%s", mode, ing.Name)
				}
				n++
				tls := o["tls"].(map[string]any)
				reality := tls["reality"].(map[string]any)
				if o["type"] != "vless" || o["server"] != ing.Host || int(o["server_port"].(float64)) != ing.Port ||
					o["uuid"] != wantUUID(t, ing, mode, d) || tls["server_name"] != ing.SNI ||
					reality["public_key"] != ing.PublicKey || reality["short_id"] != ing.ShortID {
					t.Errorf("%s-%s: wrong parameters: %v", mode, ing.Name, o)
				}
			}
		}
		if n != 6 {
			t.Fatalf("%d outbounds", n)
		}
	})
	t.Run("sing-box groups", func(t *testing.T) {
		for _, mode := range Modes {
			g := outs[mode]
			if g == nil || g["type"] != "urltest" {
				t.Fatalf("group %s: %v", mode, g)
			}
			members := asList(t, g["outbounds"])
			if len(members) != 2 || members[0] != mode+"-ingress-1" || members[1] != mode+"-ingress-2" {
				t.Errorf("group %s members: %v", mode, members)
			}
			if g["url"] != "https://www.gstatic.com/generate_204" || g["interval"] != "3m" || g["tolerance"].(float64) != 50 {
				t.Errorf("group %s probe: %v", mode, g)
			}
		}
		sel := outs["select"]
		if sel["type"] != "selector" || sel["default"] != "auto" {
			t.Fatalf("select: %v", sel)
		}
		got := asList(t, sel["outbounds"])
		if len(got) != 3 || got[0] != "auto" || got[1] != "direct" || got[2] != "exit" {
			t.Errorf("select outbounds: %v", got)
		}
	})
	t.Run("clash proxies and groups", func(t *testing.T) {
		cl, err := RenderClashProfile(multiCfg, d)
		if err != nil {
			t.Fatal(err)
		}
		p := decodeObj(t, cl)
		proxies := map[string]map[string]any{}
		for _, x := range asList(t, p["proxies"]) {
			m := x.(map[string]any)
			proxies[m["name"].(string)] = m
		}
		if len(proxies) != 6 {
			t.Fatalf("%d proxies", len(proxies))
		}
		for _, mode := range Modes {
			for _, ing := range multiCfg.Ingresses {
				x := proxies[mode+"-"+ing.Name]
				ro, _ := x["reality-opts"].(map[string]any)
				if x == nil || x["server"] != ing.Host || int(x["port"].(float64)) != ing.Port ||
					x["uuid"] != wantUUID(t, ing, mode, d) || x["servername"] != ing.SNI ||
					ro["public-key"] != ing.PublicKey || ro["short-id"] != ing.ShortID {
					t.Errorf("%s-%s: wrong parameters: %v", mode, ing.Name, x)
				}
			}
		}
		groups := map[string]map[string]any{}
		var order []string
		for _, x := range asList(t, p["proxy-groups"]) {
			m := x.(map[string]any)
			groups[m["name"].(string)] = m
			order = append(order, m["name"].(string))
		}
		if strings.Join(order, ",") != "select,auto,direct,exit" {
			t.Fatalf("group order %v", order)
		}
		sel := asList(t, groups["select"]["proxies"])
		if groups["select"]["type"] != "select" || len(sel) != 3 || sel[0] != "auto" || sel[1] != "direct" || sel[2] != "exit" {
			t.Errorf("selector: %v", groups["select"])
		}
		for _, mode := range Modes {
			g := groups[mode]
			members := asList(t, g["proxies"])
			if g["type"] != "url-test" || g["url"] != "https://www.gstatic.com/generate_204" || g["interval"].(float64) != 180 ||
				len(members) != 2 || members[0] != mode+"-ingress-1" || members[1] != mode+"-ingress-2" {
				t.Errorf("group %s: %v", mode, g)
			}
		}
	})
}

func TestResolverRuleCoversAllIngressHosts(t *testing.T) {
	sb, err := RenderProfile(multiCfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	p := decodeObj(t, sb)
	// One resolver setting covers every outbound: the route default, the local server. No outbound
	// overrides it, and no DNS rule sends an ingress host to the remote (tunnelled) server.
	def := p["route"].(map[string]any)["default_domain_resolver"].(map[string]any)
	if def["server"] != "local" {
		t.Fatalf("default resolver: %v", def)
	}
	servers := map[string]string{}
	for _, s := range asList(t, p["dns"].(map[string]any)["servers"]) {
		m := s.(map[string]any)
		servers[m["tag"].(string)] = m["type"].(string)
	}
	if servers["local"] != "local" {
		t.Fatalf("dns servers: %v", servers)
	}
	hosts := map[string]bool{}
	for _, o := range asList(t, p["outbounds"]) {
		m := o.(map[string]any)
		if _, ok := m["domain_resolver"]; ok {
			t.Errorf("outbound %v overrides the resolver", m["tag"])
		}
		if h, ok := m["server"].(string); ok {
			hosts[h] = true
		}
	}
	for _, ing := range multiCfg.Ingresses {
		if !hosts[ing.Host] {
			t.Errorf("host %s not used by any outbound", ing.Host)
		}
	}
	if rules, ok := p["dns"].(map[string]any)["rules"]; ok {
		for _, r := range asList(t, rules) {
			if m := r.(map[string]any); m["server"] == "remote" {
				t.Errorf("dns rule to the remote server: %v", m)
			}
		}
	}
	cl, err := RenderClashProfile(multiCfg, multiDevice())
	if err != nil {
		t.Fatal(err)
	}
	ns := asList(t, decodeObj(t, cl)["dns"].(map[string]any)["proxy-server-nameserver"])
	if len(ns) != 1 || ns[0] != "system" {
		t.Errorf("clash proxy-server-nameserver: %v", ns)
	}
}

func TestLinksUseLinkHost(t *testing.T) {
	const secret = "SECRET123"
	want := "https://links.example.com/sub/" + secret
	if got := SubURL(multiCfg, secret); got != want {
		t.Fatalf("SubURL = %q", got)
	}
	for name, link := range map[string]string{
		"sing-box": SingBoxDeepLink(multiCfg, secret), "default": DeepLink(multiCfg, secret),
		"karing": KaringDeepLink(multiCfg, secret), "clash": ClashDeepLink(multiCfg, secret),
	} {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(link, quote(want)) {
				t.Errorf("link %q does not carry %q", link, want)
			}
			for _, ing := range multiCfg.Ingresses {
				if strings.Contains(link, ing.Host) {
					t.Errorf("link %q carries ingress host %s", link, ing.Host)
				}
			}
		})
	}
	t.Run("admin links", func(t *testing.T) {
		f := newAdminFxCfg(t, multiCfg)
		id := f.createID()
		rec := f.post(idPath(id, "links"), map[string]any{"ttl": 3600, "max_uses": 1})
		if rec.Code != 200 {
			t.Fatalf("%d %s", rec.Code, rec.Body)
		}
		if url := decode(t, rec)["url"].(string); !strings.HasPrefix(url, "https://links.example.com/e/") {
			t.Errorf("one-time link %q", url)
		}
		rec = f.post(idPath(id, "rotate-secret"), map[string]any{})
		if u := decode(t, rec)["subscription_url"].(string); !strings.HasPrefix(u, "https://links.example.com/sub/") {
			t.Errorf("rotated sub URL %q", u)
		}
		for _, ing := range multiCfg.Ingresses {
			if strings.Contains(rec.Body.String(), ing.Host) {
				t.Errorf("response carries ingress host %s", ing.Host)
			}
		}
	})
}

func TestNoNameOrOwnerInMultiProfile(t *testing.T) {
	d := multiDevice()
	d.Name, d.Owner = "Zoë <script>", `wn"er\x`
	sb, err := RenderProfile(multiCfg, d)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := RenderClashProfile(multiCfg, d)
	if err != nil {
		t.Fatal(err)
	}
	for _, out := range [][]byte{sb, cl} {
		s := string(out)
		for _, v := range []string{d.Name, d.Owner, "<script>", "Zo"} {
			enc, _ := json.Marshal(v)
			if strings.Contains(s, v) || strings.Contains(s, strings.Trim(string(enc), `"`)) {
				t.Errorf("output contains %q", v)
			}
		}
	}
}

func TestMultiGoldenPassesSingBoxCheck(t *testing.T) {
	bin, err := exec.LookPath("sing-box")
	if err != nil {
		t.Skip("sing-box is not on PATH")
	}
	f := filepath.Join("..", "..", "testdata", "profile_multi_singbox.golden")
	if _, err := os.Stat(f); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "check", "-c", f).CombinedOutput(); err != nil {
		t.Fatalf("sing-box check: %v\n%s", err, out)
	}
}
