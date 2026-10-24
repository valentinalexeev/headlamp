package enroll

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"html"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var publicCfg = IngressCfg{Host: "ingress.example.com", PublicKey: "PUBKEY", ShortID: "ab12", SNI: "www.example.com", Port: 443, ProfileName: "tunnel"}

type pubFx struct {
	t      *testing.T
	store  *Store
	clock  *fakeClock
	mux    *http.ServeMux
	device Device
	secret string
}

func newPubFx(t *testing.T) *pubFx {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s, err := OpenStore(t.TempDir()+"/e.db", testKey, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	dev, secret, err := s.CreateDevice("phone", "alice", "sing-box", 1_000_000+10*86400)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	NewPublic(s, publicCfg).Register(mux)
	return &pubFx{t: t, store: s, clock: clock, mux: mux, device: dev, secret: secret}
}

func (f *pubFx) token(d Device) string {
	f.t.Helper()
	tok, err := f.store.NewLink(d.ID, time.Hour, 1)
	if err != nil {
		f.t.Fatal(err)
	}
	return tok
}

func (f *pubFx) newDevice(name, owner, client string) (Device, string) {
	f.t.Helper()
	d, sec, err := f.store.CreateDevice(name, owner, client, 1_000_000+86400)
	if err != nil {
		f.t.Fatal(err)
	}
	return d, sec
}

func (f *pubFx) get(path string, ua ...string) *httptest.ResponseRecorder {
	return f.req("GET", path, ua...)
}

// req sends a request; with no ua argument the request has no User-Agent header at all.
func (f *pubFx) req(method, path string, ua ...string) *httptest.ResponseRecorder {
	f.t.Helper()
	r := httptest.NewRequest(method, path, nil)
	if len(ua) > 0 {
		r.Header.Set("User-Agent", ua[0])
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, r)
	return rec
}

var hrefRe = regexp.MustCompile(`href="([^"]+)"`)

// target is the deep link a client is sent to: the Location of a 302 or the landing page's href.
func target(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	if rec.Code == 302 {
		return rec.Header().Get("Location")
	}
	m := hrefRe.FindStringSubmatch(rec.Body.String())
	if m == nil {
		t.Fatalf("no link in %d %q", rec.Code, rec.Body)
	}
	return html.UnescapeString(m[1])
}

func wantDeepLink(t *testing.T, client, secret string) string {
	t.Helper()
	l, err := DeepLinkFor(client, SubURL(publicCfg, secret), "tunnel")
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func assertEmpty404(t *testing.T, rec *httptest.ResponseRecorder, base *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code != 404 || rec.Body.Len() != 0 {
		t.Fatalf("want empty 404, got %d %q", rec.Code, rec.Body)
	}
	if base != nil && !headersEqual(rec.Header(), base.Header()) {
		t.Fatalf("headers differ: %v vs %v", rec.Header(), base.Header())
	}
}

func headersEqual(a, b http.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if strings.Join(v, ",") != strings.Join(b[k], ",") {
			return false
		}
	}
	return true
}

func TestKaringGetsLandingPageWithButton(t *testing.T) {
	f := newPubFx(t)
	d, secret := f.newDevice("Anna", "anna.owner", "karing")
	tok := f.token(d)
	rec := f.get("/e/" + tok)
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("%v", rec.Header())
	}
	if rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("%v", rec.Header())
	}
	if got := target(t, rec); got != wantDeepLink(t, "karing", secret) {
		t.Fatalf("%s", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Open in Karing") || strings.Contains(body, "<script") {
		t.Fatal(body)
	}
	assertEmpty404(t, f.get("/e/"+tok), nil)
}

