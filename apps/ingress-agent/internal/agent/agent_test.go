package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The tests are a port of the Python agent's tests (#133): the same cases, the same expectations.

const (
	token = "t0ken-for-tests"
	now0  = int64(1_000_000)
	u1    = "11111111-1111-4111-8111-111111111111"
	u2    = "22222222-2222-4222-8222-222222222222"
	u3    = "33333333-3333-4333-8333-333333333333"
)

var base = []byte(`{
 "inbounds": [
  {"tag": "api", "port": 10085, "settings": {}},
  {"tag": "reality-in", "port": 443, "protocol": "vless", "settings": {"clients": [], "decryption": "none"}}
 ],
 "outbounds": [{"tag": "direct"}, {"tag": "blocked"}, {"tag": "via-exit"}],
 "routing": {"rules": [{"type": "field", "inboundTag": ["api"], "outboundTag": "api"}]}
}`)

func cred(u, mode string, notAfter int64) Cred { return Cred{UUID: u, Mode: mode, NotAfter: notAfter} }

// fakeProc stands in for the command runner: records calls, never executes anything.
type fakeProc struct {
	mu              sync.Mutex
	calls           [][]string
	testOK          bool
	statsOut        string
	restartFailures int
	statsErr        error
}

func (f *fakeProc) run(_ context.Context, args []string) (int, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, slices.Clone(args))
	switch {
	case slices.Contains(args, "-test"):
		if f.testOK {
			return 0, "", nil
		}
		return 1, "", nil
	case slices.Contains(args, "statsquery"):
		if f.statsErr != nil {
			return -1, "", f.statsErr
		}
		return 0, f.statsOut, nil
	case f.restartFailures > 0:
		f.restartFailures--
		return -1, "", errors.New("restart timed out")
	}
	return 0, "", nil
}

func (f *fakeProc) kinds() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, a := range f.calls {
		switch {
		case slices.Contains(a, "-test"):
			out = append(out, "test")
		case slices.Contains(a, "statsquery"):
			out = append(out, "stats")
		default:
			out = append(out, "restart")
		}
	}
	return out
}

func (f *fakeProc) reset() { f.mu.Lock(); f.calls = nil; f.mu.Unlock() }

type rig struct {
	t      *testing.T
	dir    string
	proc   *fakeProc
	now    int64
	a      *Applier
	srv    *Server
	logbuf *strings.Builder
}

func newRig(t *testing.T) *rig {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "config.base.json"), base, 0o640); err != nil {
		t.Fatal(err)
	}
	r := &rig{t: t, dir: d, proc: &fakeProc{testOK: true, statsOut: "{}"}, now: now0, logbuf: &strings.Builder{}}
	var mu sync.Mutex
	logw := writerFunc(func(p []byte) (int, error) { mu.Lock(); defer mu.Unlock(); return r.logbuf.Write(p) })
	r.a = &Applier{StatePath: filepath.Join(d, "state", "credentials.json"), BasePath: filepath.Join(d, "config.base.json"),
		ConfigPath: filepath.Join(d, "config.json"), Runner: &Runner{Bin: "/bin/xray", Unit: "xray", API: "127.0.0.1:10085", Run: r.proc.run},
		Clock: func() int64 { return r.now }, Log: slog.New(slog.NewTextHandler(logw, &slog.HandlerOptions{Level: slog.LevelDebug}))}
	r.srv = &Server{A: r.a, Token: token, Clock: func() int64 { return r.now }}
	return r
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func (r *rig) do(method, path string, body any, auth string) *httptest.ResponseRecorder {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = strings.NewReader(string(b))
	}
	req := httptest.NewRequest(method, path, rd)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (r *rig) put(body any, query string) *httptest.ResponseRecorder {
	return r.do("PUT", "/v1/credentials"+query, body, "Bearer "+token)
}

func bodyOf(cs ...Cred) map[string]any {
	l := []map[string]any{}
	for _, c := range cs {
		l = append(l, map[string]any{"uuid": c.UUID, "mode": c.Mode, "not_after": c.NotAfter})
	}
	return map[string]any{"credentials": l}
}

func (r *rig) state() []Cred {
	cs, err := Load(r.a.StatePath)
	if err != nil {
		r.t.Fatal(err)
	}
	return cs
}

