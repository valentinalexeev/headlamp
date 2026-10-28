package enroll

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/core"
)

const cfgJSON = `{"host": "ingress.example.test", "public_key": "P", "short_id": "ab", "sni": "s.example.test"}`

type modEnv struct {
	dir string
	m   map[string]string
}

func newModEnv(t *testing.T, agentURL string) *modEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "c.json")
	if err := os.WriteFile(cfg, []byte(cfgJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	return &modEnv{dir: dir, m: map[string]string{
		"ENROLL_DB":         filepath.Join(dir, "e.db"),
		"ENROLL_CONFIG":     cfg,
		"ENROLL_SECRET_KEY": strings.Repeat("k", 32),
		"AGENT_URL":         agentURL,
		"AGENT_TOKEN":       "agent-token-value",
	}}
}

func (e *modEnv) env() core.Env { return core.MapEnv(e.m) }

func (e *modEnv) without(keys ...string) core.Env {
	m := map[string]string{}
	for k, v := range e.m {
		m[k] = v
	}
	for _, k := range keys {
		delete(m, k)
	}
	return core.MapEnv(m)
}

func routesFor(env core.Env) *core.Routes {
	return &core.Routes{Admin: http.NewServeMux(), Public: http.NewServeMux(), Env: env, Tailnet: core.NewLocal("127.0.0.1"), Log: nil}
}

func TestModuleName(t *testing.T) {
	if got := New(core.MapEnv(nil)).Name(); got != "enroll" {
		t.Fatal(got)
	}
}

func TestDisabledWithoutDB(t *testing.T) {
	e := newModEnv(t, "http://agent.test")
	env := e.without("ENROLL_DB")
	r := &core.Routes{Admin: http.NewServeMux(), Env: env}
	m := New(env)
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/enroll", "/enroll/devices"} {
		rec := httptest.NewRecorder()
		r.Admin.ServeHTTP(rec, httptest.NewRequest("GET", p, nil))
		if rec.Code != 404 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	// even the other variables being absent is fine when enrolment is off
	m2 := New(core.MapEnv(nil))
	if err := m2.Register(&core.Routes{Admin: http.NewServeMux()}); err != nil {
		t.Fatal(err)
	}
}

func TestMissingVarsGiveClearError(t *testing.T) {
	e := newModEnv(t, "http://agent.test")
	for _, k := range []string{"ENROLL_SECRET_KEY", "ENROLL_CONFIG", "AGENT_URL", "AGENT_TOKEN"} {
		t.Run(k, func(t *testing.T) {
			err := New(e.without(k)).Register(routesFor(e.without(k)))
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Fatalf("%v", err)
			}
			if strings.Contains(err.Error(), "agent-token-value") || strings.Contains(err.Error(), strings.Repeat("k", 32)) {
				t.Fatalf("secret in error: %v", err)
			}
		})
	}
}

func TestInvalidConfigurationFailsFastWithoutSecrets(t *testing.T) {
	e := newModEnv(t, "http://agent.test")
	badCfg := filepath.Join(e.dir, "bad.json")
	os.WriteFile(badCfg, []byte(`{"host": "h"}`), 0o600)
	garbled := filepath.Join(e.dir, "garbled.json")
	os.WriteFile(garbled, []byte(`{not json SECRETCONTENT`), 0o600)
	cases := []struct {
		name string
		set  map[string]string
		want string
	}{
		{"config file missing", map[string]string{"ENROLL_CONFIG": filepath.Join(e.dir, "nope.json")}, "ENROLL_CONFIG"},
		{"config incomplete", map[string]string{"ENROLL_CONFIG": badCfg}, "ENROLL_CONFIG"},
		{"config not JSON", map[string]string{"ENROLL_CONFIG": garbled}, "ENROLL_CONFIG"},
		{"short key", map[string]string{"ENROLL_SECRET_KEY": "SHORTKEY"}, "ENROLL_SECRET_KEY"},
		{"agent url without scheme", map[string]string{"AGENT_URL": "agent.test:8080"}, "AGENT_URL"},
		{"agent url wrong scheme", map[string]string{"AGENT_URL": "ftp://agent.test"}, "AGENT_URL"},
		{"agent url without host", map[string]string{"AGENT_URL": "http://"}, "AGENT_URL"},
		{"agent url with credentials", map[string]string{"AGENT_URL": "http://user:PASSWORD@agent.test"}, "AGENT_URL"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := map[string]string{}
			for k, v := range e.m {
				m[k] = v
			}
			for k, v := range c.set {
				m[k] = v
			}
			env := core.MapEnv(m)
			err := New(env).Register(routesFor(env))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
			for _, secret := range []string{"agent-token-value", "SHORTKEY", "PASSWORD", "SECRETCONTENT", strings.Repeat("k", 32)} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("secret %q in error: %v", secret, err)
				}
			}
			if _, statErr := os.Stat(m["ENROLL_DB"]); statErr == nil {
				t.Fatal("database was created before the configuration was validated")
			}
		})
	}
	t.Run("public mux required", func(t *testing.T) {
		env := e.env()
		r := routesFor(env)
		r.Public = nil
		if err := New(env).Register(r); err == nil {
			t.Fatal("want an error")
		}
	})
}

