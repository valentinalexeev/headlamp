package enroll

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

// fakeHS is a headscale with users, keys and nodes in memory.
type fakeHS struct {
	mu      sync.Mutex
	users   []headscale.User
	keys    []headscale.PreAuthKey
	nodes   []headscale.Node
	expired map[uint64]bool
	fail    error
}

func (h *fakeHS) CreateUser(_ context.Context, name, _, _ string) (headscale.User, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	u := headscale.User{ID: uint64(len(h.users) + 10), Name: name}
	h.users = append(h.users, u)
	return u, nil
}
func (h *fakeHS) ListUsers(context.Context) ([]headscale.User, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]headscale.User(nil), h.users...), nil
}
func (h *fakeHS) CreatePreAuthKey(_ context.Context, uid uint64, _ []string, ttl time.Duration, eph, reuse bool) (headscale.PreAuthKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if eph || reuse {
		return headscale.PreAuthKey{}, errors.New("self-enrolment keys must be single-use and not ephemeral")
	}
	k := headscale.PreAuthKey{ID: uint64(len(h.keys) + 100), Key: "hskey-" + strconv.Itoa(len(h.keys)), UserID: uid, Expiration: time.Now().Add(ttl)}
	h.keys = append(h.keys, k)
	return k, nil
}
func (h *fakeHS) ListPreAuthKeys(context.Context) ([]headscale.PreAuthKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]headscale.PreAuthKey(nil), h.keys...), nil
}
func (h *fakeHS) ExpirePreAuthKey(_ context.Context, id uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.keys {
		if h.keys[i].ID == id {
			h.keys[i].Expiration = time.Now().Add(-time.Second)
		}
	}
	return nil
}
func (h *fakeHS) ListNodes(context.Context) ([]headscale.Node, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]headscale.Node(nil), h.nodes...), nil
}
func (h *fakeHS) ExpireNode(_ context.Context, id uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.fail != nil {
		return h.fail
	}
	if h.expired == nil {
		h.expired = map[uint64]bool{}
	}
	h.expired[id] = true
	return nil
}

type selfFx struct {
	t      *testing.T
	is     *oidctest.Issuer
	self   *Self
	store  *Store
	hs     *fakeHS
	agent  *stubAgent
	srv    *httptest.Server
	admin  *Admin
	pushes int
}

func newSelfFx(t *testing.T, cfg SelfConfig) *selfFx {
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	is := oidctest.New(t, "nc", "sec", srv.URL+"/oidc/callback")
	f := &selfFx{t: t, is: is, hs: &fakeHS{}, srv: srv}
	f.store = newStore(t, adminNow)
	f.agent = &stubAgent{}
	f.admin = NewAdmin(f.store, adminCfg, f.agent)
	rp, err := oidc.Discover(context.Background(), oidc.Config{Issuer: is.URL, ClientID: "nc", ClientSecret: "sec", RedirectURL: f.srv.URL + "/oidc/callback"})
	if err != nil {
		t.Fatal(err)
	}
	users, err := OpenSelfStore(t.TempDir()+"/oidc.db", testKey, func() time.Time { return time.Unix(adminNow, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { users.Close() })
	cfg.defaults()
	f.self = &Self{cfg: cfg, users: users, store: f.store, hs: f.hs, idp: rp, push: func(ctx context.Context) {
		f.pushes++
		SyncAll(ctx, f.store, adminCfg, f.admin.reg.Current(ctx, false).SyncTargets)
	}}
	f.admin.self = f.self
	(&SelfWeb{self: f.self, rp: rp, link: func() IngressConfig { return adminCfg }}).Register(mux)
	return f
}

// browser is a client with a cookie jar that follows redirects to the issuer and back.
func (f *selfFx) browser() *http.Client {
	jar, _ := cookiejar.New(nil)
	c := *f.srv.Client() // a copy: each browser has its own jar
	c.Jar = jar
	return &c
}

func (f *selfFx) signIn(c *http.Client) string {
	f.t.Helper()
	resp, err := c.Get(f.srv.URL + "/oidc/login")
	if err != nil {
		f.t.Fatal(err)
	}
	b := readAll(resp)
	if resp.StatusCode != 200 || !strings.Contains(b, "Signed in as") {
		f.t.Fatalf("sign-in: %d %s", resp.StatusCode, b)
	}
	return csrfOf(f.t, b)
}

func readAll(resp *http.Response) string {
	defer resp.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			return sb.String()
		}
	}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([0-9a-f]+)"`)

func csrfOf(t *testing.T, page string) string {
	m := csrfRe.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no csrf in page: %s", page)
	}
	return m[1]
}

