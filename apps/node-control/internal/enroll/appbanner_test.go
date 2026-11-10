package enroll

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const iphoneUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X)"

// appPageFx serves the self pages with a given link config and returns a getter as the signed-in
// owner of one active device.
func appPageFx(t *testing.T, cfg IngressConfig) (*selfFx, Device, func(path, ua string) (int, string)) {
	t.Helper()
	f := newSelfFx(t, SelfConfig{})
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	(&SelfWeb{self: f.self, link: func() IngressConfig { return cfg }}).Register(mux)
	_ = f.self.users.RecordLogin("sub-1", "anna@example.test", true, "")
	sid, _, _ := f.self.users.NewSession("sub-1", time.Hour)
	d, _, _ := f.store.CreateDevice("Phone", "oidc:sub-1", "sing-box", adminFuture)
	get := func(path, ua string) (int, string) {
		req, _ := http.NewRequest("GET", srv.URL+path, nil)
		req.Header.Set("User-Agent", ua)
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
		c := *srv.Client()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, readAll(resp)
	}
	return f, d, get
}

func TestAppPageHasTheSmartAppBanner(t *testing.T) {
	_, d, get := appPageFx(t, adminCfg)
	id := strconv.FormatInt(d.ID, 10)
	code, p := get("/me/devices/"+id+"/app/clash", iphoneUA)
	if code != 200 {
		t.Fatalf("%d %s", code, p)
	}
	for _, want := range []string{
		`<meta name="apple-itunes-app" content="app-id=6744321968, app-argument=clash://install-config?url=https%3A%2F%2F`,
		`%2Fclash&amp;name=`, `href="clash://install-config?url=`, `href="https://apps.apple.com/app/id6744321968"`,
		"Add Phone to Clash Mi",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q in %s", want, p)
		}
	}
	if strings.Contains(p, "ZgotmplZ") {
		t.Error("the template blanked a link")
	}
	// the banner argument is the import link of the app's own format
	if _, p := get("/me/devices/"+id+"/app/shadowrocket", iphoneUA); !strings.Contains(p, "app-id=932747118, app-argument=shadowrocket://add/sub://") {
		t.Errorf("shadowrocket: %s", p)
	}
}

func TestAppPageRefusals(t *testing.T) {
	cfg := adminCfg
	cfg.Clients = []ClientChoice{{ID: "karing"}}
	f, d, get := appPageFx(t, cfg)
	id := strconv.FormatInt(d.ID, 10)
	other, _, _ := f.store.CreateDevice("theirs", "admin-made", "sing-box", adminFuture)
	for _, path := range []string{
		"/me/devices/" + id + "/app/clash",                               // not offered by the site
		"/me/devices/" + id + "/app/nope",                                // not in the catalog
		"/me/devices/" + strconv.FormatInt(other.ID, 10) + "/app/karing", // someone else's device
		"/me/devices/999/app/karing",                                     // no such device
	} {
		if code, p := get(path, iphoneUA); code != 404 || strings.Contains(p, "apple-itunes-app") {
			t.Errorf("%s: %d", path, code)
		}
	}
	_ = f.store.Revoke(d.ID)
	if code, _ := get("/me/devices/"+id+"/app/karing", iphoneUA); code != 404 {
		t.Error("a revoked device has an app page")
	}
}

func TestSiteStoreIDAndLinkWin(t *testing.T) {
	cfg := adminCfg
	cfg.Clients = []ClientChoice{
		{ID: "stash", AppStoreID: "123", Install: map[string]string{"ios": "https://apps.example.com/stash"}},
		{ID: "karing", Install: map[string]string{"android": "https://store.example.com/karing"}},
	}
	_, d, get := appPageFx(t, cfg)
	id := strconv.FormatInt(d.ID, 10)
	_, p := get("/me/devices/"+id+"/app/stash", iphoneUA)
	if !strings.Contains(p, "app-id=123, ") || !strings.Contains(p, `href="https://apps.example.com/stash"`) {
		t.Errorf("site values not used: %s", p)
	}
	// the picker: iPhone buttons go to the app pages, with the App Store page as the default iOS link
	_, p = get("/me/devices/"+id, iphoneUA)
	if !strings.Contains(p, `href="/me/devices/`+id+`/app/stash"`) || !strings.Contains(p, `href="https://apps.apple.com/app/id6472431552"`) {
		t.Errorf("picker on iPhone: %s", p)
	}
	// Android keeps the direct import link and the site's store link only
	_, p = get("/me/devices/"+id, "Mozilla/5.0 (Linux; Android 16)")
	if !strings.Contains(p, `href="karing://install-config?url=`) || strings.Contains(p, "/app/karing") ||
		!strings.Contains(p, `href="https://store.example.com/karing"`) {
		t.Errorf("picker on Android: %s", p)
	}
	// a Mac gets direct links too: Safari on a Mac has no app banner
	if _, p = get("/me/devices/"+id, "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7)"); strings.Contains(p, "/app/") {
		t.Errorf("picker on a Mac: %s", p)
	}
}

func TestParseAppStoreID(t *testing.T) {
	base := `{"host":"r.example.com","public_key":"k","short_id":"s","sni":"x.example.com","clients":[`
	cfg, err := ParseIngressConfig([]byte(base + `{"id":"clash","app_store_id":"42"}]}`))
	if err != nil || cfg.Clients[0].StoreID() != "42" {
		t.Fatalf("%v %+v", err, cfg.Clients)
	}
	for _, bad := range []string{`"x1"`, `""`, `42`, `"1234567890123"`} {
		if _, err := ParseIngressConfig([]byte(base + `{"id":"clash","app_store_id":` + bad + `}]}`)); err == nil ||
			!strings.Contains(err.Error(), "app_store_id") {
			t.Errorf("%s: %v", bad, err)
		}
	}
	for _, c := range Catalog {
		if !appStoreIDRe.MatchString(c.AppStoreID) {
			t.Errorf("%s: catalog App Store id %q", c.ID, c.AppStoreID)
		}
	}
}