func (r *rig) config() map[string]any {
	b, err := os.ReadFile(r.a.ConfigPath)
	if err != nil {
		r.t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

func clients(cfg map[string]any, idx int) []any {
	in := cfg["inbounds"].([]any)[idx].(map[string]any)
	return in["settings"].(map[string]any)["clients"].([]any)
}

// --- store ---

func TestDiffAndSweep(t *testing.T) {
	a, b := cred(u1, "auto", now0+1000), cred(u2, "direct", now0+1000)
	if ad, rm := Diff([]Cred{a, b}, []Cred{b, a}); len(ad)+len(rm) != 0 {
		t.Error("identical sets differ")
	}
	ad, rm := Diff([]Cred{a, cred(u2, "auto", now0+1000)}, []Cred{cred(u2, "auto", now0+1000), cred(u3, "auto", now0+1000)})
	if len(ad) != 1 || ad[0].UUID != u3 || !reflect.DeepEqual(rm, []string{u1}) {
		t.Errorf("diff %v %v", ad, rm)
	}
	ad, rm = Diff([]Cred{cred(u1, "auto", 5)}, []Cred{cred(u1, "direct", 5)})
	if len(ad) != 1 || !reflect.DeepEqual(rm, []string{u1}) {
		t.Error("a changed mode is remove and add")
	}
	keep := cred(u1, "auto", now0+1)
	if got := Sweep([]Cred{keep, cred(u2, "auto", now0), cred(u3, "auto", now0-5)}, now0); !reflect.DeepEqual(got, []Cred{keep}) {
		t.Errorf("sweep %v", got)
	}
}

func TestSaveLoadMode0600(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "credentials.json")
	if err := Save(p, []Cred{cred(u1, "auto", 9)}); err != nil {
		t.Fatal(err)
	}
	got, _ := Load(p)
	if !reflect.DeepEqual(got, []Cred{cred(u1, "auto", 9)}) {
		t.Errorf("roundtrip %v", got)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode().Perm())
	}
	if got, err := Load(filepath.Join(t.TempDir(), "missing.json")); got != nil || err != nil {
		t.Error("a missing file is an empty set")
	}
}

// --- render ---

func TestEmailIsDeterministicAndOpaque(t *testing.T) {
	sum := sha256.Sum256([]byte(u1))
	if e := EmailFor(u1); e != "u"+hex.EncodeToString(sum[:])[:12]+"@i" || strings.Contains(e, u1) {
		t.Errorf("email %q", e)
	}
}

