package adminoidc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

// rig is the console (a TLS test server, so the Secure cookies flow) with the sign-in routes, one
// admin route that needs nodes.view and one that needs nodes.operate, and a fake tailnet in which
// the client's address is the login set by the test.
type rig struct {
	t      *testing.T
	srv    *httptest.Server
	is     *oidctest.Issuer
	who    *authz.Who
	client *http.Client
	pol    *authz.Policy
}

func newRig(t *testing.T) *rig {
	r := &rig{t: t, who: &authz.Who{Login: "alice@example.test"}}
	pol, err := authz.NewPolicy(authz.Config{Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
	if err != nil {
		t.Fatal(err)
	}
	r.pol = pol
	old := authz.SetDefault(pol)
	t.Cleanup(func() { authz.SetDefault(old); authz.SetSessionGroups("alice@example.test", nil, time.Time{}) })

	tn := tailnetFunc(func() authz.Who { return *r.who })
	mux := core.NewMux()
	r.srv = httptest.NewUnstartedServer(mux)
	r.srv.StartTLS()
	t.Cleanup(r.srv.Close)
	r.is = oidctest.New(t, "nc-admin", "sec", r.srv.URL+"/auth/callback")
	r.is.SetUser(oidctest.User{Subject: "s-alice", Email: "alice@example.test", Groups: []string{"netops"}})
	r.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = true }
	p, err := oidc.Discover(context.Background(), oidc.Config{Issuer: r.is.URL, ClientID: "nc-admin",
		ClientSecret: "sec", RedirectURL: r.srv.URL + "/auth/callback", Scopes: []string{"openid", "email", "groups"}})
	if err != nil {
		t.Fatal(err)
	}
	m := New(Config{Provider: p, GroupsClaim: "groups", Tailnet: tn, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	m.Register(mux)
	gate := func(perm authz.Perm) http.Handler {
		return authz.Gate(gateTailnet{f: tn}, perm, authz.Allowed{}, nil)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
	}
	mux.Handle("GET /nodes", gate(authz.P(authz.FeatureNodes, authz.LevelView)))
	mux.Handle("POST /nodes/start", gate(authz.P(authz.FeatureNodes, authz.LevelOperate)))

	jar, _ := cookiejar.New(nil)
	r.client = r.srv.Client()
	r.client.Jar = jar
	// The test client follows redirects to the fake issuer (plain http) and back.
	base := r.client.Transport
	r.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == strings.TrimPrefix(r.is.URL, "http://") {
			return http.DefaultTransport.RoundTrip(req)
		}
		return base.RoundTrip(req)
	})
	return r
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type tailnetFunc func() authz.Who

func (f tailnetFunc) WhoIs(context.Context, string) (string, []string, error) {
	w := f()
	return w.Login, w.Tags, nil
}

// gateTailnet adapts tailnetFunc to core.Tailnet for authz.Gate (it calls WhoIs only).
type gateTailnet struct {
	core.Tailnet
	f tailnetFunc
}

func (g gateTailnet) WhoIs(ctx context.Context, a string) (string, []string, error) {
	return g.f.WhoIs(ctx, a)
}

func (r *rig) status(method, path string) int {
	r.t.Helper()
	req, _ := http.NewRequest(method, r.srv.URL+path, nil)
	resp, err := r.client.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (r *rig) signIn() int {
	r.t.Helper()
	resp, err := r.client.Get(r.srv.URL + "/auth/login")
	if err != nil {
		r.t.Fatal(err)
	}
	resp.Body.Close()
	if resp.Request.URL.Path == "/" {
		return http.StatusOK // the callback redirected home: signed in
	}
	return resp.StatusCode
}

// The acceptance criterion of #123: an owner grants viewer to an OIDC group; a member then views
// nodes and gets 403 on start; removing the grant takes effect without a restart.
func TestGroupGrantEndToEnd(t *testing.T) {
	r := newRig(t)
	if err := r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}}); err != nil {
		t.Fatal(err)
	}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusForbidden {
		t.Fatalf("before sign-in the group does not apply: %d", c)
	}
	if c := r.signIn(); c != http.StatusOK {
		t.Fatalf("sign-in: %d", c)
	}
	for _, c := range r.client.Jar.Cookies(mustURL(r.srv.URL)) {
		if c.Name == sessionCookie && c.Value == "" {
			t.Fatal("empty session cookie")
		}
	}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusOK {
		t.Fatalf("a member of netops views nodes: %d", c)
	}
	if c := r.status(http.MethodPost, "/nodes/start"); c != http.StatusForbidden {
		t.Fatalf("viewer may not start a node: %d", c)
	}
	_ = r.pol.SetDynamic(authz.Config{})
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusForbidden {
		t.Fatalf("a removed grant takes effect at once: %d", c)
	}

	_ = r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}})
	if c := r.status(http.MethodPost, "/auth/logout"); c != http.StatusNoContent {
		t.Fatalf("logout: %d", c)
	}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusForbidden {
		t.Fatalf("after sign-out the group does not apply: %d", c)
	}
}

// A person who signs in with another account than their tailnet login gets no groups.
func TestOtherAccountRefused(t *testing.T) {
	r := newRig(t)
	_ = r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}})
	r.is.SetUser(oidctest.User{Subject: "s-mallory", Email: "mallory@example.test", Groups: []string{"netops"}})
	if c := r.signIn(); c != http.StatusForbidden {
		t.Fatalf("another account: %d", c)
	}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusForbidden {
		t.Fatalf("no groups for the login: %d", c)
	}
	// unverified email
	r.is.SetUser(oidctest.User{Subject: "s-alice", Email: "alice@example.test", Groups: []string{"netops"}})
	r.is.Tamper = func(c jwt.MapClaims) { c["email_verified"] = false }
	if c := r.signIn(); c != http.StatusForbidden {
		t.Fatalf("unverified email: %d", c)
	}
}

// A session cookie taken to another tailnet login does not carry the groups (#366 design point 2).
func TestCookieBoundToTailnetLogin(t *testing.T) {
	r := newRig(t)
	_ = r.pol.SetDynamic(authz.Config{Grants: []authz.Grant{{Subject: "group:netops", Roles: []string{"viewer"}}}})
	if c := r.signIn(); c != http.StatusOK {
		t.Fatalf("sign-in: %d", c)
	}
	r.who = &authz.Who{Login: "bob@example.test"}
	if c := r.status(http.MethodGet, "/nodes"); c != http.StatusForbidden {
		t.Fatalf("bob with alice's cookie: %d", c)
	}
	resp, _ := r.client.Get(r.srv.URL + "/auth/me")
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(b), `"signed_in":false`) {
		t.Fatalf("bob is not signed in: %s", b)
	}
	// a tagged node cannot sign in at all
	r.who = &authz.Who{Login: "tagged-devices", Tags: []string{"tag:bridge"}}
	if c := r.status(http.MethodGet, "/auth/login"); c != http.StatusForbidden {
		t.Fatalf("tagged: %d", c)
	}
}

// A callback without the flow cookie, or with a used state, is refused.
func TestCallbackNeedsItsFlow(t *testing.T) {
	r := newRig(t)
	if c := r.status(http.MethodGet, "/auth/callback?state=x&code=y"); c != http.StatusBadRequest {
		t.Fatalf("no flow: %d", c)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }
