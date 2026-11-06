package authz

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

const (
	ipOwner    = "100.64.0.1"
	ipOperator = "100.64.0.2"
	ipViewer   = "100.64.0.3"
	ipNobody   = "100.64.0.4"
	ipTagged   = "100.64.0.5"
	ipBroken   = "100.64.0.6"
	ipFunnelOp = "100.64.0.7"
)

func rig(t *testing.T, open bool, cfg Config) *core.Mux {
	t.Helper()
	p, err := NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	tn := NewFakeTailnet(map[string]Who{
		ipOwner:    {Login: "owner@"},
		ipOperator: {Login: "op@"},
		ipViewer:   {Login: "viewer@"},
		ipNobody:   {Login: "nobody@"},
		ipTagged:   {Login: "tagged-devices", Tags: []string{"tag:exit"}},
		ipBroken:   {Err: errors.New("whois down")},
		ipFunnelOp: {Login: "funnel@"},
	})
	m := core.NewMux()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, _ := SubjectFrom(r.Context())
		_, _ = io.WriteString(w, s.Login)
	})
	for p := range Routes {
		m.Handle(p, ok)
	}
	if err := m.Seal(&Authorizer{Policy: p, Tailnet: tn, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Open: open}); err != nil {
		t.Fatal(err)
	}
	return m
}

var standard = Config{
	Roles: map[string][]string{"funnel-admin": {"funnel.admin"}},
	Grants: []Grant{
		{Subject: "owner@", Roles: []string{"owner"}},
		{Subject: "op@", Roles: []string{"operator"}},
		{Subject: "viewer@", Roles: []string{"viewer"}},
		{Subject: "funnel@", Roles: []string{"funnel-admin"}},
	},
}

func call(m http.Handler, method, path, ip string) int {
	req := httptest.NewRequest(method, path, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	return rec.Code
}

// The acceptance table of #122: what each role may do, on real route patterns.
func TestAuthorizerRolesOnRoutes(t *testing.T) {
	m := rig(t, false, standard)
	for _, tc := range []struct {
		method, path string
		ip           string
		want         int
	}{
		// viewer sees, but does not act
		{"GET", "/nodes.json", ipViewer, 200},
		{"POST", "/nodes/n1/start", ipViewer, 403},
		{"POST", "/enroll/devices/1/revoke", ipViewer, 403},
		{"POST", "/tasks/t1/run", ipViewer, 403},
		{"GET", "/policy/state", ipViewer, 200},
		{"POST", "/policy/apply", ipViewer, 403},
		{"GET", "/tasks/secrets", ipViewer, 403},
		// operator acts on nodes, tasks and devices, does not configure
		{"POST", "/nodes/n1/start", ipOperator, 200},
		{"POST", "/nodes/n1/stop", ipOperator, 200},
		{"POST", "/tasks/t1/run", ipOperator, 200},
		{"POST", "/enroll/devices/1/revoke", ipOperator, 200},
		{"PUT", "/tasks/config", ipOperator, 403},
		{"POST", "/policy/apply", ipOperator, 403},
		{"POST", "/enroll/ingresses/b1/approve", ipOperator, 403},
		{"POST", "/funnel/namespaces", ipOperator, 403},
		// owner does everything
		{"POST", "/policy/apply", ipOwner, 200},
		{"PUT", "/tasks/secrets", ipOwner, 200},
		{"DELETE", "/funnel/keys/k1", ipOwner, 200},
		// a custom role is limited to its feature
		{"POST", "/funnel/routes", ipFunnelOp, 200},
		{"GET", "/nodes.json", ipFunnelOp, 403},
		{"GET", "/_attention.json", ipFunnelOp, 200},
		// no grant: nothing, not even the shared JSON
		{"GET", "/", ipNobody, 403},
		{"GET", "/_attention.json", ipNobody, 403},
		// tagged nodes and WhoIs failures are never users
		{"GET", "/", ipTagged, 403},
		{"GET", "/_search.json", ipTagged, 403},
		{"GET", "/", ipBroken, 403},
		// open routes need nothing
		{"GET", "/healthz", ipNobody, 200},
		{"GET", "/nodes/nodes.js", ipTagged, 200},
		// machine routes are left to their own tag gate
		{"GET", "/ingress/v1/config", ipTagged, 200},
	} {
		if got := call(m, tc.method, tc.path, tc.ip); got != tc.want {
			t.Errorf("%s %s from %s = %d, want %d", tc.method, tc.path, tc.ip, got, tc.want)
		}
	}
}

// Open mode (decision 3, option B): with no grant at all, every human passes as before, and tagged nodes still do
// not. As soon as the policy grants anything, open mode has no effect.
func TestAuthorizerOpenMode(t *testing.T) {
	m := rig(t, true, Config{})
	if got := call(m, "POST", "/nodes/n1/stop", ipNobody); got != 200 {
		t.Fatalf("open mode, no policy: human stop = %d, want 200", got)
	}
	if got := call(m, "POST", "/nodes/n1/stop", ipTagged); got != 403 {
		t.Fatalf("open mode must still refuse a tagged node: %d", got)
	}
	m = rig(t, true, standard)
	if got := call(m, "POST", "/nodes/n1/stop", ipNobody); got != 403 {
		t.Fatalf("open mode with grants must apply them: %d", got)
	}
}

// Closed mode (option A): with no grant at all, every gated route refuses.
func TestAuthorizerClosedWithoutPolicy(t *testing.T) {
	m := rig(t, false, Config{})
	for _, p := range []string{"/", "/nodes.json", "/_attention.json"} {
		if got := call(m, "GET", p, ipOwner); got != 403 {
			t.Errorf("closed, no policy: GET %s = %d, want 403", p, got)
		}
	}
	if got := call(m, "GET", "/healthz", ipOwner); got != 200 {
		t.Errorf("healthz must stay open: %d", got)
	}
}

func TestSealRefusesUnknownRoute(t *testing.T) {
	m := core.NewMux()
	m.HandleFunc("GET /secret-new-page", func(http.ResponseWriter, *http.Request) {})
	if err := m.Seal(&Authorizer{Tailnet: NewFakeTailnet(nil)}); err == nil {
		t.Fatal("a route without a permission must stop the server")
	}
}

func TestRouteAddedAfterSealIsForbidden(t *testing.T) {
	m := core.NewMux()
	if err := m.Seal(&Authorizer{Tailnet: NewFakeTailnet(nil)}); err != nil {
		t.Fatal(err)
	}
	m.HandleFunc("GET /late", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if got := call(m, "GET", "/late", ipOwner); got != 403 {
		t.Fatalf("a route added after Seal = %d, want 403", got)
	}
}

// The handler sees the caller the check used.
func TestAuthorizerPassesSubject(t *testing.T) {
	m := rig(t, false, standard)
	req := httptest.NewRequest("GET", "/nodes.json", nil)
	req.RemoteAddr = ipViewer + ":1"
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Body.String() != "viewer" {
		t.Fatalf("subject = %q", rec.Body.String())
	}
}