func TestClashGetsLandingPageAndYAMLProfile(t *testing.T) {
	f := newPubFx(t)
	d, secret := f.newDevice("Anna", "anna.owner", "clash")
	rec := f.get("/e/" + f.token(d))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "Open in Clash Mi") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := target(t, rec); got != wantDeepLink(t, "clash", secret) {
		t.Fatalf("%s", got)
	}
	p := f.get("/sub/" + secret)
	if p.Code != 200 || !strings.HasPrefix(p.Header().Get("Content-Type"), "text/yaml") {
		t.Fatalf("%d %v", p.Code, p.Header())
	}
	var doc struct {
		Proxies []struct{ Name string } `json:"proxies"`
	}
	if err := json.Unmarshal(p.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, x := range doc.Proxies {
		names = append(names, x.Name)
	}
	if strings.Join(names, ",") != "auto,direct,exit" {
		t.Fatal(names)
	}
	if strings.Contains(p.Body.String(), "Anna") {
		t.Fatal("device name leaked")
	}
}

func TestValidTokenRedirectsOnce(t *testing.T) {
	f := newPubFx(t)
	tok := f.token(f.device)
	rec := f.get("/e/" + tok)
	if rec.Code != 302 || rec.Header().Get("Location") != wantDeepLink(t, "sing-box", f.secret) {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("302 has a body: %q", rec.Body)
	}
	assertEmpty404(t, f.get("/e/"+tok), nil)
}

func TestDeepLinkFollowsTheDevicesClient(t *testing.T) {
	f := newPubFx(t)
	for _, client := range []string{"sing-box", "karing", "clash"} {
		t.Run(client, func(t *testing.T) {
			d, secret := f.newDevice("n", "o", client)
			got := target(t, f.get("/e/"+f.token(d)))
			if got != wantDeepLink(t, client, secret) {
				t.Fatalf("%s", got)
			}
			if !strings.HasPrefix(got, client+"://") {
				t.Fatal(got)
			}
		})
	}
}

func TestDeviceNameAndOwnerNeverReachTheIngressHost(t *testing.T) {
	f := newPubFx(t)
	for _, client := range []string{"sing-box", "karing", "clash"} {
		t.Run(client, func(t *testing.T) {
			d, secret := f.newDevice("Anna Secret Phone", "anna.owner", client)
			loc := target(t, f.get("/e/"+f.token(d)))
			prof := f.get("/sub/" + secret).Body.String()
			for _, leaked := range []string{"Anna", "Secret", "anna.owner", "anna"} {
				if strings.Contains(loc, leaked) || strings.Contains(prof, leaked) {
					t.Fatalf("%q leaked", leaked)
				}
			}
			if !strings.HasSuffix(loc, "#tunnel") && !strings.HasSuffix(loc, "&name=tunnel") {
				t.Fatal(loc)
			}
		})
	}
	t.Run("configurable label", func(t *testing.T) {
		cfg := publicCfg
		cfg.ProfileName = "work vpn"
		mux := http.NewServeMux()
		NewPublic(f.store, cfg).Register(mux)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/e/"+f.token(f.device), nil))
		if loc := rec.Header().Get("Location"); !strings.HasSuffix(loc, "#work%20vpn") {
			t.Fatal(loc)
		}
	})
}

var previewUAs = []string{
	"facebookexternalhit/1.1 (+http://www.facebook.com/externalhit_uatext.php)",
	"Twitterbot/1.0",
	"TelegramBot (like TwitterBot)",
	"WhatsApp/2.23.20.0 A",
	"Slackbot-LinkExpanding 1.0 (+https://api.slack.com/robots)",
	"Mozilla/5.0 (compatible; Discordbot/2.0; +https://discordapp.com)",
	"Mozilla/5.0 (Windows NT 6.1; WOW64) SkypeUriPreview Preview/0.5",
	"LinkedInBot/1.0 (compatible; Mozilla/5.0; Apache-HttpClient +http://www.linkedin.com)",
	"Mozilla/5.0 (compatible; Embedly/0.2; +http://support.embed.ly/)",
	"Mozilla/5.0 (compatible; Googlebot/2.1; +http://www.google.com/bot.html)",
	"Mozilla/5.0 (compatible; SomeCrawler/1.0)",
	"Mozilla/5.0 (compatible; Yeti-Spider)",
	"vkShare; +http://vk.com/dev/Share",
	"Viber/20.0 (iOS) link preview",
	"Snap URL Preview Service; bot; snapchat",
	"Google-PageRenderer Google (+https://developers.google.com/+/web/snippet/)",
	"Mozilla/5.0 something LinkExpanding",
	"FACEBOOKEXTERNALHIT/1.1",
}