func render(t *testing.T, b []byte, cs []Cred, ip, site []string) map[string]any {
	t.Helper()
	out, err := Render(b, cs, ip, site, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m
}

func jsonEq(t *testing.T, got any, want string) {
	t.Helper()
	var w any
	_ = json.Unmarshal([]byte(want), &w)
	gb, _ := json.Marshal(got)
	var g any
	_ = json.Unmarshal(gb, &g)
	if !reflect.DeepEqual(g, w) {
		t.Errorf("got %s\nwant %s", gb, want)
	}
}

func TestRuleOrder(t *testing.T) {
	cfg := render(t, base, []Cred{cred(u1, "direct", 9), cred(u2, "exit", 9), cred(u3, "auto", 9)}, []string{"xx", "yy"}, []string{"zz"})
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	e := EmailFor
	jsonEq(t, rules[0], dnsRule)
	jsonEq(t, rules[1], `{"type":"field","inboundTag":["reality-in"],"ip":["geoip:private","100.64.0.0/10"],"outboundTag":"blocked"}`)
	jsonEq(t, rules[2], fmt.Sprintf(`{"type":"field","user":[%q],"outboundTag":"direct"}`, e(u1)))
	jsonEq(t, rules[3], fmt.Sprintf(`{"type":"field","user":[%q],"outboundTag":"via-exit"}`, e(u2)))
	jsonEq(t, rules[4], fmt.Sprintf(`{"type":"field","user":[%q],"ip":["geoip:xx","geoip:yy"],"outboundTag":"direct"}`, e(u3)))
	jsonEq(t, rules[5], fmt.Sprintf(`{"type":"field","user":[%q],"domain":["geosite:zz"],"outboundTag":"direct"}`, e(u3)))
	jsonEq(t, rules[6], fmt.Sprintf(`{"type":"field","user":[%q],"outboundTag":"via-exit"}`, e(u3)))
	jsonEq(t, rules[7:], `[{"type":"field","inboundTag":["api"],"outboundTag":"api"}]`)
	cl := clients(cfg, 1)
	jsonEq(t, cl[0], fmt.Sprintf(`{"id":%q,"email":%q,"flow":"xtls-rprx-vision"}`, u1, e(u1)))
	if len(cl) != 3 {
		t.Errorf("clients %d", len(cl))
	}
	if cfg["routing"].(map[string]any)["domainStrategy"] != "IPOnDemand" {
		t.Error("domainStrategy is forced")
	}
}

func TestBlockRuleFirstWithZeroCreds(t *testing.T) {
	rules := render(t, base, nil, nil, nil)["routing"].(map[string]any)["rules"].([]any)
	if len(rules) != 3 || rules[1].(map[string]any)["outboundTag"] != "blocked" {
		t.Errorf("rules %v", rules)
	}
}

func TestRenderRefusals(t *testing.T) {
	if _, err := Render(base, nil, []string{"bad entry"}, nil, nil, nil); err == nil {
		t.Error("an invalid direct list entry is accepted")
	}
	if _, err := Render([]byte(`{"inbounds":[],"routing":{}}`), nil, nil, nil, nil, nil); err == nil {
		t.Error("a base without reality-in is accepted")
	}
	if _, err := Render([]byte(`{"inbounds":[{"tag":"reality-in"}]}`), nil, nil, nil, nil, nil); err == nil {
		t.Error("a base without routing is accepted")
	}
}

func TestHy2(t *testing.T) {
	var b map[string]any
	_ = json.Unmarshal(base, &b)
	b["inbounds"] = append(b["inbounds"].([]any), map[string]any{"tag": "hy2-in", "protocol": "hysteria", "settings": map[string]any{"version": 2, "clients": []any{}}})
	hb, _ := json.Marshal(b)
	cfg := render(t, hb, []Cred{cred(u1, "direct", 9), cred(u2, "exit", 9)}, nil, nil)
	jsonEq(t, clients(cfg, 2), fmt.Sprintf(`[{"auth":%q,"email":%q},{"auth":%q,"email":%q}]`, u1, EmailFor(u1), u2, EmailFor(u2)))
	jsonEq(t, cfg["routing"].(map[string]any)["rules"].([]any)[1].(map[string]any)["inboundTag"], `["reality-in","hy2-in"]`)
	b["inbounds"].([]any)[2].(map[string]any)["protocol"] = "vless"
	hb, _ = json.Marshal(b)
	if _, err := Render(hb, nil, nil, nil, nil, nil); err == nil {
		t.Error("hy2-in with another protocol is accepted")
	}
}

// --- runner ---

func TestRestartCommandOverride(t *testing.T) {
	p := &fakeProc{testOK: true}
	r := &Runner{Unit: "xray", RestartCmd: []string{"supervisorctl", "restart", "xray"}, Run: p.run}
	if err := r.Restart(context.Background()); err != nil || !reflect.DeepEqual(p.calls, [][]string{{"supervisorctl", "restart", "xray"}}) {
		t.Errorf("%v %v", err, p.calls)
	}
}

func TestStatsParse(t *testing.T) {
	p := &fakeProc{statsOut: fmt.Sprintf(`{"stat":[{"name":"user>>>%s>>>traffic>>>uplink","value":"3"},{"name":"user>>>%s>>>traffic>>>downlink","value":4},{"name":"junk"}]}`, EmailFor(u1), EmailFor(u1))}
	r := &Runner{Bin: "x", API: "a", Run: p.run}
	if got := r.Stats(context.Background()); got[EmailFor(u1)] != (Traffic{3, 4}) || len(got) != 1 {
		t.Errorf("stats %v", got)
	}
	p.statsOut = "not json"
	if len(r.Stats(context.Background())) != 0 {
		t.Error("bad output gives stats")
	}
	p.statsErr = errors.New("no xray")
	if len(r.Stats(context.Background())) != 0 {
		t.Error("an error gives stats")
	}
}

// --- API ---

func TestAuth(t *testing.T) {
	r := newRig(t)
	for _, h := range []string{"", "Bearer nope", token} {
		rec := r.do("PUT", "/v1/credentials", bodyOf(cred(u1, "auto", now0+1000)), h)
		if rec.Code != 401 || rec.Body.Len() != 0 {
			t.Errorf("auth %q: %d %q", h, rec.Code, rec.Body.String())
		}
	}
	if r.do("GET", "/v1/stats", nil, "").Code != 401 || r.do("PUT", "/v1/credentials", map[string]any{"junk": 1}, "").Code != 401 {
		t.Error("an unauthenticated request got past the token")
	}
	if len(r.proc.calls) != 0 {
		t.Error("xray was called")
	}
}

func TestEmptyNeedsConfirmation(t *testing.T) {
	r := newRig(t)
	if c := r.put(map[string]any{"credentials": []any{}}, "").Code; c != 409 {
		t.Errorf("%d", c)
	}
	if c := r.put(map[string]any{"credentials": []any{}}, "?confirm_empty=true").Code; c != 200 {
		t.Errorf("%d", c)
	}
	if c := r.put(bodyOf(cred(u1, "auto", now0)), "").Code; c != 409 {
		t.Errorf("all expired: %d", c)
	}
}

func TestValidation(t *testing.T) {
	r := newRig(t)
	bad := []any{
		map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "mars", "not_after": now0 + 5}}},
		map[string]any{"credentials": []any{map[string]any{"uuid": "not-a-uuid", "mode": "auto", "not_after": now0 + 5}}},
		map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": "soon"}}},
		map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": 1.5}}},
		map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto"}}},
		map[string]any{"nope": 1},
		// a duplicate, also when it differs only in case, and with another mode
		bodyOf(cred(u1, "auto", now0+9), cred(strings.ToUpper(u1), "auto", now0+9)),
		bodyOf(cred(u1, "auto", now0+9), cred(strings.ToUpper(u1), "direct", now0+9)),
	}
	for _, b := range bad {
		if c := r.put(b, "").Code; c != 422 {
			t.Errorf("%v: %d", b, c)
		}
	}
	if len(r.proc.calls) != 0 {
		t.Error("xray was called")
	}
}

