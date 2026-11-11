package enroll

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCatalogLinks(t *testing.T) {
	sub := "https://link.example.com/sub/abc/uri"
	cases := map[string]string{
		"sing-box":     "sing-box://import-remote-profile?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi#tunnel",
		"karing":       "karing://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"clash":        "clash://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"stash":        "stash://install-config?url=https%3A%2F%2Flink.example.com%2Fsub%2Fabc%2Furi&name=tunnel",
		"shadowrocket": "shadowrocket://add/sub://" + base64.StdEncoding.EncodeToString([]byte(sub)) + "?remark=tunnel",
		"happ":         "happ://add/https://link.example.com/sub/abc/uri",
		"throne":       "throne://addsub/" + base64.StdEncoding.EncodeToString([]byte(sub+"#tunnel")),
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
	if clientsRepr != "('sing-box', 'karing', 'clash', 'shadowrocket', 'stash', 'happ', 'throne')" {
		t.Errorf("clientsRepr %s", clientsRepr)
	}
}

func TestFormatOfStoredClient(t *testing.T) {
	for client, want := range map[string]string{"sing-box": FormatSingBox, "karing": FormatSingBox, "clash": FormatClash,
		"stash": FormatStash, "shadowrocket": FormatURIList, "happ": FormatURIList, "throne": FormatURIList, "": FormatSingBox, "other": FormatSingBox} {
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
	for _, c := range []string{"stash", "shadowrocket", "happ", "throne"} {
		d, _ := f.newDevice("x", "y", c)
		rec := f.get("/e/"+f.token(d), "Mozilla/5.0")
		def, _ := ClientByID(c)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Open in "+def.Label) || !strings.Contains(rec.Body.String(), c+"://") {
			t.Errorf("%s: %d %s", c, rec.Code, rec.Body.String())
		}
	}
}

// Happ takes the subscription URL as is (#278): only an https URL of URL-safe characters gets a
// link, so no quote, space or angle bracket can reach the href.
func TestHappLinkRefusesUnsafeURL(t *testing.T) {
	for _, u := range []string{"http://link.example.com/sub/abc/uri", "https://x/\"><script>",
		"https://x/a b", "https://x/a?b=c&d", "javascript:alert(1)"} {
		if got := happLink(u, "n"); got != "" {
			t.Errorf("%q: got %q", u, got)
		}
	}
	if got := happLink("https://link.example.com/sub/0a1b/uri", ""); got != "happ://add/https://link.example.com/sub/0a1b/uri" {
		t.Errorf("got %q", got)
	}
}

// Throne (#295): standard Base64 (Throne refuses the URL-safe form) of "<url>#<name>"; the name has
// no line breaks and is at most 64 characters; no name gives the URL alone.
func TestThroneLink(t *testing.T) {
	decode := func(link string) string {
		t.Helper()
		p, ok := strings.CutPrefix(link, "throne://addsub/")
		if !ok {
			t.Fatalf("prefix: %q", link)
		}
		if strings.ContainsAny(p, "-_") {
			t.Errorf("URL-safe Base64 in %q", p)
		}
		b, err := base64.StdEncoding.DecodeString(p)
		if err != nil {
			t.Fatalf("%q: %v", p, err)
		}
		return string(b)
	}
	// "?>>" in the URL gives "+" and "/" in standard Base64: they must stay.
	sub := "https://l.example.com/sub/a?>>/uri"
	if got := decode(throneLink(sub, "My  VPN\nname")); got != sub+"#My VPN name" {
		t.Errorf("got %q", got)
	}
	if p := strings.TrimPrefix(throneLink(sub, "x"), "throne://addsub/"); !strings.ContainsAny(p, "+/") {
		t.Errorf("want standard Base64 with + or /: %q", p)
	}
	if got := decode(throneLink(sub, "")); got != sub {
		t.Errorf("no name: %q", got)
	}
	long := strings.Repeat("я", 100)
	if got := decode(throneLink(sub, long)); got != sub+"#"+strings.Repeat("я", 64) {
		t.Errorf("long name: %d bytes", len(got))
	}
	def, _ := ClientByID("throne")
	if def.RunsOn(PlatformIOS) || !def.RunsOn(PlatformAndroid) || !def.RunsOn(PlatformMacOS) || def.OnPlay || def.AppStoreID != "" {
		t.Errorf("platforms: %+v", def)
	}
}

// The picker shows Throne on Android with the app check, and never on an iPhone (#295).
func TestThroneOnThePicker(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	cfg := adminCfg
	cfg.Clients = []ClientChoice{{ID: "throne"}, {ID: "happ"}}
	(&SelfWeb{self: f.self, link: func() IngressConfig { return cfg }}).Register(mux)
	_ = f.self.users.RecordLogin("sub-1", "anna@example.test", true, "")
	sid, _, _ := f.self.users.NewSession("sub-1", time.Hour)
	d, _, _ := f.store.CreateDevice("Phone", "oidc:sub-1", "sing-box", adminFuture)
	get := func(ua string) string {
		req, _ := http.NewRequest("GET", srv.URL+"/me/devices/"+strconv.FormatInt(d.ID, 10), nil)
		req.Header.Set("User-Agent", ua)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	android := get("Mozilla/5.0 (Linux; Android 16; Pixel 9) AppleWebKit/537.36 Chrome/140 Mobile")
	if !strings.Contains(android, "<span>Throne</span>") || !strings.Contains(android, "package=com.nb4a.throne") ||
		!strings.Contains(android, "throne://addsub/") {
		t.Errorf("android: no Throne intent link")
	}
	iphone := get("Mozilla/5.0 (iPhone; CPU iPhone OS 26_1 like Mac OS X) AppleWebKit/605.1.15 Mobile/15E148 Safari/604.1")
	if !strings.Contains(android, "intent://") {
		t.Error("android: want an intent link")
	}
	if strings.Contains(iphone, "throne://") || strings.Contains(iphone, "<span>Throne</span>") ||
		!strings.Contains(iphone, "<span>Happ</span>") {
		t.Errorf("iphone: Throne must not show")
	}
}
