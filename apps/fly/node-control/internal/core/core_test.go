package core

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type stubModule struct {
	name     string
	startErr error
	register func(r *Routes)
}

func (m *stubModule) Name() string { return m.name }
func (m *stubModule) Register(r *Routes) error {
	if m.register != nil {
		m.register(r)
	}
	return nil
}
func (m *stubModule) Start(ctx context.Context) error { return m.startErr }

// runBG starts Run in the background and returns a stop func that cancels
// the context and returns Run's error.
func runBG(t *testing.T, mods []Module, env Env, tn Tailnet) func() error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, mods, env, tn) }()
	return func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("Run did not return after cancel")
			return nil
		}
	}
}

func get(t *testing.T, addr, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestHealthz(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, nil, MapEnv(nil), tn)
	code, body := get(t, tn.AdminAddr(), "", "/healthz")
	if code != 200 || strings.TrimSpace(body) != `{"status":"ok"}` {
		t.Fatalf("got %d %q", code, body)
	}
	if err := stop(); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

func TestPublicMuxOnlyWhenEnrollDBSet(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, nil, MapEnv(nil), tn)
	tn.AdminAddr()
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if tn.PublicListened() {
		t.Fatal("public listener opened without ENROLL_DB")
	}

	tn2 := NewLocal("127.0.0.1")
	env := MapEnv(map[string]string{"ENROLL_DB": filepath.Join(t.TempDir(), "enroll.db")})
	var sawDB bool
	mod := &stubModule{name: "m", register: func(r *Routes) {
		sawDB = r.Public != nil
		if r.Public != nil {
			r.Public.HandleFunc("/pub", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pub")) })
		}
	}}
	stop2 := runBG(t, []Module{mod}, env, tn2)
	code, body := get(t, tn2.PublicAddr(), "", "/pub")
	if code != 200 || body != "pub" {
		t.Fatalf("public: %d %q", code, body)
	}
	if err := stop2(); err != nil {
		t.Fatal(err)
	}
	if !sawDB {
		t.Fatal("module did not see the Public mux")
	}
}

func TestHostGuard(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, nil, MapEnv(map[string]string{"ENROLL_ALLOWED_HOSTS": "a,b"}), tn)
	defer stop()
	addr := tn.AdminAddr()
	if code, _ := get(t, addr, "evil", "/healthz"); code != 400 {
		t.Fatalf("evil: got %d want 400", code)
	}
	if code, _ := get(t, addr, "a", "/healthz"); code != 200 {
		t.Fatalf("a: got %d want 200", code)
	}
	if code, _ := get(t, addr, "b:80", "/healthz"); code != 200 {
		t.Fatalf("b:80: got %d want 200", code)
	}
}

func TestHostGuardInactiveWhenUnset(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, nil, MapEnv(nil), tn)
	defer stop()
	if code, _ := get(t, tn.AdminAddr(), "anything", "/healthz"); code != 200 {
		t.Fatalf("got %d", code)
	}
}

func TestModuleStartErrorStopsRun(t *testing.T) {
	want := errors.New("boom")
	tn := NewLocal("127.0.0.1")
	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), []Module{&stubModule{name: "bad", startErr: want}}, MapEnv(nil), tn)
	}()
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not stop on module Start error")
	}
}

func TestHostGuardAdminOnly(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	env := MapEnv(map[string]string{
		"ENROLL_ALLOWED_HOSTS": "a",
		"ENROLL_DB":            filepath.Join(t.TempDir(), "enroll.db"),
	})
	mod := &stubModule{name: "m", register: func(r *Routes) {
		r.Public.HandleFunc("/pub", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("pub")) })
	}}
	stop := runBG(t, []Module{mod}, env, tn)
	defer stop()
	if code, body := get(t, tn.PublicAddr(), "evil", "/pub"); code != 200 || body != "pub" {
		t.Fatalf("public with foreign Host: %d %q", code, body)
	}
	if code, _ := get(t, tn.AdminAddr(), "evil", "/healthz"); code != 400 {
		t.Fatalf("admin evil: %d", code)
	}
}