func TestChangedPushTestsThenRestarts(t *testing.T) {
	r := newRig(t)
	rec := r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "direct", now0+1000)), "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"count":2,"tailnet_groups":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(r.proc.kinds(), []string{"test", "restart"}) || len(clients(r.config(), 1)) != 2 || len(r.state()) != 2 {
		t.Errorf("%v", r.proc.kinds())
	}
	// the validated temp file ends in .json and sits next to the live config
	path := r.proc.calls[0][4]
	if !strings.HasSuffix(path, ".json") || filepath.Dir(path) != r.dir {
		t.Errorf("temp %q", path)
	}
	if st, _ := os.Stat(r.a.StatePath); st.Mode().Perm() != 0o600 {
		t.Error("state mode")
	}
}

func TestUnchangedPushDoesNotRestart(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0+1000)), "")
	r.proc.reset()
	if c := r.put(bodyOf(cred(u1, "auto", now0+1000)), "").Code; c != 200 || len(r.proc.calls) != 0 {
		t.Errorf("%d %v", c, r.proc.calls)
	}
}

func TestExpiredDroppedOnPut(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0), cred(u2, "auto", now0+1000)), "")
	if s := r.state(); len(s) != 1 || s[0].UUID != u2 {
		t.Errorf("%v", s)
	}
}

func TestRestartFailureThenRetry(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0+1000)), "")
	saved, _ := os.ReadFile(r.a.StatePath)
	r.proc.restartFailures = 1
	if c := r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)), "").Code; c != 503 {
		t.Errorf("%d", c)
	}
	if now, _ := os.ReadFile(r.a.StatePath); string(now) != string(saved) {
		t.Error("the state changed on a failed restart")
	}
	r.proc.reset()
	if c := r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)), "").Code; c != 200 {
		t.Errorf("retry %d", c)
	}
	if !reflect.DeepEqual(r.proc.kinds(), []string{"test", "restart"}) || len(r.state()) != 2 {
		t.Errorf("%v", r.proc.kinds())
	}
}

func TestInvalidConfigKeepsEverything(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0+1000)), "")
	oldCfg, _ := os.ReadFile(r.a.ConfigPath)
	oldState, _ := os.ReadFile(r.a.StatePath)
	r.proc.testOK = false
	r.proc.reset()
	if c := r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)), "").Code; c < 500 {
		t.Errorf("%d", c)
	}
	nowCfg, _ := os.ReadFile(r.a.ConfigPath)
	nowState, _ := os.ReadFile(r.a.StatePath)
	if string(nowCfg) != string(oldCfg) || string(nowState) != string(oldState) || !reflect.DeepEqual(r.proc.kinds(), []string{"test"}) {
		t.Error("a rejected config changed something")
	}
	ents, _ := os.ReadDir(r.dir)
	if len(ents) != 3 { // base, config, state dir: no temp leftovers
		var names []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		t.Errorf("leftovers %v", names)
	}
}

