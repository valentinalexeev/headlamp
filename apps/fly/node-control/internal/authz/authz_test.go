package authz

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	userIP = "100.64.0.1"
	tagIP  = "100.64.0.2"
)

func fake() *FakeTailnet {
	return NewFakeTailnet(map[string]Who{
		"100.64.0.1": {Login: "alice@"},
		"100.64.0.2": {Login: "tagged-devices", Tags: []string{"tag:ingress-proxy"}},
		"100.64.0.3": {Login: "bob@example.test"},
		"100.64.0.4": {Login: "tagged-devices"},
		"100.64.0.5": {Login: "alice@", Tags: []string{"tag:other"}},
		"100.64.0.6": {Login: "", Tags: nil},
		"100.64.0.7": {Tags: []string{"tag:ingress-proxyx"}},
		"100.64.0.8": {Err: errors.New("boom")},
		"100.64.0.9": {Login: "Tagged-Devices@"},
	})
}

type probe struct {
	called bool
	login  string
	tags   []string
}

func (p *probe) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p.called = true
		p.login = LoginFrom(r.Context())
		p.tags = NodeTagsFrom(r.Context())
		w.WriteHeader(http.StatusOK)
	})
}

func do(h http.Handler, remote string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", "/x", nil)
	r.RemoteAddr = remote
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func assertDenied(t *testing.T, w *httptest.ResponseRecorder, p *probe) {
	t.Helper()
	if w.Code != 403 || w.Body.Len() != 0 {
		t.Fatalf("got %d body %q, want empty 403", w.Code, w.Body.String())
	}
	if p.called {
		t.Fatal("next handler called")
	}
}

func TestUserGateAllowsListedUser(t *testing.T) {
	p := &probe{}
	w := do(UserGate(fake(), ParseAllowed("alice@"), nil)(p.Handler()), userIP+":1234")
	if w.Code != 200 || !p.called {
		t.Fatalf("got %d", w.Code)
	}
}

func TestUserGateLoginFormsMatch(t *testing.T) {
	for _, l := range []string{"alice", "alice@", "Alice@"} {
		tn := NewFakeTailnet(map[string]Who{userIP: {Login: l}})
		p := &probe{}
		w := do(UserGate(tn, ParseAllowed("alice@"), nil)(p.Handler()), userIP+":1")
		if w.Code != 200 {
			t.Errorf("login %q: got %d", l, w.Code)
		}
	}
}

func TestUserGateOtherUser403(t *testing.T) {
	p := &probe{}
	assertDenied(t, do(UserGate(fake(), ParseAllowed("alice@"), nil)(p.Handler()), "100.64.0.3:1"), p)
}

func TestUserGateEmptyAllowlist403(t *testing.T) {
	p := &probe{}
	assertDenied(t, do(UserGate(fake(), ParseAllowed(" , "), nil)(p.Handler()), userIP+":1"), p)
}

func TestUserGateTaggedNode403(t *testing.T) {
	p := &probe{}
	assertDenied(t, do(UserGate(fake(), ParseAllowed("alice@"), nil)(p.Handler()), "100.64.0.5:1"), p)
}

func TestUserGateTaggedDevicesLogin403(t *testing.T) {
	// Even if the allow-list names it, the tagged-devices pseudo user is never a person.
	for _, ip := range []string{"100.64.0.4:1", "100.64.0.9:1", "100.64.0.6:1"} {
		p := &probe{}
		assertDenied(t, do(UserGate(fake(), ParseAllowed("tagged-devices,tagged-devices@,"), nil)(p.Handler()), ip), p)
	}
}

func TestUserGateWhoIsError403(t *testing.T) {
	p := &probe{}
	assertDenied(t, do(UserGate(fake(), ParseAllowed("alice@"), nil)(p.Handler()), "100.64.0.8:1"), p)
}

func TestUserGateBadRemoteAddr403(t *testing.T) {
	for _, a := range []string{"garbage", "", "[::1", ":::"} {
		p := &probe{}
		assertDenied(t, do(UserGate(fake(), ParseAllowed("alice@"), nil)(p.Handler()), a), p)
	}
}

func TestUserGateNeverCallsNextOnFailure(t *testing.T) {
	g := UserGate(fake(), ParseAllowed("alice@"), nil)
	for _, a := range []string{"100.64.0.3:1", "100.64.0.4:1", "100.64.0.8:1", "x", "100.64.0.99:1"} {
		p := &probe{}
		do(g(p.Handler()), a)
		if p.called {
			t.Errorf("next called for %q", a)
		}
	}
}

func TestUserGateLoginInContext(t *testing.T) {
	tn := NewFakeTailnet(map[string]Who{userIP: {Login: "Alice@"}})
	p := &probe{}
	do(UserGate(tn, ParseAllowed("alice"), nil)(p.Handler()), userIP+":1")
	if p.login != "alice" {
		t.Fatalf("login %q", p.login)
	}
	if LoginFrom(context.Background()) != "" {
		t.Fatal("empty context should give empty login")
	}
}

func TestUserGateRefusalLogsNoIdentity(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	g := UserGate(fake(), ParseAllowed("alice@"), log)
	for _, a := range []string{"100.64.0.3:1", "100.64.0.5:1", "100.64.0.8:1", "garbage"} {
		do(g((&probe{}).Handler()), a)
	}
	do(UserGate(fake(), Allowed{}, log)((&probe{}).Handler()), userIP+":1")
	out := buf.String()
	if out == "" {
		t.Fatal("expected refusals to be logged")
	}
	for _, s := range []string{"bob", "example.test", "alice", "tag:other", "100.64", "garbage", "boom"} {
		if strings.Contains(out, s) {
			t.Errorf("log leaks %q: %s", s, out)
		}
	}
}

func TestTagGateMatrix(t *testing.T) {
	const tag = "tag:ingress-proxy"
	cases := []struct {
		name, addr string
		want       int
	}{
		{"exact", tagIP + ":1", 200},
		{"user without tag", userIP + ":1", 403},
		{"other tag", "100.64.0.5:1", 403},
		{"lookalike", "100.64.0.7:1", 403},
		{"whois error", "100.64.0.8:1", 403},
		{"bad addr", "garbage", 403},
	}
	for _, c := range cases {
		p := &probe{}
		w := do(TagGate(fake(), tag, nil)(p.Handler()), c.addr)
		if w.Code != c.want || p.called != (c.want == 200) {
			t.Errorf("%s: got %d called=%v", c.name, w.Code, p.called)
		}
		if c.want == 403 && w.Body.Len() != 0 {
			t.Errorf("%s: body %q", c.name, w.Body.String())
		}
	}
}

func TestTagGateTagsInContext(t *testing.T) {
	p := &probe{}
	do(TagGate(fake(), "tag:ingress-proxy", nil)(p.Handler()), tagIP+":1")
	if len(p.tags) != 1 || p.tags[0] != "tag:ingress-proxy" {
		t.Fatalf("tags %v", p.tags)
	}
	if NodeTagsFrom(context.Background()) != nil {
		t.Fatal("empty context should give nil tags")
	}
}

func TestRequireJSON(t *testing.T) {
	h := RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	cases := []struct {
		method, ct string
		want       int
	}{
		{"POST", "application/x-www-form-urlencoded", 422},
		{"POST", "", 422},
		{"POST", "application/json; charset=utf-8", 204},
		{"POST", "Application/JSON", 204},
		{"PUT", "text/plain", 422},
		{"PATCH", "", 422},
		{"GET", "", 204},
		{"HEAD", "", 204},
		{"DELETE", "", 422},
		{"DELETE", "application/json", 204},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "/x", nil)
		if c.ct != "" {
			r.Header.Set("Content-Type", c.ct)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s %q: got %d want %d", c.method, c.ct, w.Code, c.want)
		}
		if c.want == 422 {
			if got := w.Body.String(); got != `{"detail":"Content-Type must be application/json"}` {
				t.Errorf("body %q", got)
			}
			if w.Header().Get("Content-Type") != "application/json" {
				t.Errorf("response content type %q", w.Header().Get("Content-Type"))
			}
		}
	}
}

func TestParseAllowed(t *testing.T) {
	a := ParseAllowed(" Alice@ , ,bob@example.test,,CAROL ")
	for _, l := range []string{"alice", "alice@", "bob@example.test", "Carol@"} {
		if !a.Has(l) {
			t.Errorf("want %q allowed", l)
		}
	}
	for _, l := range []string{"", "@", "bob", "dave"} {
		if a.Has(l) {
			t.Errorf("want %q refused", l)
		}
	}
	if ParseAllowed("").Has("") || ParseAllowed(" , ").Has("alice") {
		t.Error("empty set must allow nothing")
	}
}

func TestNormalizeLogin(t *testing.T) {
	for in, want := range map[string]string{
		"Valexeev@":        "valexeev",
		"valexeev":         "valexeev",
		"  Alice@  ":       "alice",
		"alice@@":          "alice@",
		"Bob@Example.Test": "bob@example.test",
		"@":                "",
		"":                 "",
	} {
		if got := NormalizeLogin(in); got != want {
			t.Errorf("NormalizeLogin(%q)=%q want %q", in, got, want)
		}
	}
}
