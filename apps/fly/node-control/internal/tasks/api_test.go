package tasks

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

const (
	aliceAddr  = "100.64.0.1:5555"
	bobAddr    = "100.64.0.2:5555"
	taggedAddr = "100.64.0.3:5555"

	runnerToken = "runner-token-value-xyz"
)

var modStateKey = base64.StdEncoding.EncodeToString([]byte("abcdef0123456789abcdef0123456789"))

func gateTailnet() *authz.FakeTailnet {
	return authz.NewFakeTailnet(map[string]authz.Who{
		"100.64.0.1": {Login: "alice@"},
		"100.64.0.2": {Login: "bob@"},
		"100.64.0.3": {Login: "alice@", Tags: []string{"tag:server"}},
	})
}

func modEnv(redisURL string) map[string]string {
	return map[string]string{
		"REDIS_URL":              redisURL,
		"NODE_CONTROL_STATE_KEY": modStateKey,
		"TASKS_RUNNER_TOKEN":     runnerToken,
		"TASKS_ADMIN_USERS":      "alice",
		"ENROLL_ALLOWED_HOSTS":   "node-control.example.com",
	}
}

type modHarness struct {
	t        *testing.T
	m        *Module
	mux      *http.ServeMux
	runner   *fakeRunner
	notifier *fakeNotifier
	mr       *miniredis.Miniredis
	logs     *bytes.Buffer
}

func testNodes() headscale.NodeSource {
	return &fakeNodes{nodes: []headscale.Node{runnerNode("n1", "100.64.0.10")}}
}

// newModHarness registers and starts the module against miniredis with a fake
// runner and notifier, and waits until the store is open.
func newModHarness(t *testing.T, env map[string]string) *modHarness {
	t.Helper()
	return newModHarnessEnv(t, env, nil)
}

// newModHarnessEnv is newModHarness with mod applied to the environment
// (the default one when env is nil) before Register.
func newModHarnessEnv(t *testing.T, env map[string]string, mod func(map[string]string)) *modHarness {
	t.Helper()
	mr := miniredis.RunT(t)
	if env == nil {
		env = modEnv("redis://" + mr.Addr())
	}
	if mod != nil {
		mod(env)
	}
	h := &modHarness{t: t, mux: http.NewServeMux(), runner: &fakeRunner{}, notifier: &fakeNotifier{}, mr: mr,
		logs: &bytes.Buffer{}}
	h.m = New(core.MapEnv(env), testNodes()).(*Module)
	h.m.newRunner = func() Runner { return h.runner }
	h.m.newNotifier = func(*Store) Notifier { return h.notifier }
	h.m.pollEvery = 2 * time.Millisecond
	log := slog.New(slog.NewTextHandler(h.logs, nil))
	if err := h.m.Register(&core.Routes{Admin: h.mux, Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: log}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := h.m.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.m.Stop(ctx)
	})
	return h
}

func (h *modHarness) waitOpen() *Store {
	h.t.Helper()
	waitFor(h.t, "store open", func() bool { s, _ := h.m.current(); return s != nil })
	s, _ := h.m.current()
	// Keep the scheduler away from "job": the tests start it by hand.
	if err := s.SaveState("job", TaskState{NextRun: time.Now().Add(24 * time.Hour)}); err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *modHarness) do(method, path, addr, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	if method == "PUT" || method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *modHarness) alice(method, path, body string) *httptest.ResponseRecorder {
	return h.do(method, path, aliceAddr, body)
}

func decodeMap(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return m
}

func strList(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	sort.Strings(out)
	return out
}

const (
	scriptA = "module.exports.run = async () => ({ balance: 1 })"
	scriptB = "module.exports.run = async () => ({ other: 2 })"
)

// fullTask references every kind of secret name a task can carry.
const fullTask = `{"script":"check-balance.js","target":{"node":"n1"},"schedule":{"every":"1h"},
 "secrets":["API_TOKEN"],"healthcheck":"HC_URL",
 "alert":{"when":{"field":"balance","op":"<","value":10},"notify":{"topic":"NTFY_TOPIC","title":"low"}},
 "on_failure":{"notify":{"topic":"FAIL_TOPIC"}}}`

func configBody(tasks string, scripts map[string]string) string {
	b, _ := json.Marshal(scripts)
	return `{"config":{"tasks":{` + tasks + `}},"scripts":` + string(b) + `}`
}

func (h *modHarness) putSecrets(names ...string) {
	h.t.Helper()
	m := map[string]string{}
	for _, n := range names {
		m[n] = "value-of-" + n
	}
	b, _ := json.Marshal(m)
	if rec := h.alice("PUT", "/tasks/secrets", string(b)); rec.Code != 200 {
		h.t.Fatalf("put secrets: %d %s", rec.Code, rec.Body)
	}
}