// fakeIngressAgent plays the ingress agent for end-to-end tests.
type fakeIngressAgent struct {
	srv  *httptest.Server
	mu   sync.Mutex
	puts []string // request bodies
	auth []string
	stat string
}

func newFakeIngressAgent(t *testing.T) *fakeIngressAgent {
	a := &fakeIngressAgent{stat: "{}"}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.auth = append(a.auth, r.Header.Get("Authorization"))
		switch {
		case r.Method == "PUT" && r.URL.Path == "/v1/credentials":
			b, _ := io.ReadAll(r.Body)
			a.puts = append(a.puts, string(b))
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/v1/stats":
			w.Write([]byte(a.stat))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeIngressAgent) putCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.puts)
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func httpDo(t *testing.T, method, url, ctype, body string) (int, http.Header, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func TestEndToEndThroughCore(t *testing.T) {
	restore := setReconcileForTest(10*time.Millisecond, 20*time.Millisecond)
	defer restore()
	agent := newFakeIngressAgent(t)
	e := newModEnv(t, agent.srv.URL)
	e.m["ENROLL_ALLOWED_HOSTS"] = "admin.example.test,127.0.0.1"
	tn := core.NewLocal("127.0.0.1")

	mod := New(e.env())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- core.Run(ctx, []core.Module{mod}, e.env(), tn) }()
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not stop")
		}
	}
	defer stop()
	admin := "http://" + tn.AdminAddr()
	public := "http://" + tn.PublicAddr()

	code, _, page := httpDo(t, "GET", admin+"/enroll", "", "")
	if code != 200 || !strings.Contains(page, `id="syncbanner"`) {
		t.Fatalf("page %d", code)
	}
	body := `{"name":"Anna phone","owner":"anna","client":"karing","expires_at":` + "99999999999" + `}`
	code, _, resp := httpDo(t, "POST", admin+"/enroll/devices", "application/json", body)
	if code != 200 {
		t.Fatalf("create %d %s", code, resp)
	}
	var created struct {
		ID  int64  `json:"id"`
		URL string `json:"subscription_url"`
	}
	json.Unmarshal([]byte(resp), &created)
	if !strings.HasPrefix(created.URL, "https://ingress.example.test/sub/") {
		t.Fatal(created.URL)
	}
	if agent.putCount() < 1 {
		t.Fatal("create did not push")
	}
	agent.mu.Lock()
	for _, a := range agent.auth {
		if a != "Bearer agent-token-value" {
			t.Errorf("agent got Authorization %q", a)
		}
	}
	agent.mu.Unlock()

	// the reconcile loop started by Start keeps pushing
	n := agent.putCount()
	waitFor(t, "reconcile pushes", func() bool { return agent.putCount() >= n+2 })

	// link -> public redeem
	code, _, resp = httpDo(t, "POST", admin+"/enroll/devices/"+strconv.FormatInt(created.ID, 10)+"/links", "application/json", `{"ttl":3600,"max_uses":1}`)
	if code != 200 {
		t.Fatalf("link %d %s", code, resp)
	}
	var link struct{ URL string }
	json.Unmarshal([]byte(resp), &link)
	tok := link.URL[strings.LastIndex(link.URL, "/")+1:]
	if code, _, _ := httpDo(t, "GET", public+"/e/"+tok, "", ""); code != 200 {
		t.Fatalf("redeem %d", code)
	}
	if code, _, b := httpDo(t, "GET", public+"/e/"+tok, "", ""); code != 404 || b != "" {
		t.Fatalf("second redeem %d %q", code, b)
	}
	sub := created.URL[strings.Index(created.URL, "/sub/"):]
	if code, h, _ := httpDo(t, "GET", public+sub, "", ""); code != 200 || h.Get("Profile-Update-Interval") != "12" {
		t.Fatalf("sub %d", code)
	}

	// the admin routes are not on the public listener and vice versa
	if code, _, b := httpDo(t, "GET", public+"/enroll", "", ""); code != 404 || b != "" {
		t.Fatalf("public /enroll %d %q", code, b)
	}
	if code, _, _ := httpDo(t, "GET", admin+"/e/"+tok, "", ""); code != 404 {
		t.Fatalf("admin /e %d", code)
	}

	// Host guard covers the admin listener only
	req, _ := http.NewRequest("GET", admin+"/enroll/devices", nil)
	req.Host = "evil.example.test"
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 400 {
		t.Fatalf("evil host %d", resp2.StatusCode)
	}
	req, _ = http.NewRequest("GET", public+sub, nil)
	req.Host = "ingress.example.test"
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp3.Body.Close()
	if resp3.StatusCode != 200 {
		t.Fatalf("public with ingress host %d", resp3.StatusCode)
	}

	// shutdown stops the loop; core.Run then calls Stop, which closes the store
	stop()
	m := mod.(*Module)
	waitFor(t, "store closed", func() bool { return m.store.(*Store).db.Ping() != nil })
	settled := agent.putCount()
	time.Sleep(100 * time.Millisecond)
	if agent.putCount() != settled {
		t.Fatal("reconcile loop kept running after shutdown")
	}
}

