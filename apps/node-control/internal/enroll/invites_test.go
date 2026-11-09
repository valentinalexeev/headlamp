package enroll

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// inviteFx is a fixture whose issuer says the email is verified, as invitations need by default.
func inviteFx(t *testing.T, cfg SelfConfig) *selfFx {
	f := newSelfFx(t, cfg)
	f.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = true }
	return f
}

// adminDo calls the invitation API of the fixture's admin.
func (f *selfFx) adminDo(method, path, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	mux := core.NewMux()
	f.admin.registerInvites(mux)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// newInvite makes an invitation through the API and returns its id and the path of its link.
func (f *selfFx) newInvite(body string) (int64, string) {
	f.t.Helper()
	rec := f.adminDo("POST", "/enroll/invites", body)
	var res struct {
		ID  int64  `json:"id"`
		URL string `json:"url"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &res) != nil {
		f.t.Fatalf("create invite: %d %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(res.URL)
	if err != nil || u.Scheme != "https" || u.Host != adminCfg.Link().Host || !strings.HasPrefix(u.Path, "/i/") {
		f.t.Fatalf("invite url %q", res.URL)
	}
	return res.ID, u.Path
}

// follow opens a path in the browser and follows the sign-in to the end.
func (f *selfFx) follow(c *http.Client, path string) (int, string) {
	f.t.Helper()
	resp, err := c.Get(f.srv.URL + path)
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, readAll(resp)
}

func (f *selfFx) invites() []Invite {
	f.t.Helper()
	invs, err := f.self.users.Invites(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return invs
}

func TestInviteRedeemEndToEnd(t *testing.T) {
	f := inviteFx(t, SelfConfig{InviteOnly: true})
	id, path := f.newInvite(`{"email":"Anna@Example.test","device_name":"Anna's phone"}`)
	c := f.browser()
	code, page := f.follow(c, path)
	if code != 200 || !strings.Contains(page, "is ready") || !strings.Contains(page, "hskey-0") || !strings.Contains(page, "Add Anna&#39;s phone to an app") {
		t.Fatalf("%d %s", code, page)
	}
	devs, _ := f.store.ListDevices()
	if len(devs) != 1 || devs[0].Owner != "oidc:sub-1" || devs[0].Name != "Anna's phone" || devs[0].Client != defaultSelfClient {
		t.Fatalf("%+v", devs)
	}
	inv := f.invites()[0]
	if inv.ID != id || inv.State != InviteUsed || inv.UsedBy != "oidc:sub-1" || inv.DeviceID != devs[0].ID || inv.Email != "anna@example.test" {
		t.Fatalf("%+v", inv)
	}
	if last := f.agent.last(); len(last.creds) != 3 {
		t.Errorf("the device was not pushed to the bridge: %+v", last)
	}
	// the cookie is gone and the link is spent
	for _, ck := range c.Jar.Cookies(mustURL(f.srv.URL)) {
		if ck.Name == inviteCookie {
			t.Error("the invitation cookie stayed")
		}
	}
	if code, page := f.follow(f.browser(), path); code != 404 || !strings.Contains(page, "not valid") {
		t.Errorf("second use: %d %s", code, page)
	}
	if n, _ := f.store.ListDevices(); len(n) != 1 {
		t.Error("a second device was made")
	}
	// invite-only: the form is gone and the POST is refused
	resp, _ := c.Get(f.srv.URL + "/me")
	p := readAll(resp)
	if strings.Contains(p, `action="/me/devices"`) || !strings.Contains(p, "ask an administrator for an invitation") {
		t.Errorf("invite-only page: %s", p)
	}
	if code, _ := f.enrol(c, csrfOf(t, p), "extra"); code != 403 {
		t.Errorf("invite-only enrol: %d", code)
	}
}

func TestInviteRefusalOtherEmail(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	_, path := f.newInvite(`{"email":"bob@example.test"}`)
	code, page := f.follow(f.browser(), path)
	if code != 409 || !strings.Contains(page, "for another account") {
		t.Fatalf("%d %s", code, page)
	}
	if devs, _ := f.store.ListDevices(); len(devs) != 0 {
		t.Error("a device was made")
	}
	if f.invites()[0].State != InvitePending {
		t.Error("the invitation was spent")
	}
}

func TestInviteRefusalUnverifiedEmail(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	f.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = false }
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	if code, page := f.follow(f.browser(), path); code != 409 || !strings.Contains(page, "does not confirm") {
		t.Fatalf("%d %s", code, page)
	}
	if f.invites()[0].State != InvitePending {
		t.Error("the invitation was spent")
	}
}

func TestInviteRefusalVerifiedAsAString(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	f.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = "true" }
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	if code, page := f.follow(f.browser(), path); code != 200 || !strings.Contains(page, "is ready") {
		t.Fatalf("%d %s", code, page)
	}
}

func TestInviteRefusalRevoked(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	id, path := f.newInvite(`{"email":"anna@example.test"}`)
	if rec := f.adminDo("POST", "/enroll/invites/"+strconv.FormatInt(id, 10)+"/revoke", `{}`); rec.Code != 200 {
		t.Fatalf("revoke %d %s", rec.Code, rec.Body)
	}
	if code, _ := f.follow(f.browser(), path); code != 404 {
		t.Errorf("revoked invitation: %d", code)
	}
	if f.invites()[0].State != InviteRevoked {
		t.Errorf("%+v", f.invites()[0])
	}
}

func TestInviteRefusalExpired(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	inv, tok, err := f.self.users.NewInvite(context.Background(), "anna@example.test", "x", "admin", -time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if code, _ := f.follow(f.browser(), "/i/"+tok); code != 404 {
		t.Errorf("expired: %d", code)
	}
	if f.invites()[0].ID != inv.ID || f.invites()[0].State != InviteExpired {
		t.Errorf("%+v", f.invites()[0])
	}
}

func TestInviteRefusalDeviceLimitKeepsTheInvitation(t *testing.T) {
	f := inviteFx(t, SelfConfig{MaxDevices: 1})
	c := f.browser()
	f.enrol(c, f.signIn(c), "first")
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	if code, page := f.follow(c, path); code != 409 || !strings.Contains(page, "limit") {
		t.Fatalf("%d %s", code, page)
	}
	if inv := f.invites()[0]; inv.State != InvitePending || inv.UsedBy != "" {
		t.Errorf("the invitation was not released: %+v", inv)
	}
}

func TestInviteRefusalPreviewAndUnknown(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
	req.Header.Set("User-Agent", "TelegramBot (like TwitterBot)")
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if b := readAll(resp); resp.StatusCode != 404 || b != "" {
		t.Errorf("preview: %d %q", resp.StatusCode, b)
	}
	if code, _ := f.follow(f.browser(), "/i/"+strings.Repeat("0", 64)); code != 404 {
		t.Errorf("unknown: %d", code)
	}
}

func TestInviteAPI(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	for body, want := range map[string]int{
		`{}`:                                          422,
		`{"email":"no-at"}`:                           422,
		`{"email":"a b@example.test"}`:                422,
		`{"email":"a@example.test","days":0}`:         422,
		`{"email":"a@example.test","days":91}`:        422,
		`{"email":"a@example.test","device_name":""}`: 422,
		`{"email":"a@example.test","days":30}`:        200,
	} {
		if rec := f.adminDo("POST", "/enroll/invites", body); rec.Code != want {
			t.Errorf("%s: %d %s", body, rec.Code, rec.Body)
		}
	}
	rec := f.adminDo("GET", "/enroll/invites", "")
	var res struct {
		Invites []Invite `json:"invites"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &res) != nil || len(res.Invites) != 1 || res.Invites[0].State != InvitePending ||
		res.Invites[0].DeviceName != "a" || res.Invites[0].ExpiresAt != adminNow+30*86400 {
		t.Fatalf("%s", rec.Body)
	}
	if strings.Contains(rec.Body.String(), "token") {
		t.Error("the list shows a token")
	}
	if rec := f.adminDo("POST", "/enroll/invites/999/revoke", `{}`); rec.Code != 404 {
		t.Errorf("unknown revoke: %d", rec.Code)
	}
	entries, _ := f.store.AuditLog(10)
	if len(entries) == 0 || entries[0].Action != "invite.create" {
		t.Errorf("audit: %+v", entries)
	}
}

func TestPickerPage(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	c := f.browser()
	csrf := f.signIn(c)
	f.enrol(c, csrf, "Phone")
	devs, _ := f.store.ListDevices()
	id := strconv.FormatInt(devs[0].ID, 10)
	get := func(path, ua string) (int, string) {
		req, _ := http.NewRequest("GET", f.srv.URL+path, nil)
		req.Header.Set("User-Agent", ua)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, readAll(resp)
	}
	const iphone = "Mozilla/5.0 (iPhone; CPU iPhone OS 19_0 like Mac OS X)"
	code, p := get("/me/devices/"+id, iphone)
	if code != 200 {
		t.Fatalf("%d %s", code, p)
	}
	for _, want := range []string{`href="shadowrocket://add/sub://`, `href="stash://install-config?url=`, `href="karing://install-config?url=`,
		`%2Fstash&amp;name=`, "/sub/", "The list shows the apps for iPhone and iPad"} {
		if !strings.Contains(p, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(p, "hskey-") || strings.Contains(p, "ZgotmplZ") {
		t.Error("the key shows again, or a link was blanked by the template")
	}
	// the page of an Android browser has no iOS-only apps
	if _, p := get("/me/devices/"+id, "Mozilla/5.0 (Linux; Android 16)"); strings.Contains(p, "shadowrocket://") || !strings.Contains(p, "karing://") ||
		!strings.Contains(p, "Show all apps") {
		t.Error("platform filter")
	}
	if _, p := get("/me/devices/"+id+"?all=1", "Mozilla/5.0 (Linux; Android 16)"); !strings.Contains(p, "shadowrocket://") || strings.Contains(p, "Show all apps") {
		t.Error("all=1")
	}
	// someone else's device, an unknown one, and a revoked one are not shown
	other, _, _ := f.store.CreateDevice("theirs", "admin-made", "sing-box", adminFuture)
	for _, path := range []string{"/me/devices/" + strconv.FormatInt(other.ID, 10), "/me/devices/999", "/me/devices/x"} {
		if code, p := get(path, iphone); code != 404 || strings.Contains(p, "/sub/") {
			t.Errorf("%s: %d", path, code)
		}
	}
	_ = f.store.Revoke(devs[0].ID)
	if code, _ := get("/me/devices/"+id, iphone); code != 404 {
		t.Error("revoked device")
	}
	// without a session, back to /me
	nc := *f.srv.Client()
	nc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	if resp, _ := nc.Get(f.srv.URL + "/me/devices/" + id); resp.StatusCode != 303 {
		t.Errorf("no session: %d", resp.StatusCode)
	}
}

func TestPickerUsesSiteChoicesAndIcons(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "stash.svg"), []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "unrelated.png"), []byte("x"), 0o600)
	icons, err := LoadIcons(dir)
	if err != nil || len(icons) != 1 {
		t.Fatalf("%v %v", icons, err)
	}
	cfg := adminCfg
	cfg.Clients = []ClientChoice{{ID: "stash", Install: map[string]string{"ios": "https://apps.example.com/stash"}}, {ID: "karing"}}
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	(&SelfWeb{self: f.self, rp: nil, link: func() IngressConfig { return cfg }, icons: icons}).Register(mux)
	sid, _, _ := f.self.users.NewSession("sub-1", time.Hour)
	_ = f.self.users.RecordLogin("sub-1", "anna@example.test", "")
	d, _, _ := f.store.CreateDevice("Phone", "oidc:sub-1", "sing-box", adminFuture)
	req, _ := http.NewRequest("GET", srv.URL+"/me/devices/"+strconv.FormatInt(d.ID, 10), nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (iPhone)")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: sid})
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	p := readAll(resp)
	if strings.Contains(p, "shadowrocket://") || strings.Index(p, "stash://") > strings.Index(p, "karing://") {
		t.Error("the site's choice and order are not kept")
	}
	if !strings.Contains(p, `<img src="/me/icons/stash"`) || !strings.Contains(p, `href="https://apps.example.com/stash"`) {
		t.Errorf("icon or store link missing: %s", p)
	}
	ir, _ := srv.Client().Get(srv.URL + "/me/icons/stash")
	if ir.StatusCode != 200 || ir.Header.Get("Content-Type") != "image/svg+xml" || !strings.Contains(ir.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("icon: %d %v", ir.StatusCode, ir.Header)
	}
	if r404, _ := srv.Client().Get(srv.URL + "/me/icons/karing"); r404.StatusCode != 404 {
		t.Error("an icon without a file")
	}
	big := t.TempDir()
	_ = os.WriteFile(filepath.Join(big, "karing.png"), make([]byte, maxIconBytes+1), 0o600)
	if _, err := LoadIcons(big); err == nil {
		t.Error("a large icon was accepted")
	}
	if _, err := LoadIcons(filepath.Join(dir, "missing")); err == nil {
		t.Error("a missing directory was accepted")
	}
}

func TestInviteOnlyConfig(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "c", "OIDC_DB": "/x", "OIDC_REDIRECT_URL": "https://links.example/oidc/callback"}
	for v, want := range map[string]bool{"": true, "true": true, "false": false, "0": false} {
		base["OIDC_INVITE_REQUIRE_VERIFIED"] = v
		if _, sc, _, err := selfConfig(core.MapEnv(base)); err != nil || sc.InviteAnyEmail == want {
			t.Errorf("require verified %q: %v %v", v, sc.InviteAnyEmail, err)
		}
	}
	base["OIDC_INVITE_REQUIRE_VERIFIED"] = "x"
	if _, _, _, err := selfConfig(core.MapEnv(base)); err == nil {
		t.Error("bad OIDC_INVITE_REQUIRE_VERIFIED accepted")
	}
	delete(base, "OIDC_INVITE_REQUIRE_VERIFIED")
	for v, want := range map[string]bool{"": false, "false": false, "true": true, "1": true} {
		base["OIDC_INVITE_ONLY"] = v
		if _, sc, _, err := selfConfig(core.MapEnv(base)); err != nil || sc.InviteOnly != want {
			t.Errorf("%q: %v %v", v, sc.InviteOnly, err)
		}
	}
	base["OIDC_INVITE_ONLY"] = "maybe"
	if _, _, _, err := selfConfig(core.MapEnv(base)); err == nil || !strings.Contains(err.Error(), "OIDC_INVITE_ONLY") {
		t.Errorf("bad value: %v", err)
	}
}