func (h *modHarness) pushFull() {
	h.t.Helper()
	h.putSecrets("API_TOKEN", "HC_URL", "NTFY_TOPIC", "FAIL_TOPIC")
	rec := h.alice("PUT", "/tasks/config", configBody(`"job":`+fullTask, map[string]string{"check-balance.js": scriptA}))
	if rec.Code != 200 {
		h.t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
}

// --- activation and degradation ---------------------------------------------

func TestTasksInactiveWithoutEnv(t *testing.T) {
	for _, missing := range []string{"REDIS_URL", "NODE_CONTROL_STATE_KEY", "TASKS_RUNNER_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			env := modEnv("redis://user:hunter2-pass@127.0.0.1:1")
			delete(env, missing)
			var logs bytes.Buffer
			mux := http.NewServeMux()
			m := New(core.MapEnv(env), testNodes())
			err := m.Register(&core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: gateTailnet(),
				Log: slog.New(slog.NewTextHandler(&logs, nil))})
			if err != nil {
				t.Fatalf("Register: %v", err)
			}
			if nav := m.(core.Navigator).Nav(); len(nav) != 0 {
				t.Fatalf("nav = %+v", nav)
			}
			if err := m.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			if err := m.(core.Stopper).Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest("GET", "/tasks", nil)
			req.RemoteAddr = aliceAddr
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != 404 {
				t.Fatalf("GET /tasks = %d, want 404 (no route)", rec.Code)
			}
			out := logs.String()
			if !strings.Contains(out, missing) || strings.Count(out, "\n") != 1 {
				t.Fatalf("want one log line naming %s, got %q", missing, out)
			}
			for _, v := range []string{"hunter2", modStateKey, runnerToken} {
				if strings.Contains(out, v) {
					t.Fatalf("log carries a value: %q", out)
				}
			}
		})
	}
}

func TestTasksRequiresAdminUsersWhenActive(t *testing.T) {
	env := modEnv("redis://127.0.0.1:1")
	env["TASKS_ADMIN_USERS"] = " , "
	m := New(core.MapEnv(env), testNodes())
	err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet()})
	if err == nil || !strings.Contains(err.Error(), "TASKS_ADMIN_USERS") {
		t.Fatalf("err = %v", err)
	}
	for k, v := range map[string]string{"TASKS_RUNNER_PORT": "0", "TASKS_POLL_SECONDS": "61"} {
		env := modEnv("redis://127.0.0.1:1")
		env[k] = v
		m := New(core.MapEnv(env), testNodes())
		err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet()})
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("%s=%s: err = %v", k, v, err)
		}
	}
}

type otherModule struct{}