func TestStatsOpaque(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)), "")
	r.proc.statsOut = fmt.Sprintf(`{"stat":[{"name":"user>>>%s>>>traffic>>>uplink","value":"3"},{"name":"user>>>%s>>>traffic>>>downlink","value":"4"},{"name":"user>>>stranger@i>>>traffic>>>uplink","value":"9"}]}`, EmailFor(u1), EmailFor(u1))
	rec := r.do("GET", "/v1/stats", nil, "Bearer "+token)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != fmt.Sprintf(`{%q:{"up":3,"down":4}}`, OpaqueID(u1)) {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	for _, u := range []string{u1, u2} {
		if strings.Contains(rec.Body.String(), u) {
			t.Error("a uuid in stats")
		}
	}
}

func TestNoOtherRoutes(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"/docs", "/openapi.json", "/redoc", "/", "/v1/other"} {
		if rec := r.do("GET", p, nil, "Bearer "+token); rec.Code != 404 || rec.Body.Len() != 0 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if rec := r.do("DELETE", "/v1/credentials", nil, ""); rec.Code != 401 {
		t.Errorf("a wrong method without a token: %d", rec.Code)
	}
}

func TestLogsContainNoUUIDs(t *testing.T) {
	r := newRig(t)
	r.put(bodyOf(cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)), "")
	r.put(bodyOf(cred(u3, "auto", now0+1000)), "")
	r.proc.testOK = false
	r.put(bodyOf(cred(u1, "auto", now0+1000)), "")
	text := r.logbuf.String()
	for _, u := range []string{u1, u2, u3} {
		if strings.Contains(text, u) || strings.Contains(text, EmailFor(u)) {
			t.Errorf("log has %s", u)
		}
	}
	if strings.Contains(text, token) || !strings.Contains(text, "applied: +2 -0") {
		t.Errorf("log %q", text)
	}
}

// --- lifecycle ---

func TestExpireSweepsAndReapplies(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	if _, err := r.a.Apply(ctx, []Cred{cred(u1, "auto", now0+10), cred(u2, "auto", now0+1000)}); err != nil {
		t.Fatal(err)
	}
	r.proc.reset()
	if _, _ = r.a.Expire(ctx); len(r.proc.calls) != 0 {
		t.Error("nothing expired yet, but xray was called")
	}
	r.now = now0 + 10
	_, _ = r.a.Expire(ctx)
	if !reflect.DeepEqual(r.proc.kinds(), []string{"test", "restart"}) || len(r.state()) != 1 || r.state()[0].UUID != u2 {
		t.Errorf("%v %v", r.proc.kinds(), r.state())
	}
}

func TestRestore(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, _ = r.a.Apply(ctx, []Cred{cred(u1, "auto", now0+1000)})
	old, _ := os.ReadFile(r.a.ConfigPath)
	_ = os.WriteFile(r.a.ConfigPath, []byte("{}"), 0o640) // config lost, state kept
	r.proc.reset()
	if err := r.a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(r.a.ConfigPath); !sameJSON(now, old) || !reflect.DeepEqual(r.proc.kinds(), []string{"test", "restart"}) {
		t.Errorf("restore %v", r.proc.kinds())
	}
	// identical: no restart
	r.proc.reset()
	_ = r.a.Restore(ctx)
	if len(r.proc.calls) != 0 {
		t.Error("an identical restore restarted xray")
	}
}

func TestRestoreAfterHalfApplied(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, _ = r.a.Apply(ctx, []Cred{cred(u1, "auto", now0+1000)})
	r.proc.restartFailures = 1
	if _, err := r.a.Apply(ctx, []Cred{cred(u1, "auto", now0+1000), cred(u2, "auto", now0+1000)}); err == nil {
		t.Fatal("a failed restart is not an error")
	}
	r.proc.reset()
	_ = r.a.Restore(ctx)
	if !reflect.DeepEqual(r.proc.kinds(), []string{"test", "restart"}) || len(clients(r.config(), 1)) != 1 {
		t.Errorf("%v", r.proc.kinds())
	}
}

// A sweep and a PUT run one after the other, so the sweep never reverts the PUT.
func TestExpireDoesNotRevertConcurrentPut(t *testing.T) {
	r := newRig(t)
	ctx := context.Background()
	_, _ = r.a.Apply(ctx, []Cred{cred(u1, "auto", now0+10)})
	r.now = now0 + 10
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = r.a.Expire(ctx) }()
		go func() { defer wg.Done(); _, _ = r.a.Apply(ctx, []Cred{cred(u2, "auto", now0+500)}) }()
	}
	wg.Wait()
	if s := r.state(); len(s) != 1 || s[0].UUID != u2 {
		t.Errorf("%v", s)
	}
}

func TestSweepLoopStops(t *testing.T) {
	r := newRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { SweepLoop(ctx, r.a, time.Millisecond); close(done) }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("the loop did not stop")
	}
}