func (f *selfFx) enrol(c *http.Client, csrf, name string) (int, string) {
	resp, err := c.PostForm(f.srv.URL+"/me/devices", url.Values{"csrf": {csrf}, "name": {name}, "client": {"sing-box"}})
	if err != nil {
		f.t.Fatal(err)
	}
	return resp.StatusCode, readAll(resp)
}

func TestSelfEnrolEndToEnd(t *testing.T) {
	f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}})
	c := f.browser()
	csrf := f.signIn(c)
	code, page := f.enrol(c, csrf, "Phone")
	if code != 200 || !strings.Contains(page, "Phone is ready") || !strings.Contains(page, "hskey-0") || !strings.Contains(page, "/sub/") {
		t.Fatalf("%d %s", code, page)
	}
	devs, _ := f.store.ListDevices()
	if len(devs) != 1 || devs[0].Owner != "oidc:sub-1" || devs[0].Client != "sing-box" {
		t.Fatalf("%+v", devs)
	}
	if last := f.agent.last(); len(last.creds) != 3 {
		t.Errorf("the device was not pushed to the bridge: %+v", last)
	}
	if len(f.hs.users) != 1 || !strings.HasPrefix(f.hs.users[0].Name, "oidc-") || f.hs.keys[0].UserID != f.hs.users[0].ID {
		t.Errorf("headscale: %+v %+v", f.hs.users, f.hs.keys)
	}
	// a second device reuses the same headscale user
	f.enrol(c, csrf, "Laptop")
	if len(f.hs.users) != 1 || len(f.hs.keys) != 2 {
		t.Errorf("headscale user not reused: %+v", f.hs.users)
	}
	// the page after reload shows the devices, not the key
	resp, _ := c.Get(f.srv.URL + "/me")
	p := readAll(resp)
	if !strings.Contains(p, "Phone") || !strings.Contains(p, "Laptop") || strings.Contains(p, "hskey-") {
		t.Errorf("reload: %s", p)
	}
}

func TestSelfRefusals(t *testing.T) {
	t.Run("not in group", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"admins"}})
		resp, _ := f.browser().Get(f.srv.URL + "/oidc/login")
		if p := readAll(resp); resp.StatusCode != 403 || !strings.Contains(p, "not in a group") {
			t.Errorf("%d %s", resp.StatusCode, p)
		}
	})
	t.Run("device limit", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{MaxDevices: 1})
		c := f.browser()
		csrf := f.signIn(c)
		f.enrol(c, csrf, "a")
		if code, p := f.enrol(c, csrf, "b"); code != 409 || !strings.Contains(p, "limit") {
			t.Errorf("%d", code)
		}
		if len(f.hs.keys) != 1 {
			t.Error("a key was made past the limit")
		}
	})
	t.Run("csrf", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{})
		c := f.browser()
		f.signIn(c)
		if code, _ := f.enrol(c, "00", "x"); code != 403 {
			t.Errorf("%d", code)
		}
	})
	t.Run("no session", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{})
		c := f.browser()
		c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		resp, _ := c.PostForm(f.srv.URL+"/me/devices", url.Values{"name": {"x"}, "client": {"sing-box"}})
		if resp.StatusCode != 303 {
			t.Errorf("%d", resp.StatusCode)
		}
	})
	t.Run("callback replay", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{})
		c := f.browser()
		c.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
			if strings.HasPrefix(r.URL.Path, "/oidc/callback") {
				return http.ErrUseLastResponse
			}
			return nil
		}
		resp, _ := c.Get(f.srv.URL + "/oidc/login")
		cb := resp.Header.Get("Location")
		resp.Body.Close()
		r1, _ := c.Get(cb)
		r1.Body.Close()
		r2, _ := c.Get(cb)
		if p := readAll(r2); r2.StatusCode != 400 || !strings.Contains(p, "state does not match") {
			t.Errorf("replay: %d %s", r2.StatusCode, p)
		}
	})
	t.Run("other's device", func(t *testing.T) {
		f := newSelfFx(t, SelfConfig{})
		d, _, _ := f.store.CreateDevice("theirs", "admin-made", "sing-box", adminFuture)
		c := f.browser()
		csrf := f.signIn(c)
		_, _ = c.PostForm(f.srv.URL+"/me/devices/"+strconv.FormatInt(d.ID, 10)+"/revoke", url.Values{"csrf": {csrf}})
		if got, _ := f.store.GetDevice(d.ID); got.Revoked {
			t.Error("a person revoked a device that is not theirs")
		}
	})
}