func (otherModule) Name() string { return "other" }
func (otherModule) Register(r *core.Routes) error {
	r.Admin.HandleFunc("GET /other", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return nil
}
func (otherModule) Start(context.Context) error { return nil }

func closedPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestTasksDegradedWhenRedisDown(t *testing.T) {
	env := modEnv("redis://:pw-secret-123@" + closedPort(t))
	var logs bytes.Buffer
	mux := http.NewServeMux()
	routes := &core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: gateTailnet(),
		Log: slog.New(slog.NewTextHandler(&logs, nil))}
	tm := New(core.MapEnv(env), testNodes()).(*Module)
	opened := make(chan struct{}, 4)
	tm.onOpenFail = func() { opened <- struct{}{} }
	mods := []core.Module{tm, otherModule{}}
	for _, m := range mods {
		if err := m.Register(routes); err != nil {
			t.Fatalf("Register %s: %v", m.Name(), err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := time.Now()
	for _, m := range mods {
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(begin); d > 500*time.Millisecond {
		t.Fatalf("Start took %s", d)
	}
	select {
	case <-opened:
	case <-time.After(15 * time.Second):
		t.Fatal("no open attempt")
	}
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = aliceAddr
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}
	if rec := get("/other"); rec.Code != 200 {
		t.Fatalf("other module: %d", rec.Code)
	}
	for _, p := range []string{"/tasks.json", "/tasks/job", "/tasks/job/runs", "/runs/abc", "/tasks/config", "/tasks/secrets"} {
		rec := get(p)
		if rec.Code != 503 || decodeMap(t, rec)["error"] != "degraded" {
			t.Fatalf("GET %s = %d %s", p, rec.Code, rec.Body)
		}
	}
	req := httptest.NewRequest("POST", "/tasks/job/run", nil)
	req.RemoteAddr = aliceAddr
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("POST run = %d", rec.Code)
	}
	// The placeholder page is served regardless.
	req = httptest.NewRequest("GET", "/tasks", nil)
	req.RemoteAddr = aliceAddr
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("page = %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	if nav := tm.Nav(); len(nav) != 1 || nav[0].Path != "/tasks" {
		t.Fatalf("nav = %+v", nav)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	if err := tm.Stop(sctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(logs.String(), "pw-secret-123") || strings.Contains(logs.String(), modStateKey) {
		t.Fatalf("log carries a secret: %s", logs.String())
	}
}

func TestTasksRequireAllowedUser(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	routes := []struct{ method, path string }{
		{"GET", "/tasks"}, {"GET", "/tasks.json"}, {"GET", "/tasks/job"}, {"POST", "/tasks/job/run"}, {"GET", "/tasks/job/runs"},
		{"GET", "/runs/x"}, {"DELETE", "/runs/x"}, {"GET", "/tasks/config"}, {"PUT", "/tasks/config"},
		{"PUT", "/tasks/secrets"}, {"GET", "/tasks/secrets"},
	}
	for _, r := range routes {
		for _, addr := range []string{bobAddr, taggedAddr} {
			if rec := h.do(r.method, r.path, addr, `{}`); rec.Code != 403 {
				t.Errorf("%s %s from %s = %d", r.method, r.path, addr, rec.Code)
			}
		}
	}
	// PUT and POST need a JSON content type.
	req := httptest.NewRequest("PUT", "/tasks/secrets", strings.NewReader(`{"A":"b"}`))
	req.RemoteAddr = aliceAddr
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	if rec.Code != 422 {
		t.Fatalf("form PUT = %d", rec.Code)
	}
	for _, p := range []string{"/tasks", "/tasks.json"} {
		if rec := h.alice("GET", p, ""); rec.Code != 200 {
			t.Fatalf("alice GET %s = %d %s", p, rec.Code, rec.Body)
		}
	}
}

// --- config ------------------------------------------------------------------

func TestConfigDryRunDoesNotStore(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	h.putSecrets("API_TOKEN", "HC_URL", "NTFY_TOPIC", "FAIL_TOPIC")
	body := configBody(`"job":`+fullTask, map[string]string{"check-balance.js": scriptA})
	rec := h.alice("PUT", "/tasks/config?dry_run=1", body)
	if rec.Code != 200 {
		t.Fatalf("dry run: %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec)
	if got["dry_run"] != true {
		t.Fatalf("dry_run = %v", got["dry_run"])
	}
	tasks := got["tasks"].(map[string]any)
	if a := strList(tasks["added"]); len(a) != 1 || a[0] != "job" {
		t.Fatalf("tasks = %v", tasks)
	}
	if a := strList(got["scripts"].(map[string]any)["added"]); len(a) != 1 || a[0] != "check-balance.js" {
		t.Fatalf("scripts = %v", got["scripts"])
	}
	if store.Config() != nil {
		t.Fatal("dry run stored the config")
	}
	if _, ok := store.Script("check-balance.js"); ok {
		t.Fatal("dry run stored the script")
	}
	if rec := h.alice("GET", "/tasks/config", ""); rec.Code != 404 {
		t.Fatalf("GET config after dry run = %d", rec.Code)
	}

	// A real push stores; a second push of the same content changes nothing.
	if rec := h.alice("PUT", "/tasks/config", body); rec.Code != 200 || decodeMap(t, rec)["dry_run"] != false {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	if store.Config() == nil {
		t.Fatal("push did not store")
	}
	rec = h.alice("PUT", "/tasks/config?dry_run=1", strings.Replace(body, `"1h"`, `"2h"`, 1))
	got = decodeMap(t, rec)
	if c := strList(got["tasks"].(map[string]any)["changed"]); len(c) != 1 || c[0] != "job" {
		t.Fatalf("changed = %v", got)
	}
	rec = h.alice("PUT", "/tasks/config", body)
	got = decodeMap(t, rec)
	for _, k := range []string{"added", "changed", "removed"} {
		if len(got["tasks"].(map[string]any)[k].([]any)) != 0 || len(got["scripts"].(map[string]any)[k].([]any)) != 0 {
			t.Fatalf("second push not a no-op: %s", rec.Body)
		}
	}
}

func TestConfigPutRejectsInvalidAndNamesProblems(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	body := configBody(`"a":{"script":"x.js","target":{},"schedule":{"every":"1s"}},
		"b":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"},"timeout":"99h"}`,
		map[string]string{"x.js": scriptA})
	rec := h.alice("PUT", "/tasks/config", body)
	if rec.Code != 422 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	got := decodeMap(t, rec)
	probs := strings.Join(strList(got["problems"]), "\n")
	for _, want := range []string{"task a: target", "task a: schedule.every", "task b: timeout"} {
		if !strings.Contains(probs, want) {
			t.Errorf("problems lack %q: %s", want, probs)
		}
	}
	if got["error"] == "" || store.Config() != nil {
		t.Fatalf("error %v / stored %v", got["error"], store.Config())
	}
	// Bad script names and oversize scripts.
	rec = h.alice("PUT", "/tasks/config", configBody(`"a":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"../x.js": scriptA, "x.js": scriptA}))
	if rec.Code != 422 {
		t.Fatalf("bad script name: %d %s", rec.Code, rec.Body)
	}
	rec = h.alice("PUT", "/tasks/config", configBody(`"a":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"x.js": strings.Repeat("a", 256<<10+1)}))
	if rec.Code != 413 {
		t.Fatalf("big script: %d %s", rec.Code, rec.Body)
	}
	// Reserved task ids would shadow the config and secrets routes.
	rec = h.alice("PUT", "/tasks/config", configBody(`"config":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"x.js": scriptA}))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "reserved") {
		t.Fatalf("reserved id: %d %s", rec.Code, rec.Body)
	}
	// "scheduler" is the heartbeat's task label in metrics and logs.
	rec = h.alice("PUT", "/tasks/config", configBody(`"scheduler":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"x.js": scriptA}))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "task scheduler: id: reserved") {
		t.Fatalf("reserved id scheduler: %d %s", rec.Code, rec.Body)
	}
	if rec := h.alice("PUT", "/tasks/config", `not json`); rec.Code != 422 {
		t.Fatalf("garbage: %d", rec.Code)
	}
	if rec := h.alice("PUT", "/tasks/config", `{"scripts":{}}`); rec.Code != 422 {
		t.Fatalf("no config: %d", rec.Code)
	}
}

func TestConfigRejectsMissingScript(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	task := `"a":{"script":"check-balance.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`
	rec := h.alice("PUT", "/tasks/config?dry_run=1", configBody(task, map[string]string{}))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "check-balance.js") {
		t.Fatalf("missing script: %d %s", rec.Code, rec.Body)
	}
	// Once stored, a later push may omit it.
	if rec := h.alice("PUT", "/tasks/config", configBody(task, map[string]string{"check-balance.js": scriptA})); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	task2 := task + `,"b":{"script":"check-balance.js","target":{"node":"n1"},"schedule":{"every":"2h"}}`
	if rec := h.alice("PUT", "/tasks/config", configBody(task2, nil)); rec.Code != 200 {
		t.Fatalf("push without script: %d %s", rec.Code, rec.Body)
	}
}

func TestConfigRejectsUnknownSecretName(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	body := configBody(`"job":`+fullTask, map[string]string{"check-balance.js": scriptA})
	rec := h.alice("PUT", "/tasks/config", body)
	if rec.Code != 422 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	probs := strings.Join(strList(decodeMap(t, rec)["problems"]), "\n")
	for _, n := range []string{"API_TOKEN", "HC_URL", "NTFY_TOPIC", "FAIL_TOPIC"} {
		if !strings.Contains(probs, n) {
			t.Errorf("problems lack %s: %s", n, probs)
		}
	}
	h.putSecrets("API_TOKEN", "HC_URL", "NTFY_TOPIC")
	rec = h.alice("PUT", "/tasks/config", body)
	probs = strings.Join(strList(decodeMap(t, rec)["problems"]), "\n")
	if rec.Code != 422 || !strings.Contains(probs, "FAIL_TOPIC") || strings.Contains(probs, "NTFY_TOPIC") {
		t.Fatalf("after partial secrets: %d %s", rec.Code, probs)
	}
	if store.Config() != nil {
		t.Fatal("stored despite missing secrets")
	}
	h.putSecrets("FAIL_TOPIC")
	if rec := h.alice("PUT", "/tasks/config", body); rec.Code != 200 {
		t.Fatalf("with all secrets: %d %s", rec.Code, rec.Body)
	}
}

// scripts/tasks.sh recognises a not-yet-pushed secret by this exact expression (MISSING_SECRET_RE there). Keep the two
// in step: if the wording of the problem changes, this test fails and the script must change with it.
var missingSecretRe = regexp.MustCompile(`^task .*: secret ([A-Za-z0-9_]+) is not stored`)

func TestUnknownSecretProblemWordingMatchesCLI(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	body := configBody(`"job":`+fullTask, map[string]string{"check-balance.js": scriptA})
	rec := h.alice("PUT", "/tasks/config?dry_run=1", body)
	if rec.Code != 422 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	got := map[string]bool{}
	for _, p := range strList(decodeMap(t, rec)["problems"]) {
		m := missingSecretRe.FindStringSubmatch(p)
		if m == nil {
			t.Fatalf("problem %q does not match the CLI's missing-secret expression", p)
		}
		got[m[1]] = true
	}
	for _, n := range []string{"API_TOKEN", "HC_URL", "NTFY_TOPIC", "FAIL_TOPIC"} {
		if !got[n] {
			t.Errorf("secret %s not reported in the expected wording", n)
		}
	}
}

func TestConfigGetReturnsReferencedScripts(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	if rec := h.alice("GET", "/tasks/config", ""); rec.Code != 404 || decodeMap(t, rec)["error"] != "no config" {
		t.Fatalf("empty: %d %s", rec.Code, rec.Body)
	}
	two := `"a":{"script":"a.js","target":{"node":"n1"},"schedule":{"every":"1h"}},
		"b":{"script":"b.js","target":{"tags":["tag:region-xx"]},"schedule":{"every":"1h"}}`
	if rec := h.alice("PUT", "/tasks/config", configBody(two, map[string]string{"a.js": scriptA, "b.js": scriptB})); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	one := `"a":{"script":"a.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`
	rec := h.alice("PUT", "/tasks/config", configBody(one, nil))
	got := decodeMap(t, rec)
	if r := strList(got["tasks"].(map[string]any)["removed"]); len(r) != 1 || r[0] != "b" {
		t.Fatalf("removed tasks: %s", rec.Body)
	}
	if r := strList(got["scripts"].(map[string]any)["removed"]); len(r) != 1 || r[0] != "b.js" {
		t.Fatalf("removed scripts: %s", rec.Body)
	}
	rec = h.alice("GET", "/tasks/config", "")
	if rec.Code != 200 {
		t.Fatalf("get: %d", rec.Code)
	}
	var out struct {
		Config  Config            `json:"config"`
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Config.Tasks) != 1 || out.Config.Tasks["a"].Script != "a.js" {
		t.Fatalf("config = %+v", out.Config)
	}
	if len(out.Scripts) != 1 || out.Scripts["a.js"] != scriptA {
		t.Fatalf("scripts = %v", out.Scripts)
	}
}

// --- secrets -----------------------------------------------------------------

func TestSecretsNeverReturned(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	values := map[string]string{"API_TOKEN": "tok-SECRETVALUE-1", "HC_URL": "https://hc.example.com/SECRETVALUE-2",
		"NTFY_TOPIC": "topic-SECRETVALUE-3", "FAIL_TOPIC": "topic-SECRETVALUE-4"}
	b, _ := json.Marshal(values)
	rec := h.alice("PUT", "/tasks/secrets", string(b))
	if rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if got := strList(decodeMap(t, rec)["stored"]); strings.Join(got, ",") != "API_TOKEN,FAIL_TOPIC,HC_URL,NTFY_TOPIC" {
		t.Fatalf("stored = %v", got)
	}
	if rec := h.alice("PUT", "/tasks/config", configBody(`"job":`+fullTask, map[string]string{"check-balance.js": scriptA})); rec.Code != 200 {
		t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
	rec = h.alice("GET", "/tasks/secrets", "")
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 4 {
		t.Fatalf("list %s: %v", rec.Body, err)
	}
	for _, e := range list {
		if e["set"] != true || len(e) != 2 {
			t.Fatalf("entry %v", e)
		}
	}
	// Bad requests do not echo values either.
	for _, bad := range []string{`{"bad name":"SECRETVALUE-9"}`, `{"A":""}`, `{"A":"` + strings.Repeat("x", 4097) + `"}`, `{"A":1}`, `[]`} {
		rec := h.alice("PUT", "/tasks/secrets", bad)
		if rec.Code != 422 || strings.Contains(rec.Body.String(), "SECRETVALUE") || strings.Contains(rec.Body.String(), "xxxx") {
			t.Fatalf("bad %q: %d %s", bad, rec.Code, rec.Body)
		}
	}
	many := map[string]string{}
	for i := 0; i < 65; i++ {
		many["N"+string(rune('A'+i/26))+string(rune('A'+i%26))] = "v"
	}
	b, _ = json.Marshal(many)
	if rec := h.alice("PUT", "/tasks/secrets", string(b)); rec.Code != 422 {
		t.Fatalf("65 entries: %d", rec.Code)
	}
	if rec := h.alice("PUT", "/tasks/secrets", `{"A":"`+strings.Repeat("x", 70<<10)+`"}`); rec.Code != 413 {
		t.Fatalf("oversize body: %d", rec.Code)
	}

	if _, err := h.m.engineFor().Trigger("job"); err != nil {
		t.Fatal(err)
	}
	h.m.engineFor().wait()
	for _, p := range []string{"/tasks", "/tasks.json", "/tasks/job", "/tasks/job/runs", "/tasks/config", "/tasks/secrets"} {
		rec := h.alice("GET", p, "")
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "SECRETVALUE") {
			t.Fatalf("GET %s = %d leaks: %s", p, rec.Code, rec.Body)
		}
	}
	if strings.Contains(h.logs.String(), "SECRETVALUE") {
		t.Fatalf("log leaks: %s", h.logs)
	}
}

// --- runs --------------------------------------------------------------------

func (h *modHarness) pushSimple() {
	h.t.Helper()
	rec := h.alice("PUT", "/tasks/config", configBody(`"job":{"script":"check-balance.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"check-balance.js": scriptA}))
	if rec.Code != 200 {
		h.t.Fatalf("push: %d %s", rec.Code, rec.Body)
	}
}

func TestRunTriggerConflict409(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	h.pushSimple()
	polled := make(chan struct{}, 1)
	h.runner.status = blockRunning(polled)
	rec := h.alice("POST", "/tasks/job/run", "")
	if rec.Code != 202 {
		t.Fatalf("trigger: %d %s", rec.Code, rec.Body)
	}
	id, _ := decodeMap(t, rec)["run_id"].(string)
	if id == "" {
		t.Fatalf("no run id: %s", rec.Body)
	}
	if rec := h.alice("POST", "/tasks/job/run", ""); rec.Code != 409 {
		t.Fatalf("second trigger: %d %s", rec.Code, rec.Body)
	}
	if rec := h.alice("POST", "/tasks/nope/run", ""); rec.Code != 404 || decodeMap(t, rec)["error"] != "unknown task" {
		t.Fatalf("unknown: %d %s", rec.Code, rec.Body)
	}
	<-polled
	rec = h.alice("GET", "/tasks/job", "")
	got := decodeMap(t, rec)
	rr, _ := got["running_run"].(map[string]any)
	if got["running"] != true || rr["id"] != id || rr["state"] != "running" {
		t.Fatalf("task view while running: %s", rec.Body)
	}
	if rec := h.alice("GET", "/runs/"+id, ""); rec.Code != 200 || decodeMap(t, rec)["id"] != id {
		t.Fatalf("GET run: %d %s", rec.Code, rec.Body)
	}
	if rec := h.alice("GET", "/tasks/nope", ""); rec.Code != 404 {
		t.Fatalf("unknown task view: %d", rec.Code)
	}
}

func TestRunCancelEndpoint(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	h.pushSimple()
	polled := make(chan struct{}, 1)
	h.runner.status = blockRunning(polled)
	id, _ := decodeMap(t, h.alice("POST", "/tasks/job/run", ""))["run_id"].(string)
	<-polled
	if rec := h.alice("DELETE", "/runs/"+id, ""); rec.Code != 204 {
		t.Fatalf("cancel: %d %s", rec.Code, rec.Body)
	}
	h.m.engineFor().wait()
	rec := h.alice("GET", "/runs/"+id, "")
	if got := decodeMap(t, rec); got["state"] != "cancelled" || got["reason"] != "cancelled by operator" {
		t.Fatalf("after cancel: %s", rec.Body)
	}
	if rec := h.alice("DELETE", "/runs/"+id, ""); rec.Code != 404 {
		t.Fatalf("cancel finished: %d", rec.Code)
	}
	if rec := h.alice("DELETE", "/runs/unknown", ""); rec.Code != 404 {
		t.Fatalf("cancel unknown: %d", rec.Code)
	}
	// A run recorded as running that this process is not executing cannot be cancelled.
	if err := store.AppendRun(Run{ID: "ghost", Task: "job", State: RunRunning, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if rec := h.alice("DELETE", "/runs/ghost", ""); rec.Code != 409 {
		t.Fatalf("cancel ghost: %d %s", rec.Code, rec.Body)
	}
}

func TestRunsViewHasNoRunnerFields(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	h.pushSimple()
	start := time.Date(2030, 1, 1, 10, 0, 0, 0, time.UTC)
	for i, r := range []Run{
		{ID: "r1", Task: "job", Node: "n1", Attempt: 1, State: RunSucceeded, Started: start, Ended: start.Add(1500 * time.Millisecond),
			Result: map[string]any{"balance": float64(5)}, ScriptSHA: "abc", NotifyOutcome: "alert=sent", RunnerID: "rid-1", RunnerAddr: "100.64.0.10"},
		{ID: "r2", Task: "job", Node: "n1", Attempt: 1, State: RunFailed, Reason: "timeout", Started: start.Add(time.Hour),
			Ended: start.Add(time.Hour + time.Second), RunnerID: "rid-2", RunnerAddr: "100.64.0.10"},
	} {
		if err := store.AppendRun(r); err != nil {
			t.Fatal(i, err)
		}
	}
	rec := h.alice("GET", "/tasks/job/runs", "")
	if rec.Code != 200 || strings.Contains(strings.ToLower(rec.Body.String()), "runner") || strings.Contains(rec.Body.String(), "rid-") {
		t.Fatalf("runs: %d %s", rec.Code, rec.Body)
	}
	var runs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil || len(runs) != 2 {
		t.Fatalf("%s: %v", rec.Body, err)
	}
	want := []string{"attempt", "duration_ms", "ended", "id", "node", "notify_outcome", "reason", "result", "script_sha", "started", "state", "task"}
	var keys []string
	for k := range runs[1] {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("keys = %v", keys)
	}
	if runs[0]["id"] != "r2" || runs[1]["duration_ms"] != float64(1500) || runs[1]["started"] != "2030-01-01T10:00:00Z" {
		t.Fatalf("runs = %v", runs)
	}
	rec = h.alice("GET", "/runs/r1", "")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "rid-") {
		t.Fatalf("run: %d %s", rec.Code, rec.Body)
	}
	if rec := h.alice("GET", "/runs/none", ""); rec.Code != 404 {
		t.Fatalf("unknown run: %d", rec.Code)
	}
	// The task view picks the newest succeeded run's result.
	got := decodeMap(t, h.alice("GET", "/tasks/job", ""))
	if lr, _ := got["last_result"].(map[string]any); lr["balance"] != float64(5) {
		t.Fatalf("last_result = %v", got["last_result"])
	}
}

// --- stale -------------------------------------------------------------------

func TestStaleFlag(t *testing.T) {
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	every := time.Hour
	cases := []struct {
		name      string
		st        TaskState
		running   bool
		ranBefore bool
		want      bool
	}{
		{"new task, never ran", TaskState{}, false, false, false},
		{"recent success", TaskState{LastSuccess: now.Add(-90 * time.Minute)}, false, true, false},
		{"success exactly 2x ago", TaskState{LastSuccess: now.Add(-2 * time.Hour)}, false, true, false},
		{"success older than 2x", TaskState{LastSuccess: now.Add(-2*time.Hour - time.Second)}, false, true, true},
		{"old success while running", TaskState{LastSuccess: now.Add(-3 * time.Hour)}, true, true, true},
		{"never succeeded, failed before", TaskState{NextRun: now.Add(time.Hour), Streak: 1}, false, true, true},
		{"never succeeded, first run due long ago, idle", TaskState{NextRun: now.Add(-2 * time.Minute)}, false, false, true},
		{"never succeeded, first run due long ago, running", TaskState{NextRun: now.Add(-2 * time.Minute)}, true, false, false},
		{"never succeeded, first run just due", TaskState{NextRun: now.Add(-30 * time.Second)}, false, false, false},
		{"never succeeded, first run in the future", TaskState{NextRun: now.Add(time.Minute)}, false, false, false},
	}
	for _, c := range cases {
		if got := isStale(now, every, c.st, c.running, c.ranBefore); got != c.want {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTaskListView(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	h.pushSimple()
	if err := store.SaveState("job", TaskState{Attempt: 2, AttemptAt: time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC),
		NextRun: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC), Streak: 3}); err != nil {
		t.Fatal(err)
	}
	rec := h.alice("GET", "/tasks.json", "")
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil || len(list) != 1 {
		t.Fatalf("%s: %v", rec.Body, err)
	}
	v := list[0]
	if v["id"] != "job" || v["script"] != "check-balance.js" || v["every"] != "1h0m0s" || v["streak"] != float64(3) ||
		v["attempt"] != float64(2) || v["next_run"] != "2030-01-01T00:05:00Z" || v["last_success"] != nil ||
		v["last_result"] != nil || v["running"] != false || v["running_run"] != nil {
		t.Fatalf("view = %v", v)
	}
	if _, ok := v["stale"].(bool); !ok {
		t.Fatalf("stale missing: %v", v)
	}
}

func TestTasksPageAndListNeverMix(t *testing.T) {
	h := newModHarness(t, nil)
	h.waitOpen()
	h.pushSimple()
	for _, accept := range []string{"text/html", "*/*", "application/json, text/html", ""} {
		get := func(path string) *httptest.ResponseRecorder {
			req := httptest.NewRequest("GET", path, nil)
			req.RemoteAddr = aliceAddr
			if accept != "" {
				req.Header.Set("Accept", accept)
			}
			rec := httptest.NewRecorder()
			h.mux.ServeHTTP(rec, req)
			return rec
		}
		page := get("/tasks")
		if page.Code != 200 || !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") ||
			strings.HasPrefix(strings.TrimSpace(page.Body.String()), "[") {
			t.Fatalf("Accept %q: GET /tasks = %d %q %s", accept, page.Code, page.Header().Get("Content-Type"), page.Body)
		}
		list := get("/tasks.json")
		var v []map[string]any
		if list.Code != 200 || list.Header().Get("Content-Type") != "application/json" ||
			json.Unmarshal(list.Body.Bytes(), &v) != nil || len(v) != 1 {
			t.Fatalf("Accept %q: GET /tasks.json = %d %q %s", accept, list.Code, list.Header().Get("Content-Type"), list.Body)
		}
	}
}

func TestTrailingDataRejected(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	cfg := configBody(`"job":{"script":"check-balance.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{"check-balance.js": scriptA})
	for _, c := range []struct{ path, body string }{
		{"/tasks/config", cfg + ` {"x":1}`},
		{"/tasks/config?dry_run=1", cfg + `junk`},
		{"/tasks/secrets", `{"A":"b"} {"C":"d"}`},
		{"/tasks/secrets", `{"A":"b"}]`},
	} {
		rec := h.alice("PUT", c.path, c.body)
		if rec.Code != 422 || decodeMap(t, rec)["error"] != "trailing data" {
			t.Fatalf("%s %q: %d %s", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if store.Config() != nil || len(store.SecretNames()) != 0 {
		t.Fatal("stored despite trailing data")
	}
	// Trailing whitespace is fine.
	if rec := h.alice("PUT", "/tasks/secrets", "{\"A\":\"b\"}\n \t"); rec.Code != 200 {
		t.Fatalf("whitespace: %d %s", rec.Code, rec.Body)
	}
	// A rejected script name is echoed at most 64 bytes long.
	long := strings.Repeat("y", 300) + "/"
	rec := h.alice("PUT", "/tasks/config", configBody(`"job":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"}}`,
		map[string]string{long: scriptA}))
	if rec.Code != 422 || strings.Contains(rec.Body.String(), strings.Repeat("y", 65)) {
		t.Fatalf("long name: %d %s", rec.Code, rec.Body)
	}
}

func TestTaskViewStaleCountsOnlyRealFailures(t *testing.T) {
	now := time.Now()
	task := Task{Script: "check-balance.js", Target: Target{Node: "n1"}, Schedule: Schedule{Every: Duration(time.Hour)}}
	future := TaskState{NextRun: now.Add(time.Hour)}
	past := TaskState{NextRun: now.Add(-2 * time.Minute)}
	ended := now.Add(-3 * time.Minute)
	cases := []struct {
		name string
		st   TaskState
		runs []Run
		want bool
	}{
		{"only an operator-cancelled run", future, []Run{{ID: "c", State: RunCancelled, Reason: "cancelled by operator", Ended: ended}}, false},
		{"only a shutdown run", future, []Run{{ID: "s", State: RunFailed, Reason: "shutdown", Ended: ended}}, false},
		{"only a restart casualty", future, []Run{{ID: "l", State: RunLost, Reason: "node-control restarted", Ended: ended}}, false},
		{"a real failure, next run ahead", future, []Run{{ID: "f", State: RunFailed, Reason: "timeout", Ended: ended}}, true},
		{"a lost run", future, []Run{{ID: "l", State: RunLost, Reason: "runner unreachable", Ended: ended}}, true},
		{"a real failure and a past first schedule", past, []Run{{ID: "f", State: RunFailed, Reason: "no eligible node", Ended: ended}}, true},
		{"cancelled only but first schedule long past", past, []Run{{ID: "c", State: RunCancelled, Reason: "cancelled by operator", Ended: ended}}, true},
	}
	for _, c := range cases {
		if got := buildTaskView(now, "job", task, c.st, c.runs, nil).Stale; got != c.want {
			t.Errorf("%s: stale = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConfigRejectsTaskOverRunnerEnvCap(t *testing.T) {
	h := newModHarness(t, nil)
	store := h.waitOpen()
	big := map[string]string{}
	for i := 0; i < 26; i++ { // 26 x 4000 bytes: over the runner's 100000-byte cap together
		big["BIG_"+string(rune('A'+i))] = strings.Repeat("s", 4000)
	}
	half := map[string]string{} // a secrets body is at most 64 KiB: two requests
	for n, v := range big {
		half[n] = v
		if len(half) == 13 {
			b, _ := json.Marshal(half)
			if rec := h.alice("PUT", "/tasks/secrets", string(b)); rec.Code != 200 {
				t.Fatalf("put secrets: %d %s", rec.Code, rec.Body)
			}
			half = map[string]string{}
		}
	}
	names := make([]string, 0, len(big))
	for n := range big {
		names = append(names, `"`+n+`"`)
	}
	task := func(id string, secrets []string, params string) string {
		return `"` + id + `":{"script":"x.js","target":{"node":"n1"},"schedule":{"every":"1h"},"secrets":[` +
			strings.Join(secrets, ",") + `],"params":` + params + `}`
	}
	rec := h.alice("PUT", "/tasks/config", configBody(task("too-big", names, `{}`), map[string]string{"x.js": scriptA}))
	if rec.Code != 422 {
		t.Fatalf("code %d %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "task too-big") || !strings.Contains(body, "100000") || strings.Contains(body, "sss") {
		t.Fatalf("answer must name the task and the cap, never a value: %s", body)
	}
	if store.Config() != nil {
		t.Fatal("stored despite the cap")
	}
	// Params count too: few secrets plus large params also exceed it.
	params := `{"p":"` + strings.Repeat("x", 97000) + `"}`
	rec = h.alice("PUT", "/tasks/config", configBody(task("params-big", names[:1], params), map[string]string{"x.js": scriptA}))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "task params-big") {
		t.Fatalf("params: %d %s", rec.Code, rec.Body)
	}
	// Under the cap is accepted.
	rec = h.alice("PUT", "/tasks/config", configBody(task("fits", names[:20], `{"a":1}`), map[string]string{"x.js": scriptA}))
	if rec.Code != 200 {
		t.Fatalf("fits: %d %s", rec.Code, rec.Body)
	}
}
