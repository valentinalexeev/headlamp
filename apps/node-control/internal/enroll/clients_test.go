package enroll

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"strings"
	"testing"
)

func TestCatalogLinks(t *testing.T) {
	sub := "https://link.example.com/sub/abc/uri"
	cases := map[string]string{
		"sing-box":     "sing-box://import-remote-profile?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi#tunnel",
		"karing":       "karing://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"clash":        "clash://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"stash":        "stash://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"shadowrocket": "shadowrocket://add/sub://" + base64.StdEncoding.EncodeToString([]byte(sub)) + "?remark=tunnel",
	}
	for id, want := range cases {
		got, err := DeepLinkFor(id, sub, "tunnel")
		if err != nil || got != want {
			t.Errorf("%s: %q %v, want %q", id, got, err, want)
		}
	}
	if _, err := DeepLinkFor("nope", sub, "x"); err == nil {
		t.Error("unknown client accepted")
	}
	if clientsRepr != "('sing-box', 'karing', 'clash', 'shadowrocket', 'stash')" {
		t.Errorf("clientsRepr %s", clientsRepr)
	}
}

func TestFormatOfStoredClient(t *testing.T) {
	for client, want := range map[string]string{"sing-box": FormatSingBox, "karing": FormatSingBox, "clash": FormatClash,
		"stash": FormatStash, "shadowrocket": FormatURIList, "": FormatSingBox, "other": FormatSingBox} {
		if got := formatOf(client); got != want {
			t.Errorf("%q: %s, want %s", client, got, want)
		}
	}
}

