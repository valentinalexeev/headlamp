package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

const (
	sessTag = "tag:browser-session"
	sessTok = "0123456789abcdef0123456789abcdef"
)

type fakeSessions struct {
	mu       sync.Mutex
	cur      map[string]*SessionInfo
	curErr   map[string]error
	startErr error
	stopErr  error
	calls    []string
	ttl      int
	idle     int
}

func (f *fakeSessions) rec(s string) { f.mu.Lock(); f.calls = append(f.calls, s); f.mu.Unlock() }

func (f *fakeSessions) Current(_ context.Context, addr string) (*SessionInfo, error) {
	f.rec("current " + addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.curErr[addr]; err != nil {
		return nil, err
	}
	return f.cur[addr], nil
}

func (f *fakeSessions) Start(_ context.Context, addr string, ttl, idle int) (SessionInfo, error) {
	f.rec("start " + addr)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ttl, f.idle = ttl, idle
	if f.startErr != nil {
		return SessionInfo{}, f.startErr
	}
	return SessionInfo{ID: "s1", State: "running", Port: 6080, Token: sessTok, ExpiresAt: time.Now().Add(time.Hour)}, nil
}

func (f *fakeSessions) Stop(_ context.Context, addr, id string) error {
	f.rec("stop " + addr + " " + id)
	return f.stopErr
}

func (f *fakeSessions) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

type sessHarness struct {
	m    *Module
	mux  *http.ServeMux
	fake *fakeSessions
	logs *bytes.Buffer
}

func sessNode(name, ip string, tags ...string) headscale.Node {
	return headscale.Node{Name: name, GivenName: name, Online: true, Tags: tags, IPAddresses: []string{ip}}
}

func newSessHarness(t *testing.T, withTag bool, nodes ...headscale.Node) *sessHarness {
	t.Helper()
	env := modEnv("redis://127.0.0.1:1")
	if withTag {
		env["SESSIONS_TAG"] = sessTag
	}
	h := &sessHarness{mux: http.NewServeMux(), fake: &fakeSessions{cur: map[string]*SessionInfo{}, curErr: map[string]error{}}, logs: &bytes.Buffer{}}
	h.m = New(core.MapEnv(env), &fakeNodes{nodes: nodes}).(*Module)
	h.m.newSessions = func() SessionClient { return h.fake }
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	if err := h.m.Register(&core.Routes{Admin: h.mux, Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: log}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	return h
}

func (h *sessHarness) do(method, path, addr, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

type sessView struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Available bool   `json:"available"`
	Session   *struct {
		ID    string `json:"id"`
		State string `json:"state"`
		Port  int    `json:"port"`
	} `json:"session"`
	Token string `json:"token"`
	URL   string `json:"url"`
}

func (h *sessHarness) list(t *testing.T) []sessView {
	t.Helper()
	rec := h.do("GET", "/sessions.json", aliceAddr, "")
	if rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Nodes []sessView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out.Nodes
}

func TestSessionsJSONListsOnlyTaggedNodes(t *testing.T) {
	h := newSessHarness(t, true,
		sessNode("b1", "100.64.0.11", sessTag),
		sessNode("plain", "100.64.0.12", DefaultRunnerTag),
		sessNode("a1", "100.64.0.10", sessTag))
	got := h.list(t)
	if len(got) != 2 || got[0].Name != "a1" || got[1].Name != "b1" {
		t.Fatalf("nodes = %+v", got)
	}
	for _, c := range h.fake.callList() {
		if strings.Contains(c, "100.64.0.12") {
			t.Fatalf("untagged node was contacted: %v", h.fake.callList())
		}
	}
	if !got[0].Available || got[0].Session != nil {
		t.Fatalf("idle node = %+v", got[0])
	}
}

func TestSessionsJSONMarksUnreachableUnavailable(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.curErr["100.64.0.10"] = errors.New("runner session status: dial: refused")
	got := h.list(t)
	if len(got) != 1 || got[0].Available || got[0].Session != nil {
		t.Fatalf("nodes = %+v", got)
	}
}

func TestSessionsJSONShowsRunningSession(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Port: 6080, Token: sessTok, ExpiresAt: time.Now().Add(time.Hour)}
	got := h.list(t)[0]
	if !got.Available || got.Session == nil || got.Session.State != "running" || got.Session.Port != 6080 {
		t.Fatalf("view = %+v", got)
	}
	if got.URL != "http://a1:6080/vnc.html?autoconnect=1&resize=remote&quality=9&path=websockify%3Ftoken%3D"+sessTok {
		t.Fatalf("url = %q", got.URL)
	}
}

func TestSessionStartOnUntaggedNode404(t *testing.T) {
	h := newSessHarness(t, true,
		sessNode("a1", "100.64.0.10", sessTag), sessNode("plain", "100.64.0.12", DefaultRunnerTag))
	for _, n := range []string{"plain", "nosuch"} {
		if rec := h.do("POST", "/sessions/"+n+"/start", aliceAddr, "{}"); rec.Code != 404 {
			t.Fatalf("%s: status %d", n, rec.Code)
		}
		if rec := h.do("POST", "/sessions/"+n+"/stop", aliceAddr, "{}"); rec.Code != 404 {
			t.Fatalf("%s stop: status %d", n, rec.Code)
		}
	}
	if c := h.fake.callList(); len(c) != 0 {
		t.Fatalf("client called: %v", c)
	}
}

func TestSessionStartDefaultsAndPassThrough(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}")
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	if h.fake.ttl != 3600 || h.fake.idle != 900 {
		t.Fatalf("defaults = %d/%d", h.fake.ttl, h.fake.idle)
	}
	var v sessView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Session == nil || v.URL == "" {
		t.Fatalf("body %s (%v)", rec.Body, err)
	}
	if rec := h.do("POST", "/sessions/a1/start", aliceAddr, `{"ttl_seconds":1800,"idle_seconds":300}`); rec.Code != 201 {
		t.Fatalf("status %d", rec.Code)
	}
	if h.fake.ttl != 1800 || h.fake.idle != 300 {
		t.Fatalf("passed = %d/%d", h.fake.ttl, h.fake.idle)
	}
}

func TestSessionStartErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrRunnerBusy, 409},
		{ErrRunnerRejected, 422},
		{ErrRunUnknown, 503},
		{ErrRunnerUnauthorized, 503},
		{errors.New("dial tcp: refused"), 503},
	}
	for _, c := range cases {
		h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
		h.fake.startErr = c.err
		if rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}"); rec.Code != c.want {
			t.Fatalf("%v: status %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func TestSessionStartBusy409(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.startErr = ErrRunnerBusy
	rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}")
	if rec.Code != 409 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestSessionStartAuditLogsUser(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Port: 6080}
	h.do("POST", "/sessions/a1/start", aliceAddr, `{"ttl_seconds":1800}`)
	h.do("POST", "/sessions/a1/stop", aliceAddr, "{}")
	out := h.logs.String()
	for _, w := range []string{"session started", "session stopped", "node=a1", "session=s1", "user=alice", "ttl_seconds=1800"} {
		if !strings.Contains(out, w) {
			t.Fatalf("log lacks %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, runnerToken) {
		t.Fatalf("token logged:\n%s", out)
	}
}

func TestSessionErrorsAreScrubbed(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.startErr = errors.New("boom " + runnerToken)
	rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}")
	if strings.Contains(rec.Body.String(), runnerToken) || strings.Contains(h.logs.String(), runnerToken) {
		t.Fatalf("token leaked: %s\n%s", rec.Body, h.logs)
	}
}

func TestSessionStopIdempotent(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	// No session at all.
	if rec := h.do("POST", "/sessions/a1/stop", aliceAddr, "{}"); rec.Code != 204 {
		t.Fatalf("no session: %d", rec.Code)
	}
	// A running one, then the runner says it is already gone.
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Port: 6080}
	h.fake.stopErr = ErrRunUnknown
	if rec := h.do("POST", "/sessions/a1/stop", aliceAddr, "{}"); rec.Code != 204 {
		t.Fatalf("unknown at runner: %d", rec.Code)
	}
	// An ended one needs no stop call.
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "ended", Reason: "ttl"}
	before := len(h.fake.callList())
	if rec := h.do("POST", "/sessions/a1/stop", aliceAddr, "{}"); rec.Code != 204 {
		t.Fatalf("ended: %d", rec.Code)
	}
	for _, c := range h.fake.callList()[before:] {
		if strings.HasPrefix(c, "stop") {
			t.Fatalf("stop sent for an ended session: %v", h.fake.callList())
		}
	}
}