// --- info ---

var infoEnv = map[string]string{"INGRESS_NAME": "ingress-1", "INGRESS_HOST": "relay.example.com",
	"INGRESS_PUBLIC_KEY": "PUBKEY-fake", "INGRESS_SHORT_ID": "0123456789abcdef"}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func with(m map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range m {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

func TestInfo(t *testing.T) {
	i, err := LoadInfo(envOf(infoEnv))
	if err != nil || i == nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(i)
	if string(b) != `{"name":"ingress-1","host":"relay.example.com","port":443,"sni":"relay.example.com","public_key":"PUBKEY-fake","short_id":"0123456789abcdef","api":1}` {
		t.Errorf("info %s", b)
	}
	i, _ = LoadInfo(envOf(with(infoEnv, "INGRESS_PORT", "8443", "INGRESS_SNI", "front.example.com", "INGRESS_HY2_PORT", "443")))
	if i.Port != 8443 || i.SNI != "front.example.com" || i.Hy2Port != 443 {
		t.Errorf("%+v", i)
	}
	for _, k := range []string{"INGRESS_NAME", "INGRESS_HOST", "INGRESS_PUBLIC_KEY", "INGRESS_SHORT_ID"} {
		if i, err := LoadInfo(envOf(with(infoEnv, k, ""))); i != nil || err != nil {
			t.Errorf("without %s: %v %v", k, i, err)
		}
	}
	for k, bad := range map[string]string{"INGRESS_NAME": "Bad_Name", "INGRESS_HOST": "https://x", "INGRESS_PORT": "70000",
		"INGRESS_HY2_PORT": "0", "INGRESS_SNI": "a b", "INGRESS_PUBLIC_KEY": "a b", "INGRESS_SHORT_ID": "a\tb"} {
		_, err := LoadInfo(envOf(with(infoEnv, k, bad)))
		var ie *InfoConfigError
		if !errors.As(err, &ie) || ie.Var != k || strings.Contains(err.Error(), bad) {
			t.Errorf("%s=%q: %v", k, bad, err)
		}
	}
	// the endpoint: 503 with an empty body when unconfigured, never a secret
	r := newRig(t)
	if rec := r.do("GET", "/v1/info", nil, "Bearer "+token); rec.Code != 503 || rec.Body.Len() != 0 {
		t.Errorf("unconfigured: %d", rec.Code)
	}
	r.srv.Info, _ = LoadInfo(envOf(infoEnv))
	rec := r.do("GET", "/v1/info", nil, "Bearer "+token)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), token) {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
	if r.do("GET", "/v1/info", nil, "").Code != 401 {
		t.Error("info without a token")
	}
}

// The binary has no variable for the Reality private key: the agent never reads one.
func TestNoPrivateKeyVariable(t *testing.T) {
	for _, f := range []string{"info.go", "server.go", "xray.go", "../../cmd/ingress-agent/main.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "PRIVATE_KEY") {
			t.Errorf("%s mentions PRIVATE_KEY", f)
		}
	}
}

var _ = http.StatusOK

// --- tailnet grants (#132) ---

func withTailnetOut(t *testing.T) []byte {
	t.Helper()
	var b map[string]any
	_ = json.Unmarshal(base, &b)
	b["outbounds"] = append(b["outbounds"].([]any), map[string]any{"tag": "via-tailnet", "protocol": "socks"})
	out, _ := json.Marshal(b)
	return out
}

func TestTailnetGrantRuleComesFirstAndOnlyForGranted(t *testing.T) {
	g := Cred{UUID: u1, Mode: "direct", NotAfter: 9, Tailnet: "all"}
	cfg := render(t, withTailnetOut(t), []Cred{g, cred(u2, "exit", 9)}, nil, nil)
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	jsonEq(t, rules[1], fmt.Sprintf(`{"type":"field","inboundTag":["reality-in"],"user":[%q],"ip":["100.64.0.0/10","fd7a:115c:a1e0::/48"],"outboundTag":"via-tailnet"}`, EmailFor(u1)))
	jsonEq(t, rules[2], `{"type":"field","inboundTag":["reality-in"],"ip":["geoip:private","100.64.0.0/10"],"outboundTag":"blocked"}`)
	// u2 has no grant: it is not in the tailnet rule, so the block rule catches its tailnet traffic
	if strings.Contains(fmt.Sprint(rules[1]), EmailFor(u2)) {
		t.Error("an ungranted credential is in the tailnet rule")
	}
	// private ranges other than the tailnet are not in the tailnet rule
	if strings.Contains(fmt.Sprint(rules[1]), "geoip:private") {
		t.Error("the tailnet rule must not open private ranges")
	}
}

