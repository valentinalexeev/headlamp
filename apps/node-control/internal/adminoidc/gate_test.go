package adminoidc

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
)

// clock is a test clock that the server goroutines read.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func requiredRig(t *testing.T) (*rig, *clock) {
	c := &clock{t: time.Now()}
	r := newRigWith(t, func(cfg *Config) {
		cfg.Required, cfg.Idle, cfg.Session, cfg.Now = true, 30*time.Minute, 8*time.Hour, c.now
	})
	// alice is an owner by login (as ADMIN_USERS would make her).
	if err := r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{
		{Subject: "alice@example.test", Roles: []string{"owner"}}}}); err != nil {
		t.Fatal(err)
	}
	r.client.CheckRedirect = nil
	return r, c
}

// do sends a request without following redirects.
func (r *rig) do(method, path string, hdr map[string]string) *http.Response {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	cl := *r.client
	cl.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := cl.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp
}

var browser = map[string]string{"Accept": "text/html", "Sec-Fetch-Mode": "navigate"}

// Required mode: without a session, an owner by login gets nothing; a page load is sent to the
// sign-in with the page as next; an API call gets 401.
func TestRequiredNeedsSession(t *testing.T) {
	r, _ := requiredRig(t)
	resp := r.do(http.MethodGet, "/nodes?x=1", browser)
	if resp.StatusCode != http.StatusFound ||
		resp.Header.Get("Location") != "/auth/login?next=%2Fnodes%3Fx%3D1" {
		t.Fatalf("browser: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	if c := r.do(http.MethodGet, "/nodes", nil).StatusCode; c != http.StatusUnauthorized {
		t.Fatalf("api get: %d", c)
	}
	if c := r.do(http.MethodPost, "/nodes/start", browser).StatusCode; c != http.StatusUnauthorized {
		t.Fatalf("post: %d", c)
	}
	// /auth/* is open to a human caller, so the sign-in can start
	if c := r.do(http.MethodGet, "/auth/me", nil).StatusCode; c != http.StatusOK {
		t.Fatalf("me: %d", c)
	}
	if c := r.signIn(); c != http.StatusOK {
		t.Fatalf("sign-in: %d", c)
	}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusOK {
		t.Fatalf("signed in: %d", c)
	}
	if c := r.status(http.MethodPost, "/nodes/start"); c != http.StatusOK {
		t.Fatalf("signed-in owner operates: %d", c)
	}
	// tagged callers are refused before the session is looked at
	r.who = &authz.Who{Login: "tagged-devices", Tags: []string{"tag:x"}}
	if c := r.do(http.MethodGet, "/nodes", nil).StatusCode; c != http.StatusForbidden {
		t.Fatalf("tagged: %d", c)
	}
}

// The sign-in returns to the local page of next.
func TestRequiredLoginNext(t *testing.T) {
	r, _ := requiredRig(t)
	resp, err := r.client.Get(r.srv.URL + "/auth/login?next=%2Fnodes%3Fx%3D1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Request.URL.RequestURI() != "/nodes?x=1" {
		t.Fatalf("landed on %d %s", resp.StatusCode, resp.Request.URL)
	}
}

func TestLocalNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "/",
		"/nodes?x=1":            "/nodes?x=1",
		"/":                     "/",
		"//evil.test/x":         "/",
		"/\\evil.test":          "/",
		"\\\\evil.test":         "/",
		"https://evil.test/":    "/",
		"javascript:alert(1)":   "/",
		"nodes":                 "/",
		"/a\r\nLocation: //x":   "/",
		"/auth/logout":          "/",
		"/%2F%2Fevil.test":      "/",
		"/x?next=//evil.test":   "/x?next=//evil.test",
		"/\t/evil.test":         "/",
		"/x#frag":               "/x#frag",
		"https:/\\evil.test":    "/",
		"/../..//evil.test":     "/../..//evil.test",
		"/x@evil.test":          "/x@evil.test",
		"http:evil.test":        "/",
		"/ok/path with space":   "/ok/path with space",
		"\x00/x":                "/",
		"/%0d%0aSet-Cookie:a=b": "/%0d%0aSet-Cookie:a=b",
	} {
		if got := localNext(in); got != want {
			t.Errorf("localNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// A session of another login gives 403, not the rights of either.
func TestRequiredOtherLogin(t *testing.T) {
	r, _ := requiredRig(t)
	if c := r.signIn(); c != http.StatusOK {
		t.Fatalf("sign-in: %d", c)
	}
	r.who = &authz.Who{Login: "bob@example.test"}
	if c := r.do(http.MethodGet, "/nodes", browser).StatusCode; c != http.StatusForbidden {
		t.Fatalf("bob with alice's cookie: %d", c)
	}
}

// A session ends after Idle without a request, and each request resets the timer. It also ends
// at its expiry. Either way, the next page load goes to the sign-in again.
func TestRequiredIdleAndExpiry(t *testing.T) {
	r, c := requiredRig(t)
	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in: %d", code)
	}
	for i := 0; i < 4; i++ { // 4 x 20 min > 30 min, but each request resets the timer
		c.add(20 * time.Minute)
		if code := r.do(http.MethodGet, "/nodes", nil).StatusCode; code != http.StatusOK {
			t.Fatalf("active session, step %d: %d", i, code)
		}
	}
	c.add(31 * time.Minute)
	if code := r.do(http.MethodGet, "/nodes", browser).StatusCode; code != http.StatusFound {
		t.Fatalf("idle: %d", code)
	}
	if code := r.do(http.MethodGet, "/nodes", nil).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("idle session is gone: %d", code)
	}
	if authz.SessionGroups("alice@example.test") != nil {
		t.Fatal("the groups of an idle session stay")
	}

	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in again: %d", code)
	}
	for i := 0; i < 17; i++ { // 17 x 29 min > 8 h
		c.add(29 * time.Minute)
		r.do(http.MethodGet, "/nodes", nil)
	}
	if code := r.do(http.MethodGet, "/nodes", browser).StatusCode; code != http.StatusFound {
		t.Fatalf("expired: %d", code)
	}
}