func TestHostGuardBracketedIPv6(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	stop := runBG(t, nil, MapEnv(map[string]string{"ENROLL_ALLOWED_HOSTS": "::1"}), tn)
	defer stop()
	addr := tn.AdminAddr()
	if code, _ := get(t, addr, "[::1]", "/healthz"); code != 200 {
		t.Fatalf("[::1]: %d", code)
	}
	if code, _ := get(t, addr, "[::1]:80", "/healthz"); code != 200 {
		t.Fatalf("[::1]:80: %d", code)
	}
	if code, _ := get(t, addr, "[::2]", "/healthz"); code != 400 {
		t.Fatalf("[::2]: %d", code)
	}
}

func TestShutdownDoesNotHangOnStuckConnection(t *testing.T) {
	old := shutdownTimeout
	shutdownTimeout = 200 * time.Millisecond
	defer func() { shutdownTimeout = old }()

	tn := NewLocal("127.0.0.1")
	release := make(chan struct{})
	defer close(release)
	inflight := make(chan struct{})
	mod := &stubModule{name: "m", register: func(r *Routes) {
		r.Admin.HandleFunc("/block", func(w http.ResponseWriter, _ *http.Request) {
			close(inflight)
			<-release
		})
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, []Module{mod}, MapEnv(nil), tn) }()
	addr := tn.AdminAddr()
	go http.Get("http://" + addr + "/block")
	<-inflight
	start := time.Now()
	cancel()
	select {
	case <-done:
		if d := time.Since(start); d > 3*time.Second {
			t.Fatalf("Run took %v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run hung on a stuck connection")
	}
}

type orderModule struct {
	stubModule
	started atomic.Bool
	sawFlag atomic.Bool
}

func (m *orderModule) Start(ctx context.Context) error {
	time.Sleep(300 * time.Millisecond)
	m.started.Store(true)
	return nil
}

func TestModuleStartedBeforeServing(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	m := &orderModule{stubModule: stubModule{name: "o"}}
	m.register = func(r *Routes) {
		r.Admin.HandleFunc("/o", func(w http.ResponseWriter, _ *http.Request) {
			m.sawFlag.Store(m.started.Load())
		})
	}
	stop := runBG(t, []Module{m}, MapEnv(nil), tn)
	defer stop()
	get(t, tn.AdminAddr(), "", "/o")
	if !m.sawFlag.Load() {
		t.Fatal("handler served before module Start finished")
	}
}

func TestNoListenerWhenStartFails(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	env := MapEnv(map[string]string{"ENROLL_DB": filepath.Join(t.TempDir(), "enroll.db")})
	err := Run(context.Background(), []Module{&stubModule{name: "bad", startErr: errors.New("boom")}}, env, tn)
	if err == nil {
		t.Fatal("expected error")
	}
	if tn.AdminListened() || tn.PublicListened() {
		t.Fatal("listener opened despite Start failure")
	}
}

func TestPublicUncleanPathsAreEmpty404(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	env := MapEnv(map[string]string{"ENROLL_DB": filepath.Join(t.TempDir(), "enroll.db")})
	mod := &stubModule{name: "m", register: func(r *Routes) {
		r.Public.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	}}
	stop := runBG(t, []Module{mod}, env, tn)
	defer stop()
	addr := tn.PublicAddr()
	for _, p := range []string{"//sub/x", "//e/x", "/a/../e/x", "/e/./x", "/e//x"} {
		req, _ := http.NewRequest("GET", "http://"+addr+p, nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || len(b) != 0 || resp.Header.Get("Location") != "" {
			t.Fatalf("%s: %d %q %v", p, resp.StatusCode, b, resp.Header)
		}
	}
	if code, body := get(t, addr, "", "/e/x"); code != 200 || body != "ok" {
		t.Fatalf("clean path: %d %q", code, body)
	}
}

type stopModule struct {
	stubModule
	tn        *Local
	calls     atomic.Int32
	dialErr   atomic.Value // error from dialing admin inside Stop
	deadlineN atomic.Bool
}

func (m *stopModule) Stop(ctx context.Context) error {
	m.calls.Add(1)
	_, hasDeadline := ctx.Deadline()
	m.deadlineN.Store(hasDeadline)
	if !m.tn.AdminListened() {
		return nil
	}
	c, err := net.DialTimeout("tcp", m.tn.AdminAddr(), time.Second)
	if err == nil {
		c.Close()
		m.dialErr.Store(errors.New("admin listener still accepting"))
	} else {
		m.dialErr.Store(err)
	}
	return nil
}

func TestStopRunsOnceAfterServersStopped(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	started := make(chan struct{})
	release := make(chan struct{})
	finished := atomic.Bool{}
	stoppedWhileServing := atomic.Bool{}
	m := &stopModule{stubModule: stubModule{name: "s"}, tn: tn}
	m.register = func(r *Routes) {
		r.Admin.HandleFunc("/slow", func(w http.ResponseWriter, _ *http.Request) {
			close(started)
			<-release
			finished.Store(true)
		})
	}
	plain := &stubModule{name: "plain"} // does not implement Stopper
	stop := runBG(t, []Module{plain, m}, MapEnv(nil), tn)
	go func() {
		if resp, err := http.Get("http://" + tn.AdminAddr() + "/slow"); err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	done := make(chan error, 1)
	go func() { done <- stop() }()
	time.Sleep(100 * time.Millisecond)
	if m.calls.Load() != 0 {
		stoppedWhileServing.Store(true)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stoppedWhileServing.Load() {
		t.Fatal("Stop ran while a request was in flight")
	}
	if !finished.Load() {
		t.Fatal("in-flight request did not complete before Stop")
	}
	if n := m.calls.Load(); n != 1 {
		t.Fatalf("Stop called %d times", n)
	}
	if err, _ := m.dialErr.Load().(error); err == nil || err.Error() == "admin listener still accepting" {
		t.Fatalf("admin listener open during Stop: %v", err)
	}
	if !m.deadlineN.Load() {
		t.Fatal("Stop context is unbounded")
	}
}

func TestStopCalledWhenAnotherModuleFailsToStart(t *testing.T) {
	tn := NewLocal("127.0.0.1")
	good := &stopModule{stubModule: stubModule{name: "good"}, tn: tn}
	bad := &stubModule{name: "bad", startErr: errors.New("boom")}
	if err := Run(context.Background(), []Module{good, bad}, MapEnv(nil), tn); err == nil {
		t.Fatal("expected error")
	}
	if n := good.calls.Load(); n != 1 {
		t.Fatalf("Stop called %d times", n)
	}
}

func TestNewHTTPServerTimeouts(t *testing.T) {
	admin := newHTTPServer(http.NotFoundHandler(), false)
	pub := newHTTPServer(http.NotFoundHandler(), true)
	for name, s := range map[string]*http.Server{"admin": admin, "public": pub} {
		if s.ReadHeaderTimeout <= 0 || s.IdleTimeout <= 0 || s.WriteTimeout < 60*time.Second {
			t.Errorf("%s: %+v", name, s)
		}
	}
	if pub.ReadTimeout <= 0 {
		t.Error("public server needs a ReadTimeout")
	}
}

func guardCode(t *testing.T, patterns []string, host string) int {
	t.Helper()
	h := hostGuard(patterns, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = host
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func TestHostGuardWildcards(t *testing.T) {
	cases := []struct {
		patterns []string
		host     string
		want     int
	}{
		{[]string{"*"}, "anything.invalid", 200},
		{[]string{"a", "*"}, "whatever:8080", 200},
		{[]string{"*.example.com"}, "a.example.com", 200},
		{[]string{"*.example.com"}, "x.y.example.com:8080", 200},
		{[]string{"*.EXAMPLE.com"}, "A.Example.Com", 200},
		{[]string{"*.example.com"}, "example.com", 400}, // Starlette: the dot is part of the suffix
		{[]string{"*.example.com", "example.com"}, "example.com", 200},
		{[]string{"*.example.com"}, "evilexample.com", 400},
		{[]string{"*.example.com"}, "a.example.com.evil.invalid", 400},
		{[]string{"a.example.com"}, "b.example.com", 400},
		{[]string{"*x.example.com"}, "ax.example.com", 400}, // invalid pattern never matches
		{[]string{"a"}, "a:80", 200},
	}
	for _, c := range cases {
		if got := guardCode(t, c.patterns, c.host); got != c.want {
			t.Errorf("%v host %q: got %d want %d", c.patterns, c.host, got, c.want)
		}
	}
}