func TestInviteNeedsTheVerifiedClaimByDefault(t *testing.T) {
	f := newSelfFx(t, SelfConfig{}) // the issuer sends no email_verified
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	if code, page := f.follow(f.browser(), path); code != 409 || !strings.Contains(page, "does not confirm") {
		t.Fatalf("%d %s", code, page)
	}
	g := newSelfFx(t, SelfConfig{InviteAnyEmail: true})
	_, path = g.newInvite(`{"email":"anna@example.test"}`)
	if code, page := g.follow(g.browser(), path); code != 200 || !strings.Contains(page, "is ready") {
		t.Fatalf("with OIDC_INVITE_REQUIRE_VERIFIED=false: %d %s", code, page)
	}
}

func TestWithdrawStopsARunningRedemption(t *testing.T) {
	f := inviteFx(t, SelfConfig{})
	ctx := context.Background()
	inv, tok, err := f.self.users.NewInvite(ctx, "anna@example.test", "x", "admin", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.self.users.ClaimInvite(ctx, tok, "anna@example.test", "oidc:sub-1"); err != nil {
		t.Fatal(err)
	}
	if err := f.self.users.RevokeInvite(ctx, inv.ID); err != nil {
		t.Fatalf("withdraw during a redemption: %v", err)
	}
	_ = f.self.users.ReleaseInvite(ctx, inv.ID) // the device could not be made
	if got := f.invites()[0]; got.State != InviteRevoked {
		t.Fatalf("the invitation came back: %+v", got)
	}
	// an invitation that made a device cannot be withdrawn
	_, path := f.newInvite(`{"email":"anna@example.test"}`)
	f.follow(f.browser(), path)
	used := f.invites()[0]
	rec := f.adminDo("POST", "/enroll/invites/"+strconv.FormatInt(used.ID, 10)+"/revoke", `{}`)
	if used.State != InviteUsed || rec.Code != 409 {
		t.Fatalf("%+v %d %s", used, rec.Code, rec.Body)
	}
}

func TestCallbackNeedsTheBrowserThatStartedTheSignIn(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	attacker := f.browser()
	attacker.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
		if strings.HasPrefix(r.URL.Path, "/oidc/callback") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	resp, err := attacker.Get(f.srv.URL + "/oidc/login")
	if err != nil {
		t.Fatal(err)
	}
	cb := resp.Header.Get("Location")
	resp.Body.Close()
	victim := f.browser()
	r2, err := victim.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	if p := readAll(r2); r2.StatusCode != 400 || strings.Contains(p, "Signed in as") {
		t.Fatalf("another browser used the callback: %d %s", r2.StatusCode, p)
	}
	// the flow was not spent: the browser that started it still signs in
	r3, err := attacker.Get(cb)
	if err != nil {
		t.Fatal(err)
	}
	if p := readAll(r3); !strings.Contains(p, "Signed in as") {
		t.Fatalf("own callback: %d %s", r3.StatusCode, p)
	}
}