const safariIOS = "Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile/15E148 Safari/604.1"

func TestBotUserAgentGets404AndKeepsToken(t *testing.T) {
	f := newPubFx(t)
	tok := f.token(f.device) // max_uses 1
	base := f.get("/no/such/path", safariIOS)
	for i, ua := range previewUAs {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			assertEmpty404(t, f.get("/e/"+tok, ua), base)
		})
	}
	if rec := f.get("/e/"+tok, safariIOS); rec.Code != 302 {
		t.Fatalf("token was consumed by a preview: %d", rec.Code)
	}
	assertEmpty404(t, f.get("/e/"+tok, safariIOS), nil)
}

func TestEveryMarkerIsADenylistEntry(t *testing.T) {
	want := []string{"facebookexternalhit", "twitterbot", "telegrambot", "whatsapp", "slackbot", "discordbot",
		"skypeuripreview", "linkedinbot", "embedly", "bot", "crawler", "spider", "preview",
		"linkexpanding", "google-", "vkshare", "viber"}
	if strings.Join(PreviewUAMarkers, "|") != strings.Join(want, "|") {
		t.Fatalf("%v", PreviewUAMarkers)
	}
	f := newPubFx(t)
	for _, m := range want {
		t.Run(m, func(t *testing.T) {
			for _, ua := range []string{"x " + m + " y", "X " + strings.ToUpper(m) + " Y"} {
				if !isPreview(ua) {
					t.Fatalf("%q not treated as preview", ua)
				}
			}
			assertEmpty404(t, f.get("/e/"+f.token(f.device), "agent/"+m+"/1"), nil)
		})
	}
}

func TestRealClientsAreNotPreviews(t *testing.T) {
	f := newPubFx(t)
	for i, ua := range []string{
		safariIOS,
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Mobile Safari/537.36",
		"SFI/1.11.4 (Build 1; sing-box 1.11.4; language en_US)",
		"karing/1.1.2.606 (iOS 17.5; iPhone15,2)",
		"sing-box 1.11.4",
	} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			if rec := f.get("/e/"+f.token(f.device), ua); rec.Code != 302 {
				t.Fatalf("%d", rec.Code)
			}
		})
	}
}