// /auth/me tells the console the mode.
func TestRequiredMe(t *testing.T) {
	r, _ := requiredRig(t)
	if c := r.signIn(); c != http.StatusOK {
		t.Fatalf("sign-in: %d", c)
	}
	resp, err := r.client.Get(r.srv.URL + "/auth/me")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`"signed_in":true`, `"required":true`, `"idle_minutes":30`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("me %s: want %s", b, want)
		}
	}
}

// reauthRig is requiredRig with re-authentication (#469) on POST /nodes/start, 3 minutes. The
// issuer sends auth_time = now - authAge seconds; authAge < 0 sends no auth_time.
func reauthRig(t *testing.T, warn bool) (*rig, *clock, *atomic.Int64) {
	c := &clock{t: time.Now()}
	r := newRigWith(t, func(cfg *Config) {
		cfg.Required, cfg.Idle, cfg.Session, cfg.Now = true, 30*time.Minute, 8*time.Hour, c.now
		cfg.Reauth, cfg.ReauthWarn = 3*time.Minute, warn
		cfg.Sensitive = map[string]bool{"POST /nodes/start": true}
	})
	if err := r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{
		{Subject: "alice@example.test", Roles: []string{"owner"}}}}); err != nil {
		t.Fatal(err)
	}
	authAge := &atomic.Int64{}
	r.is.Tamper = func(cl jwt.MapClaims) {
		cl["email_verified"] = true
		if age := authAge.Load(); age >= 0 {
			cl["auth_time"] = c.now().Add(-time.Duration(age) * time.Second).Unix()
		}
	}
	return r, c, authAge
}

// reauth runs the new sign-in of a sensitive route and returns the status and the text of the
// last page.
func (r *rig) reauth() (int, string) {
	r.t.Helper()
	resp, err := r.client.Get(r.srv.URL + "/auth/login?reauth=1")
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.Request.URL.Path == "/" {
		return http.StatusOK, string(b)
	}
	return resp.StatusCode, string(b)
}

// A sensitive route needs a sign-in of the last Reauth; other routes do not. The new sign-in asks
// the IdP for prompt=login and max_age.
func TestReauthSensitiveRoute(t *testing.T) {
	r, c, _ := reauthRig(t, false)
	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in: %d", code)
	}
	if q := r.is.LastAuthorize(); q.Get("prompt") != "" || q.Get("max_age") != "" {
		t.Fatalf("a plain sign-in asks for a new one: %v", q)
	}
	if code := r.do(http.MethodPost, "/nodes/start", nil).StatusCode; code != http.StatusOK {
		t.Fatalf("fresh sign-in: %d", code)
	}
	c.add(4 * time.Minute)
	if code := r.do(http.MethodGet, "/nodes", nil).StatusCode; code != http.StatusOK {
		t.Fatalf("not sensitive: %d", code)
	}
	resp := r.do(http.MethodPost, "/nodes/start", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old sign-in: %d", resp.StatusCode)
	}
	if !strings.Contains(r.log.String(), "admin re-authentication required") {
		t.Fatalf("log: %s", r.log.String())
	}
	if code, page := r.reauth(); code != http.StatusOK {
		t.Fatalf("re-authentication: %d %s", code, page)
	}
	if q := r.is.LastAuthorize(); q.Get("prompt") != "login" || q.Get("max_age") != "180" {
		t.Fatalf("re-authentication query: %v", q)
	}
	if code := r.do(http.MethodPost, "/nodes/start", nil).StatusCode; code != http.StatusOK {
		t.Fatalf("after re-authentication: %d", code)
	}
}

