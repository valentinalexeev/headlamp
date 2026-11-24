package adminoidc

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

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