func TestSelfPageEscapesAndHeaders(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	f.is.SetUser(oidctest.User{Subject: "s", Email: `<script>x</script>@e.test`})
	c := f.browser()
	csrf := f.signIn(c)
	_, p := f.enrol(c, csrf, `<img src=x onerror=1>`)
	if strings.Contains(p, "<script>x") || strings.Contains(p, "<img src=x") {
		t.Error("not escaped")
	}
	resp, _ := c.Get(f.srv.URL + "/me?error=<b>pwned</b>")
	if p := readAll(resp); strings.Contains(p, "pwned") {
		t.Error("URL text reached the page")
	}
	if resp.Header.Get("Content-Security-Policy") == "" || resp.Header.Get("Cache-Control") != "no-store" {
		t.Errorf("headers %v", resp.Header)
	}
	for _, ck := range c.Jar.Cookies(mustURL(f.srv.URL)) {
		if ck.Name == sessionCookie && len(ck.Value) < 64 {
			t.Error("short session id")
		}
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestLockRevokesExpiresAndRefuses(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	c := f.browser()
	csrf := f.signIn(c)
	f.enrol(c, csrf, "Phone")
	hsID := f.hs.users[0].ID
	f.hs.nodes = []headscale.Node{{ID: 1, UserID: hsID}, {ID: 2, UserID: 999}}
	before := f.pushes
	if err := f.self.Lock(context.Background(), "sub-1", "admin@", "left"); err != nil {
		t.Fatal(err)
	}
	devs, _ := f.store.ListDevices()
	if !devs[0].Revoked || f.pushes == before {
		t.Error("device not revoked and pushed")
	}
	if !f.hs.expired[1] || f.hs.expired[2] {
		t.Errorf("node expiry: %v", f.hs.expired)
	}
	if f.hs.keys[0].Expiration.After(time.Now()) {
		t.Error("unused key not expired")
	}
	// the session is gone, and a new login is refused
	resp, _ := c.Get(f.srv.URL + "/me")
	if p := readAll(resp); strings.Contains(p, "Signed in as") {
		t.Error("session survived the lock")
	}
	resp, _ = f.browser().Get(f.srv.URL + "/oidc/login")
	if p := readAll(resp); resp.StatusCode != 403 || !strings.Contains(p, "locked") {
		t.Errorf("login after lock: %d", resp.StatusCode)
	}
	// unlock lets the person in again; the old device stays revoked
	if err := f.self.Unlock("sub-1", "admin@"); err != nil {
		t.Fatal(err)
	}
	f.signIn(f.browser())
	if d, _ := f.store.GetDevice(devs[0].ID); !d.Revoked {
		t.Error("unlock revived a device")
	}
}

func TestLockReportsHeadscaleFailureButStillRevokes(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	c := f.browser()
	f.enrol(c, f.signIn(c), "Phone")
	f.hs.nodes = []headscale.Node{{ID: 1, UserID: f.hs.users[0].ID}}
	f.hs.fail = errors.New("headscale down")
	if err := f.self.Lock(context.Background(), "sub-1", "a", ""); err == nil {
		t.Error("the failure must be reported")
	}
	if devs, _ := f.store.ListDevices(); !devs[0].Revoked {
		t.Error("the relay device must be revoked anyway")
	}
}

func TestRecheck(t *testing.T) {
	cases := map[string]struct {
		setup  func(*oidctest.Issuer)
		locked bool
		failed bool
	}{
		"still fine":         {func(*oidctest.Issuer) {}, false, false},
		"disabled at issuer": {func(is *oidctest.Issuer) { is.Disabled = true }, true, false},
		"left the group": {func(is *oidctest.Issuer) {
			is.SetUser(oidctest.User{Subject: "sub-1", Groups: []string{"other"}})
		}, true, false},
		"issuer down": {func(is *oidctest.Issuer) { is.Close() }, false, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newSelfFx(t, SelfConfig{AllowedGroups: []string{"staff"}, Recheck: 5 * time.Minute})
			c := f.browser()
			f.enrol(c, f.signIn(c), "Phone")
			tc.setup(f.is)
			// not due yet: nothing happens
			f.self.Recheck(context.Background())
			if u, _ := f.self.users.User("sub-1"); u.Locked {
				t.Fatal("rechecked before due")
			}
			f.self.users.clock = func() time.Time { return time.Unix(adminNow+600, 0) }
			f.store.clock = func() time.Time { return time.Unix(adminNow+600, 0) }
			f.self.Recheck(context.Background())
			u, _ := f.self.users.User("sub-1")
			if u.Locked != tc.locked || (u.CheckFailed != 0) != tc.failed {
				t.Errorf("locked=%v failed=%v", u.Locked, u.CheckFailed)
			}
			if devs, _ := f.store.ListDevices(); devs[0].Revoked != tc.locked {
				t.Errorf("device revoked=%v", devs[0].Revoked)
			}
		})
	}
}

func TestPeopleAdminAPI(t *testing.T) {
	f := newSelfFx(t, SelfConfig{})
	c := f.browser()
	f.enrol(c, f.signIn(c), "Phone")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /enroll/people", f.admin.listPeople)
	mux.HandleFunc("POST /enroll/people/{sub}/lock", f.admin.jsonOnly(f.admin.lockPerson))
	mux.HandleFunc("POST /enroll/people/{sub}/unlock", f.admin.jsonOnly(f.admin.unlockPerson))
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	rec := do("GET", "/enroll/people", "")
	if !strings.Contains(rec.Body.String(), `"owner":"oidc:sub-1"`) || !strings.Contains(rec.Body.String(), `"active_devices":1`) || strings.Contains(rec.Body.String(), "refresh") {
		t.Errorf("%s", rec.Body)
	}
	if rec := do("POST", "/enroll/people/sub-1/lock", `{"reason":"left"}`); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("lock %d %s", rec.Code, rec.Body)
	}
	if rec := do("POST", "/enroll/people/nobody/lock", `{}`); rec.Code != 404 {
		t.Errorf("unknown %d", rec.Code)
	}
	if rec := do("POST", "/enroll/people/sub-1/unlock", `{}`); rec.Code != 200 {
		t.Errorf("unlock %d", rec.Code)
	}
}

func TestSelfConfigValidation(t *testing.T) {
	base := map[string]string{"OIDC_ISSUER": "https://idp.example", "OIDC_CLIENT_ID": "c", "OIDC_DB": "/x", "OIDC_REDIRECT_URL": "https://links.example/oidc/callback"}
	env := func(over map[string]string) core.Env {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range over {
			m[k] = v
		}
		return core.MapEnv(m)
	}
	if _, sc, _, err := selfConfig(env(nil)); err != nil || sc.MaxDevices != 3 || sc.Recheck != time.Hour {
		t.Fatalf("%+v %v", sc, err)
	}
	for k, v := range map[string]string{"OIDC_REDIRECT_URL": "http://links.example/oidc/callback", "OIDC_MAX_DEVICES": "0", "OIDC_RECHECK_MINUTES": "x", "OIDC_DB": ""} {
		if _, _, _, err := selfConfig(env(map[string]string{k: v})); err == nil {
			t.Errorf("%s=%q accepted", k, v)
		}
	}
}