func TestPlatformOf(t *testing.T) {
	cases := map[string]string{
		"Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X) AppleWebKit/605.1.15":            PlatformIOS,
		"Mozilla/5.0 (iPad; CPU OS 19_0 like Mac OS X)":                                          PlatformIOS,
		"Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 Chrome/140 Mobile":          PlatformAndroid,
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 Version/19 Safari": PlatformMacOS,
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) Chrome/140":                                   PlatformWindows,
		"Mozilla/5.0 (X11; Linux x86_64) Firefox/145":                                            PlatformLinux,
		"curl/8": "",
		"":       "",
	}
	for ua, want := range cases {
		if got := PlatformOf(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

func TestParseClients(t *testing.T) {
	base := `{"host":"r.example.com","public_key":"k","short_id":"s","sni":"x.example.com"`
	cfg, err := ParseIngressConfig([]byte(base + `,"clients":[{"id":"stash","install":{"ios":"https://apps.example.com/stash"}},{"id":"karing"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Clients) != 2 || cfg.Clients[0].ID != "stash" || cfg.Clients[0].Install["ios"] != "https://apps.example.com/stash" {
		t.Fatalf("%+v", cfg.Clients)
	}
	if got := cfg.Offered(); len(got) != 2 {
		t.Fatalf("offered %+v", got)
	}
	none, _ := ParseIngressConfig([]byte(base + `}`))
	if len(none.Offered()) != len(Catalog) {
		t.Fatal("without clients every app is offered")
	}
	multi := `{"link_host":"l.example.com","ingresses":[{"name":"a","host":"a.example.com","public_key":"k","short_id":"s","uuids":"base"}],"clients":[{"id":"clash"}]}`
	if c, err := ParseIngressConfig([]byte(multi)); err != nil || c.Clients[0].ID != "clash" {
		t.Fatalf("list shape: %v %+v", err, c.Clients)
	}
	bad := map[string]string{
		`"clients":{}`:              "array",
		`"clients":[]`:              "at least one",
		`"clients":[{"id":"nope"}]`: "id must be one of",
		`"clients":[{"id":"karing"},{"id":"karing"}]`:                          "twice",
		`"clients":[{"id":"karing","x":1}]`:                                    "unknown key",
		`"clients":[{"id":"karing","install":{"tv":""}}]`:                      "platform must be one of",
		`"clients":[{"id":"karing","install":{"ios":"http://a.example.com"}}]`: "https URL",
		`"clients":[{"id":"karing","install":[]}]`:                             "platform to URL",
	}
	for frag, want := range bad {
		_, err := ParseIngressConfig([]byte(base + "," + frag + "}"))
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want %q", frag, err, want)
		}
	}
}

func TestStashProfileDialect(t *testing.T) {
	cfg, d := hy2Cfg(), multiDevice()
	out, err := RenderStashProfile(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Proxies []map[string]any `json:"proxies"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatal(err)
	}
	var vless, hy2 map[string]any
	for _, x := range p.Proxies {
		switch x["name"] {
		case "auto-ingress-1":
			vless = x
		case "auto-ingress-1-hy2":
			hy2 = x
		}
	}
	uuid, _ := cfg.UUIDFor(cfg.Ingresses[0], "auto", d.UUIDs["auto"])
	if vless["sni"] != cfg.Ingresses[0].SNI || vless["servername"] != nil {
		t.Errorf("vless: %v", vless)
	}
	if hy2["auth"] != uuid || hy2["password"] != nil || hy2["obfs"] != "salamander" || hy2["obfs-password"] != "obfs-fake" {
		t.Errorf("hy2: %v", hy2)
	}
	clash, _ := RenderClashProfile(cfg, d)
	if strings.Contains(string(clash), `"auth"`) || !strings.Contains(string(clash), `"servername"`) {
		t.Error("the Clash profile changed")
	}
}

func TestURIList(t *testing.T) {
	cfg, d := hy2Cfg(), multiDevice()
	out, err := RenderURIList(cfg, d)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.StdEncoding.DecodeString(string(out))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	// three modes x (two VLESS + one Hysteria 2)
	if len(lines) != 9 {
		t.Fatalf("%d lines: %s", len(lines), raw)
	}
	in := cfg.Ingresses[0]
	uuid, _ := cfg.UUIDFor(in, "auto", d.UUIDs["auto"])
	var sawVless, sawHy2 bool
	for _, l := range lines {
		u, err := url.Parse(l)
		if err != nil {
			t.Fatal(err)
		}
		switch u.Fragment {
		case "auto-ingress-1":
			q := u.Query()
			sawVless = u.Scheme == "vless" && u.User.Username() == uuid && u.Host == in.Host+":443" &&
				q.Get("security") == "reality" && q.Get("pbk") == in.PublicKey && q.Get("sid") == in.ShortID &&
				q.Get("sni") == in.SNI && q.Get("flow") == "xtls-rprx-vision" && q.Get("fp") == "chrome"
		case "auto-ingress-1-hy2":
			q := u.Query()
			sawHy2 = u.Scheme == "hysteria2" && u.User.Username() == uuid && u.Host == in.Host+":8443" &&
				q.Get("sni") == in.Host && q.Get("obfs") == "salamander" && q.Get("obfs-password") == "obfs-fake"
		}
		if strings.Contains(l, d.Name) || strings.Contains(l, d.Owner) {
			t.Error("a link carries the device name or owner")
		}
	}
	if !sawVless || !sawHy2 {
		t.Fatalf("links: %s", raw)
	}
}

func TestSubscriptionFormatPath(t *testing.T) {
	f := newPubFx(t)
	for format, ctype := range map[string]string{"sing-box": "application/json", "clash": "text/yaml; charset=utf-8",
		"stash": "text/yaml; charset=utf-8", "uri": "text/plain; charset=utf-8"} {
		rec := f.get("/sub/"+f.secret+"/"+format, "app")
		if rec.Code != 200 || rec.Header().Get("Content-Type") != ctype || rec.Body.Len() == 0 {
			t.Errorf("%s: %d %q", format, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
	plain := f.get("/sub/"+f.secret, "app")
	if plain.Code != 200 || plain.Header().Get("Content-Type") != "application/json" {
		t.Errorf("plain: %d", plain.Code)
	}
	for _, p := range []string{"/sub/" + f.secret + "/nope", "/sub/" + f.secret + "/", "/sub/" + f.secret + "/uri/x", "/sub/nope/uri"} {
		if rec := f.get(p, "app"); rec.Code != 404 || rec.Body.Len() != 0 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := f.get("/sub/"+f.secret+"/uri", "TelegramBot"); rec.Code != 404 {
		t.Error("a preview saw a format")
	}
}

func TestNewClientsGetLandingPage(t *testing.T) {
	f := newPubFx(t)
	for _, c := range []string{"stash", "shadowrocket"} {
		d, _ := f.newDevice("x", "y", c)
		rec := f.get("/e/"+f.token(d), "Mozilla/5.0")
		def, _ := ClientByID(c)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Open in "+def.Label) || !strings.Contains(rec.Body.String(), c+"://") {
			t.Errorf("%s: %d %s", c, rec.Code, rec.Body.String())
		}
	}
}