func TestEmptyUserAgentIsNormalClient(t *testing.T) {
	f := newPubFx(t)
	t.Run("empty header", func(t *testing.T) {
		if rec := f.get("/e/"+f.token(f.device), ""); rec.Code != 302 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("no header", func(t *testing.T) {
		if rec := f.get("/e/" + f.token(f.device)); rec.Code != 302 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("sub without header", func(t *testing.T) {
		if rec := f.get("/sub/" + f.secret); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestPreviewsNeverSeeTheSubscription(t *testing.T) {
	f := newPubFx(t)
	for i, ua := range previewUAs {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			assertEmpty404(t, f.get("/sub/"+f.secret, ua), nil)
		})
	}
}

func TestRotatedSecretInvalidatesOldSubURL(t *testing.T) {
	f := newPubFx(t)
	old := f.secret
	if err := f.store.RotateSecret(f.device.ID); err != nil {
		t.Fatal(err)
	}
	assertEmpty404(t, f.get("/sub/"+old), nil)
	cur, _ := f.store.SubscriptionSecret(f.device.ID)
	if rec := f.get("/sub/" + cur); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	loc := f.get("/e/" + f.token(f.device)).Header().Get("Location")
	if !strings.Contains(loc, cur) || strings.Contains(loc, old) {
		t.Fatal(loc)
	}
}

func TestBadTokensAreIndistinguishableFromAnyOther404(t *testing.T) {
	f := newPubFx(t)
	expired, _ := f.store.NewLink(f.device.ID, 10*time.Second, 1)
	f.clock.Advance(100 * time.Second)
	base := f.get("/no/such/path")
	assertEmpty404(t, base, nil)
	for _, p := range []string{"/e/unknown", "/e/" + expired} {
		assertEmpty404(t, f.get(p), base)
	}
}

func TestSubServesProfileThen404AfterRevoke(t *testing.T) {
	f := newPubFx(t)
	rec := f.get("/sub/" + f.secret)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "application/json" || rec.Header().Get("Profile-Update-Interval") != "12" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	var doc struct {
		Outbounds []struct{ Type string } `json:"outbounds"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range doc.Outbounds {
		if o.Type == "vless" {
			n++
		}
	}
	if n != 3 {
		t.Fatalf("%d vless outbounds", n)
	}
	if err := f.store.Revoke(f.device.ID); err != nil {
		t.Fatal(err)
	}
	assertEmpty404(t, f.get("/sub/"+f.secret), nil)
}

func TestUnknownSecretIs404(t *testing.T) {
	f := newPubFx(t)
	assertEmpty404(t, f.get("/sub/nope"), nil)
}

func TestPublicUnknownPathEmpty404(t *testing.T) {
	f := newPubFx(t)
	base := f.get("/no/such/path")
	assertEmpty404(t, base, nil)
	t.Run("oversized ids and trailing slashes", func(t *testing.T) {
		for _, p := range []string{"/sub/" + strings.Repeat("9", 25) + ".x", "/sub/" + strings.Repeat("9", 5000) + ".x",
			"/e/x/", "/sub/x/", "/e/", "/sub/", "/e", "/sub", "/sub/" + f.secret + "/", "/e/a/b"} {
			assertEmpty404(t, f.get(p), base)
		}
	})
	t.Run("nothing else is exposed", func(t *testing.T) {
		for _, p := range []string{"/docs", "/redoc", "/openapi.json", "/", "/healthz", "/nodes", "/enroll", "/enroll/devices", "/metrics", "/debug/pprof/"} {
			assertEmpty404(t, f.get(p), base)
		}
	})
	t.Run("other methods", func(t *testing.T) {
		tok := f.token(f.device)
		for i, c := range []struct{ method, path string }{
			{"POST", "/e/x"}, {"POST", "/e/" + tok}, {"PUT", "/sub/" + f.secret},
			{"DELETE", "/"}, {"HEAD", "/e/x"}, {"HEAD", "/e/" + tok}, {"PATCH", "/sub/" + f.secret},
			{"OPTIONS", "/e/" + tok}, {"HEAD", "/sub/" + f.secret},
		} {
			t.Run(strconv.Itoa(i)+" "+c.method, func(t *testing.T) {
				assertEmpty404(t, f.req(c.method, c.path, safariIOS), base)
			})
		}
		if rec := f.get("/e/" + tok); rec.Code != 302 {
			t.Fatalf("a non-GET consumed the token: %d", rec.Code)
		}
	})
	t.Run("no cookies", func(t *testing.T) {
		rec := f.get("/e/" + f.token(f.device))
		if rec.Header().Get("Set-Cookie") != "" {
			t.Fatal("cookie set")
		}
	})
}

func TestHandlersNeverLogPathsTokensOrSecrets(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	f := newPubFx(t)
	d, secret := f.newDevice("Anna", "anna", "clash")
	tok := f.token(d)
	tok2 := f.token(f.device)
	f.get("/e/" + tok)
	f.get("/e/"+tok2, "TelegramBot")
	f.get("/e/" + tok) // used
	f.get("/sub/" + secret)
	f.get("/sub/"+secret, "WhatsApp")
	f.get("/sub/" + secret + "x")
	f.get("/e/never-issued-token")
	f.req("POST", "/e/"+tok2)
	f.store.Close() // store errors are the other thing handlers log about
	f.get("/e/" + tok2)
	f.get("/sub/" + secret)

	out := buf.String()
	for _, secretish := range []string{tok, tok2, secret, strings.Split(secret, ".")[1], "never-issued-token", "/e/", "/sub/"} {
		if strings.Contains(out, secretish) {
			t.Fatalf("log output contains %q:\n%s", secretish, out)
		}
	}
}

func TestStoreFailureIs500WithoutDetails(t *testing.T) {
	f := newPubFx(t)
	tok := f.token(f.device)
	f.store.Close()
	for _, p := range []string{"/e/" + tok, "/sub/" + f.secret} {
		rec := f.get(p)
		if rec.Code != 500 || rec.Body.Len() != 0 {
			t.Fatalf("%d %q", rec.Code, rec.Body)
		}
	}
}

// goldenStore serves fixed devices so the handler output can be compared with the Python app's.
type goldenStore struct{ devs map[string]*Device }

var goldenUUIDs = map[string]string{
	"auto":   "11111111-1111-4111-8111-111111111111",
	"direct": "22222222-2222-4222-8222-222222222222",
	"exit":   "33333333-3333-4333-8333-333333333333",
}

func newGoldenStore() *goldenStore {
	g := &goldenStore{devs: map[string]*Device{}}
	for i, c := range []string{"sing-box", "karing", "clash"} {
		g.devs[c] = &Device{ID: int64(i + 1), Name: "Anna", Owner: "anna", Client: c, UUIDs: goldenUUIDs}
	}
	return g
}

func (g *goldenStore) ConsumeToken(t string) (*Device, error) { return g.devs[t], nil }
func (g *goldenStore) SubscriptionSecret(id int64) (string, error) {
	return strconv.FormatInt(id, 10) + ".0123456789abcdef", nil
}
func (g *goldenStore) VerifySecret(s string) (*Device, error) {
	for _, d := range g.devs {
		if sec, _ := g.SubscriptionSecret(d.ID); sec == s {
			return d, nil
		}
	}
	return nil, nil
}

func TestResponsesMatchPythonGoldens(t *testing.T) {
	raw, err := os.ReadFile("testdata/public_golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    string            `json:"body"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	cfg := IngressCfg{Host: "ingress.example.test", PublicKey: "PUBKEY", ShortID: "ab12", SNI: "www.example.test", Port: 443, ProfileName: "tunnel"}
	gs := newGoldenStore()
	mux := http.NewServeMux()
	newPublic(gs, cfg).Register(mux)
	paths := map[string]string{"not_found": "/e/nope"}
	for _, c := range []string{"sing-box", "karing", "clash"} {
		sec, _ := gs.SubscriptionSecret(gs.devs[c].ID)
		paths["enrol_"+c] = "/e/" + c
		paths["sub_"+c] = "/sub/" + sec
	}
	if len(golden) != len(paths) {
		t.Fatalf("golden has %d cases, test %d", len(golden), len(paths))
	}
	for name, path := range paths {
		t.Run(name, func(t *testing.T) {
			g := golden[name]
			srv := httptest.NewServer(mux)
			defer srv.Close()
			hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			res, err := hc.Get(srv.URL + path)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			var body bytes.Buffer
			body.ReadFrom(res.Body)
			if res.StatusCode != g.Status {
				t.Fatalf("status %d want %d", res.StatusCode, g.Status)
			}
			want, _ := base64.StdEncoding.DecodeString(g.Body)
			if !bytes.Equal(body.Bytes(), want) {
				t.Fatalf("body differs:\n got %q\nwant %q", body.Bytes(), want)
			}
			if len(res.Header) == 0 {
				t.Fatal("no headers")
			}
			got := map[string]string{}
			for k := range res.Header {
				if k != "Date" {
					got[strings.ToLower(k)] = res.Header.Get(k)
				}
			}
			if len(got) != len(g.Headers) {
				t.Fatalf("headers %v want %v", got, g.Headers)
			}
			for k, v := range g.Headers {
				if got[k] != v {
					t.Fatalf("header %s = %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestRenderErrorIsEmpty500(t *testing.T) {
	gs := newGoldenStore()
	gs.devs["sing-box"].UUIDs = map[string]string{}
	mux := http.NewServeMux()
	newPublic(gs, publicCfg).Register(mux)
	sec, _ := gs.SubscriptionSecret(1)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/sub/"+sec, nil))
	if rec.Code != 500 || rec.Body.Len() != 0 {
		t.Fatalf("%d %q", rec.Code, rec.Body)
	}
}