// The API answer of a sensitive route without a recent sign-in links the new sign-in.
func TestReauthAPIAnswer(t *testing.T) {
	r, c, _ := reauthRig(t, false)
	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in: %d", code)
	}
	c.add(4 * time.Minute)
	req, _ := http.NewRequest(http.MethodPost, r.srv.URL+"/nodes/start", nil)
	req.Header.Set("Referer", r.srv.URL+"/nodes?x=1")
	resp, err := r.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Reauth bool   `json:"reauth"`
		Login  string `json:"login"`
	}
	err = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()
	if err != nil || resp.StatusCode != http.StatusUnauthorized || !out.Reauth ||
		out.Login != "/auth/login?reauth=1&next=%2Fnodes%3Fx%3D1" {
		t.Fatalf("answer %d %+v %v", resp.StatusCode, out, err)
	}
}

// An IdP that sends no auth_time, or an old one, does not prove the new sign-in. By default the
// sign-in is refused and the session ends.
func TestReauthUnprovenLogout(t *testing.T) {
	for name, age := range map[string]int64{"no_auth_time": -1, "auth_time_old": 3600} {
		t.Run(name, func(t *testing.T) {
			r, _, authAge := reauthRig(t, false)
			authAge.Store(age)
			if code := r.signIn(); code != http.StatusOK {
				t.Fatalf("sign-in: %d", code)
			}
			if code := r.do(http.MethodPost, "/nodes/start", nil).StatusCode; code != http.StatusUnauthorized {
				t.Fatalf("unproven sign-in on a sensitive route: %d", code)
			}
			if code, _ := r.reauth(); code != http.StatusForbidden {
				t.Fatalf("re-authentication: %d", code)
			}
			if code := r.do(http.MethodGet, "/nodes", nil).StatusCode; code != http.StatusUnauthorized {
				t.Fatalf("the session stays: %d", code)
			}
			if authz.SessionGroups("alice@example.test") != nil {
				t.Fatal("the groups of the ended session stay")
			}
			if l := r.log.String(); !strings.Contains(l, "admin re-authentication refused") ||
				!strings.Contains(l, "reason="+name) {
				t.Fatalf("log: %s", l)
			}
		})
	}
}

// With ReauthWarn, an unproven new sign-in is accepted with a warning in the log.
func TestReauthUnprovenWarn(t *testing.T) {
	r, _, authAge := reauthRig(t, true)
	authAge.Store(-1)
	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in: %d", code)
	}
	if code, page := r.reauth(); code != http.StatusOK {
		t.Fatalf("re-authentication: %d %s", code, page)
	}
	if code := r.do(http.MethodPost, "/nodes/start", nil).StatusCode; code != http.StatusOK {
		t.Fatalf("after accepted re-authentication: %d", code)
	}
	if !strings.Contains(r.log.String(), "admin re-authentication not proven by the IdP, accepted") {
		t.Fatalf("log: %s", r.log.String())
	}
}

// Re-authentication needs the required mode: New turns it off without it.
func TestReauthNeedsRequired(t *testing.T) {
	m := New(Config{Reauth: time.Minute})
	if m.cfg.Reauth != 0 {
		t.Fatalf("reauth on without the required mode: %v", m.cfg.Reauth)
	}
}

// Mode off: Gate changes nothing. An owner by login uses the console without a session, and an
// idle session is not ended.
func TestNotRequiredUnchanged(t *testing.T) {
	c := &clock{t: time.Now()}
	r := newRigWith(t, func(cfg *Config) { cfg.Now, cfg.Idle = c.now, time.Minute })
	_ = r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{
		{Subject: "alice@example.test", Roles: []string{"owner"}},
		{Subject: "group:netops", Roles: []string{"viewer"}}}})
	if code := r.do(http.MethodPost, "/nodes/start", nil).StatusCode; code != http.StatusOK {
		t.Fatalf("owner by login without a session: %d", code)
	}
	_ = r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{
		{Subject: "group:netops", Roles: []string{"viewer"}}}})
	if code := r.signIn(); code != http.StatusOK {
		t.Fatalf("sign-in: %d", code)
	}
	c.add(2 * time.Hour)
	if code := r.do(http.MethodGet, "/nodes", browser).StatusCode; code != http.StatusOK {
		t.Fatalf("no idle timeout when not required: %d", code)
	}
	resp, _ := r.client.Get(r.srv.URL + "/auth/me")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"required":false`) ||
		!strings.Contains(string(b), `"signed_in":true`) {
		t.Fatalf("me: %s", b)
	}
}