func TestNoGrantNoTailnetRule(t *testing.T) {
	cfg := render(t, base, []Cred{cred(u1, "auto", 9)}, nil, nil)
	for _, r := range cfg["routing"].(map[string]any)["rules"].([]any) {
		if r.(map[string]any)["outboundTag"] == "via-tailnet" {
			t.Fatal("a tailnet rule without any grant")
		}
	}
}

func TestGrantWithoutOutboundIsRefused(t *testing.T) {
	if _, err := Render(base, []Cred{{UUID: u1, Mode: "auto", NotAfter: 9, Tailnet: "all"}}, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "via-tailnet") {
		t.Errorf("got %v", err)
	}
}

func TestTailnetFieldOverTheAPI(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(r.a.BasePath, withTailnetOut(t), 0o640)
	body := map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": now0 + 1000, "tailnet": "all"},
		map[string]any{"uuid": u2, "mode": "auto", "not_after": now0 + 1000}}}
	if c := r.put(body, "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	s := r.state()
	if len(s) != 2 || s[0].Tailnet != "all" || s[1].Tailnet != "" {
		t.Errorf("state %+v", s)
	}
	// a changed grant counts as a change and is applied
	r.proc.reset()
	body["credentials"].([]any)[0].(map[string]any)["tailnet"] = ""
	if c := r.put(body, "").Code; c != 200 || len(r.proc.calls) == 0 {
		t.Errorf("revoking a grant: %d %v", c, r.proc.calls)
	}
	for _, bad := range []any{"some", 1, true} {
		b := map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": now0 + 1000, "tailnet": bad}}}
		if c := r.put(b, "").Code; c != 422 {
			t.Errorf("tailnet=%v: %d", bad, c)
		}
	}
	// an old state file without the field still loads
	_ = os.WriteFile(r.a.StatePath, []byte(fmt.Sprintf(`[{"uuid":%q,"mode":"auto","not_after":%d}]`, u3, now0+1000)), 0o600)
	if s := r.state(); len(s) != 1 || s[0].Tailnet != "" {
		t.Errorf("old state %+v", s)
	}
}

// --- tailnet grant groups (#139) ---

const gA, gB = "g-0a1b2c3d", "g-deadbeef"

func withGroupOuts(t *testing.T, groups ...string) []byte {
	t.Helper()
	var b map[string]any
	_ = json.Unmarshal(withTailnetOut(t), &b)
	for i, g := range groups {
		b["outbounds"] = append(b["outbounds"].([]any), map[string]any{"tag": GroupOutbound(g), "protocol": "socks",
			"settings": map[string]any{"servers": []any{map[string]any{"address": "127.0.0.1", "port": 1056 + i}}}})
	}
	out, _ := json.Marshal(b)
	return out
}

func TestGroupRulesOnePerGroupBeforeBlock(t *testing.T) {
	cs := []Cred{{UUID: u1, Mode: "auto", NotAfter: 9, Tailnet: gA}, {UUID: u2, Mode: "auto", NotAfter: 9, Tailnet: gB},
		{UUID: u3, Mode: "auto", NotAfter: 9}}
	cfg := render(t, withGroupOuts(t, gA, gB), cs, nil, nil)
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	jsonEq(t, rules[1], fmt.Sprintf(`{"type":"field","inboundTag":["reality-in"],"user":[%q],"ip":["100.64.0.0/10","fd7a:115c:a1e0::/48"],"outboundTag":%q}`, EmailFor(u1), GroupOutbound(gA)))
	jsonEq(t, rules[2], fmt.Sprintf(`{"type":"field","inboundTag":["reality-in"],"user":[%q],"ip":["100.64.0.0/10","fd7a:115c:a1e0::/48"],"outboundTag":%q}`, EmailFor(u2), GroupOutbound(gB)))
	if rules[3].(map[string]any)["outboundTag"] != "blocked" {
		t.Errorf("the block rule must follow the group rules: %v", rules[3])
	}
}

func TestGroupWithoutOutboundIsDenied(t *testing.T) {
	cs := []Cred{{UUID: u1, Mode: "auto", NotAfter: 9, Tailnet: gB}}
	cfg := render(t, withGroupOuts(t, gA), cs, nil, nil) // only gA is provisioned
	for _, r := range cfg["routing"].(map[string]any)["rules"].([]any) {
		if tag, _ := r.(map[string]any)["outboundTag"].(string); strings.HasPrefix(tag, TailnetOut) {
			t.Fatalf("an unprovisioned group has a tailnet rule: %v", r)
		}
	}
}