func TestStartReturnsPromptlyAndLoopStopsWithContext(t *testing.T) {
	restore := setReconcileForTest(time.Hour, time.Hour)
	defer restore()
	e := newModEnv(t, "http://agent.test")
	env := e.env()
	m := New(env)
	if err := m.Register(routesFor(env)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	start := time.Now()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("Start blocked")
	}
	cancel()
	mod := m.(*Module)
	<-mod.loopDone // the loop ends with ctx
	// ctx cancellation alone must not close the store: in-flight requests may still use it
	if err := mod.store.(*Store).db.Ping(); err != nil {
		t.Fatalf("store closed on ctx cancel: %v", err)
	}
	stopCtx, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopCancel()
	if err := mod.Stop(stopCtx); err != nil {
		t.Fatal(err)
	}
	if mod.store.(*Store).db.Ping() == nil {
		t.Fatal("store still open after Stop")
	}
	if err := mod.Stop(stopCtx); err != nil {
		t.Fatalf("second Stop: %v", err)
	}
}

func TestStopWithoutStartAndWhenDisabled(t *testing.T) {
	ctx := context.Background()
	if err := New(core.MapEnv(nil)).(*Module).Stop(ctx); err != nil {
		t.Fatal(err)
	}
	e := newModEnv(t, "http://agent.test")
	m := New(e.env()).(*Module)
	if err := m.Register(routesFor(e.env())); err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx); err != nil { // registered but never started
		t.Fatal(err)
	}
	if m.store.(*Store).db.Ping() == nil {
		t.Fatal("store still open")
	}
}

func TestProductionClientDialsThroughTheTailnet(t *testing.T) {
	agent := newFakeIngressAgent(t)
	e := newModEnv(t, "http://agent.tailnet.test:8080")
	env := e.env()
	var dialed []string
	tn := &dialRecorder{Local: core.NewLocal("127.0.0.1"), target: agent.srv.Listener.Addr().String(), seen: &dialed}
	r := routesFor(env)
	r.Tailnet = tn
	m := New(env)
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	defer m.(*Module).store.Close()
	rec := httptest.NewRecorder()
	body := `{"name":"n","owner":"o","client":"sing-box","expires_at":99999999999}`
	req := httptest.NewRequest("POST", "/enroll/devices", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.Admin.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(dialed) == 0 || dialed[0] != "agent.tailnet.test:8080" {
		t.Fatalf("dialed %v", dialed)
	}
	if agent.putCount() != 1 {
		t.Fatalf("puts %d", agent.putCount())
	}
}

type dialRecorder struct {
	*core.Local
	target string
	seen   *[]string
}

func (d *dialRecorder) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	*d.seen = append(*d.seen, addr)
	return d.Local.Dial(ctx, network, d.target)
}