func TestSessionStopUnreachable503(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.curErr["100.64.0.10"] = errors.New("refused")
	if rec := h.do("POST", "/sessions/a1/stop", aliceAddr, "{}"); rec.Code != 503 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestSessionsDisabledWithoutTag(t *testing.T) {
	h := newSessHarness(t, false, sessNode("a1", "100.64.0.10", sessTag))
	for _, p := range [][2]string{{"GET", "/sessions"}, {"GET", "/sessions.json"}, {"POST", "/sessions/a1/start"},
		{"POST", "/sessions/a1/stop"}, {"GET", "/sessions/static/sessions.js"}} {
		if rec := h.do(p[0], p[1], aliceAddr, "{}"); rec.Code != 404 {
			t.Fatalf("%s %s: %d", p[0], p[1], rec.Code)
		}
	}
	for _, n := range h.m.Nav() {
		if n.Path == "/sessions" {
			t.Fatalf("nav has the sessions page: %+v", n)
		}
	}
}

func TestSessionsNavAndPageWhenEnabled(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	found := false
	for _, n := range h.m.Nav() {
		if n.Path == "/sessions" {
			found = n.Label == "Browser" && n.Order == 45
		}
	}
	if !found {
		t.Fatalf("nav = %+v", h.m.Nav())
	}
	for path, ct := range map[string]string{"/sessions": "text/html", "/sessions/static/sessions.js": "text/javascript"} {
		rec := h.do("GET", path, aliceAddr, "")
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) || rec.Body.Len() == 0 {
			t.Fatalf("%s: %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestSessionsNeedsAdmin(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	for _, addr := range []string{bobAddr, taggedAddr} {
		for _, p := range [][2]string{{"GET", "/sessions"}, {"GET", "/sessions.json"}, {"POST", "/sessions/a1/start"}, {"POST", "/sessions/a1/stop"}} {
			if rec := h.do(p[0], p[1], addr, "{}"); rec.Code != 403 {
				t.Fatalf("%s %s %s: %d", addr, p[0], p[1], rec.Code)
			}
		}
	}
	if c := h.fake.callList(); len(c) != 0 {
		t.Fatalf("client called: %v", c)
	}
}

func TestSessionStartNeedsJSON(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	req := httptest.NewRequest("POST", "/sessions/a1/start", strings.NewReader("{}"))
	req.RemoteAddr = aliceAddr
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != 422 || len(h.fake.callList()) != 0 {
		t.Fatalf("status %d, calls %v", rec.Code, h.fake.callList())
	}
}

func TestSessionURLUsesTailnetOnly(t *testing.T) {
	cgnat := netip.MustParsePrefix("100.64.0.0/10")
	named := sessNode("a1", "100.64.0.10", sessTag)
	odd := sessNode("b1", "100.64.0.11", sessTag)
	odd.GivenName = "evil.example.com/x" // not a plain host label: the address is used
	noName := sessNode("c1", "100.64.0.12", sessTag)
	noName.GivenName = ""
	h := newSessHarness(t, true, named, odd, noName)
	for _, ip := range []string{"100.64.0.10", "100.64.0.11", "100.64.0.12"} {
		h.fake.cur[ip] = &SessionInfo{ID: "s", State: "running", Port: 6080, Token: sessTok}
	}
	for _, v := range h.list(t) {
		u := v.URL
		rest, ok := strings.CutPrefix(u, "http://")
		if !ok {
			t.Fatalf("%s: url %q", v.Name, u)
		}
		host, _, _ := strings.Cut(rest, ":")
		if a, err := netip.ParseAddr(host); err == nil {
			if !cgnat.Contains(a) {
				t.Fatalf("%s: host %s is outside the tailnet range", v.Name, host)
			}
		} else if host != "a1" {
			t.Fatalf("%s: host %q is neither the given name nor an address", v.Name, host)
		}
	}
}

func TestSessionStartNodeWithoutAddress503(t *testing.T) {
	n := sessNode("a1", "100.64.0.10", sessTag)
	n.IPAddresses = nil
	h := newSessHarness(t, true, n)
	if rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}"); rec.Code != 503 {
		t.Fatalf("status %d", rec.Code)
	}
	if got := h.list(t); len(got) != 1 || got[0].Available {
		t.Fatalf("nodes = %+v", got)
	}
}

func TestSessionsInvalidTagRefused(t *testing.T) {
	env := modEnv("redis://127.0.0.1:1")
	env["SESSIONS_TAG"] = "browser"
	m := New(core.MapEnv(env), &fakeNodes{}).(*Module)
	err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet()})
	if err == nil || !strings.Contains(err.Error(), "SESSIONS_TAG") {
		t.Fatalf("err = %v", err)
	}
}

func TestSessionURLCarriesToken(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	rec := h.do("POST", "/sessions/a1/start", aliceAddr, "{}")
	if rec.Code != 201 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var v sessView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(v.URL, "path=websockify%3Ftoken%3D"+sessTok) {
		t.Fatalf("url = %q", v.URL)
	}
}

func TestSessionTokenNotLogged(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Port: 6080, Token: sessTok}
	h.do("POST", "/sessions/a1/start", aliceAddr, "{}")
	h.list(t)
	h.do("POST", "/sessions/a1/stop", aliceAddr, "{}")
	if out := h.logs.String(); !strings.Contains(out, "session started") || strings.Contains(out, sessTok) {
		t.Fatalf("log must exist and not hold the token:\n%s", out)
	}
}

func TestSessionTokenScrubbedFromErrors(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag))
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Port: 6080, Token: sessTok}
	h.fake.stopErr = errors.New("boom " + sessTok)
	rec := h.do("POST", "/sessions/a1/stop", aliceAddr, "{}")
	if rec.Code != 503 || strings.Contains(rec.Body.String(), sessTok) || strings.Contains(h.logs.String(), sessTok) {
		t.Fatalf("token leaked (%d): %s\n%s", rec.Code, rec.Body, h.logs)
	}
	if !strings.Contains(h.logs.String(), "***") {
		t.Fatalf("expected a scrubbed error in the log:\n%s", h.logs)
	}
}

func TestSessionsJSONNoTokenWhenNoSession(t *testing.T) {
	h := newSessHarness(t, true, sessNode("a1", "100.64.0.10", sessTag), sessNode("b1", "100.64.0.11", sessTag))
	h.fake.cur["100.64.0.11"] = &SessionInfo{ID: "s1", State: "ended", Reason: "ttl", Port: 6080, Token: sessTok}
	rec := h.do("GET", "/sessions.json", aliceAddr, "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), sessTok) || strings.Contains(rec.Body.String(), "token") {
		t.Fatalf("no running session must mean no token (%d): %s", rec.Code, rec.Body)
	}
}