func TestGroupOutboundsListsOnlyGroupIDs(t *testing.T) {
	got := GroupOutbounds(withGroupOuts(t, gB, gA))
	if strings.Join(got, ",") != gA+","+gB {
		t.Errorf("%v", got)
	}
	if len(GroupOutbounds([]byte("x"))) != 0 {
		t.Error("bad base")
	}
}

func TestGroupIDOverTheAPIAndReport(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(r.a.BasePath, withGroupOuts(t, gA), 0o640)
	body := map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": now0 + 1000, "tailnet": gA}}}
	rec := r.put(body, "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"tailnet_groups":["`+gA+`"]`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, bad := range []string{"g-XYZ", "g-0a1b2c3", "nas", "all "} {
		b := map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": now0 + 1000, "tailnet": bad}}}
		if c := r.put(b, "").Code; c != 422 {
			t.Errorf("tailnet=%q: %d", bad, c)
		}
	}
}

func TestNewGroupOutboundRendersAgain(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(r.a.BasePath, withGroupOuts(t), 0o640)
	body := map[string]any{"credentials": []any{map[string]any{"uuid": u1, "mode": "auto", "not_after": now0 + 1000, "tailnet": gA}}}
	r.put(body, "")
	n := len(r.proc.kinds())
	r.put(body, "") // nothing changed
	if len(r.proc.kinds()) != n {
		t.Fatal("an unchanged push rendered again")
	}
	_ = os.WriteFile(r.a.BasePath, withGroupOuts(t, gA), 0o640) // the playbook added the group's SOCKS
	r.put(body, "")
	if len(r.proc.kinds()) == n || !strings.Contains(string(mustRead(t, r.a.ConfigPath)), GroupOutbound(gA)+`"`) {
		t.Fatal("the new group outbound was not used")
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const dnsRule = `{"type":"field","inboundTag":["dns-internal"],"outboundTag":"via-exit"}`

// xray's own lookups (IPOnDemand resolves every domain destination for the IP rules) must go through the exit
// node, not the system resolver of the bridge host: that would show every visited name to the local network.
func TestDNSGoesThroughExit(t *testing.T) {
	cfg := render(t, base, []Cred{cred(u1, "exit", 9)}, nil, nil)
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	jsonEq(t, rules[0], dnsRule)
	jsonEq(t, cfg["dns"], `{"tag":"dns-internal","queryStrategy":"UseIPv4","servers":["https://1.1.1.1/dns-query","https://1.0.0.1/dns-query"]}`)
	if strings.Contains(fmt.Sprint(cfg["dns"]), "localhost") {
		t.Error("without direct site lists no name may use the system resolver")
	}
}

func TestDNSDirectSitesResolveLocallyOnly(t *testing.T) {
	cfg := render(t, base, []Cred{cred(u1, "auto", 9)}, []string{"xx"}, []string{"zz"})
	jsonEq(t, cfg["dns"], `{"tag":"dns-internal","queryStrategy":"UseIPv4","servers":[`+
		`{"address":"localhost","domains":["geosite:zz"],"skipFallback":true},`+
		`"https://1.1.1.1/dns-query","https://1.0.0.1/dns-query"]}`)
}

func TestDNSReplacesBaseSectionAndTakesOverride(t *testing.T) {
	b := bytes.Replace(base, []byte(`"routing"`), []byte(`"dns": {"servers": ["localhost"]}, "routing"`), 1)
	out, err := Render(b, nil, nil, nil, []string{"tcp://192.0.2.53:53", "https://192.0.2.1/dns-query"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	jsonEq(t, m["dns"], `{"tag":"dns-internal","queryStrategy":"UseIPv4","servers":["tcp://192.0.2.53:53","https://192.0.2.1/dns-query"]}`)
}

func TestDNSRefusals(t *testing.T) {
	for _, bad := range []string{"localhost", "1.1.1.1", "udp://1.1.1.1", "https://dns.example.com/dns-query",
		"tcp://dns.example.com", "https+local://1.1.1.1/dns-query", "tcp://1.1.1.1 x"} {
		if _, err := Render(base, nil, nil, nil, []string{bad}, nil); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	noExit := bytes.Replace(base, []byte(`{"tag": "via-exit"}`), []byte(`{"tag": "other"}`), 1)
	if _, err := Render(noExit, nil, nil, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "via-exit") {
		t.Errorf("a base without via-exit must be refused: %v", err)
	}
}
