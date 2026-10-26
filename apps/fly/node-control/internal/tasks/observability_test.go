package tasks

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// --- scenario: secrets and result values through the real engine and notifier --

const (
	scenarioSecret = "sekrit-Value-q7w"
	scenarioTopic  = "topic-Hidden-z3x"
)

type obsScenario struct {
	m       *Metrics
	logs    *syncBuf
	pingURL string
	ntfyURL string
	pingHit atomic.Int32
}

// runObsScenario drives a task through a failed attempt, a successful retry
// that trips the alert, then an exhausted sequence that sends the failure
// notification. The secret is in the script, params, result and error.
func runObsScenario(t *testing.T) *obsScenario {
	t.Helper()
	sc := &obsScenario{m: NewMetrics(), logs: &syncBuf{}}
	ntfy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	ping := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sc.pingHit.Add(1)
		w.WriteHeader(200)
	}))
	t.Cleanup(ntfy.Close)
	t.Cleanup(ping.Close)
	sc.pingURL, sc.ntfyURL = ping.URL+"/ping/check-uuid-5150", ntfy.URL

	task := baseTask()
	task.Secrets = []string{"token"}
	task.Params = map[string]any{"user": scenarioSecret}
	task.Healthcheck = "hc"
	task.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	task.Alert = &Alert{
		When:   Condition{Field: "balance", Op: "<", Value: 1000},
		Notify: Notify{Topic: "topic", Title: "low", Message: "balance {balance}"},
	}
	task.OnFailure = &OnFailure{Notify: Notify{Topic: "topic", Title: "failed", Message: "{reason}"}}

	log := slog.New(slog.NewJSONHandler(sc.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	s := openStore(t, newMemKV())
	script := "const token = '" + scenarioSecret + "';\n"
	if err := s.PutConfig(&Config{Tasks: map[string]Task{"job": task}}, map[string]string{"job.sh": script}); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"token": scenarioSecret, "hc": sc.pingURL, "topic": scenarioTopic} {
		if err := s.PutSecret(k, v); err != nil {
			t.Fatal(err)
		}
	}
	clock := &fakeClock{t: t0}
	n := NewNotifier(s, ntfy.Client(), ntfy.URL, log, clock.Now)
	n.sleep = func(context.Context, time.Duration) {}
	n.SetObserver(sc.m.NotifySent)
	runner := &fakeRunner{}
	runner.status = func(_ context.Context, _ string, call int) (RunStatus, error) {
		switch call {
		case 2:
			return RunStatus{State: "succeeded", Result: map[string]any{"balance": 842.0, "echo": scenarioSecret}}, nil
		default:
			return RunStatus{State: "failed", Error: "login rejected for " + scenarioSecret}, nil
		}
	}
	e := NewEngine(Deps{
		Store: s, Runner: runner, Notifier: n, Metrics: sc.m, Now: clock.Now, Log: log,
		Nodes:     &fakeNodes{nodes: []headscale.Node{{Name: "n1", Online: true, IPAddresses: []string{"100.64.0.1"}}}},
		Rand:      func() float64 { return 0.5 },
		PollEvery: 2 * time.Millisecond,
	})
	e.startSpread = 0
	step := func() { e.tick(context.Background()); e.wait() }

	step()                              // attempt 1 fails
	clock.Set(s.State("job").AttemptAt) // retry
	step()                              // attempt 2 succeeds, alert sent
	clock.Set(s.State("job").NextRun)   // next sequence
	step()                              // fails
	clock.Set(s.State("job").AttemptAt) //
	step()                              // fails, retries exhausted: failure notification
	e.Reload()                          // metrics sync on reload
	if got := len(s.Runs("job")); got != 4 {
		t.Fatalf("runs = %d", got)
	}
	if sc.pingHit.Load() != 2 {
		t.Fatalf("pings = %d", sc.pingHit.Load())
	}
	return sc
}

func (sc *obsScenario) forbidden() []string {
	return []string{scenarioSecret, scenarioTopic, sc.pingURL, sc.ntfyURL, "check-uuid-5150", "127.0.0.1", "http://", "842"}
}

func TestMetricsContainNoSecretOrResultValues(t *testing.T) {
	sc := runObsScenario(t)
	out := metricsText(sc.m)
	if !strings.Contains(out, `task_runs_total{task="job",node="n1",state="failed"} 3`) ||
		!strings.Contains(out, `task_alert_sent_total{task="job",kind="alert",ok="true"} 1`) {
		t.Fatalf("scenario not reflected in metrics:\n%s", out)
	}
	for _, bad := range sc.forbidden() {
		if strings.Contains(out, bad) {
			t.Errorf("metrics contain %q:\n%s", bad, out)
		}
	}
}

func TestLogsContainNoSecrets(t *testing.T) {
	sc := runObsScenario(t)
	raw := sc.logs.String()
	events := map[string]int{}
	var cleaned strings.Builder
	sn := bufio.NewScanner(strings.NewReader(raw))
	for sn.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(sn.Bytes(), &rec); err != nil {
			t.Fatalf("not a JSON log line: %q", sn.Text())
		}
		events[rec["msg"].(string)]++
		// Random run ids and wall-clock timestamps could contain any digits.
		delete(rec, "time")
		delete(rec, "run")
		b, _ := json.Marshal(rec)
		cleaned.Write(b)
		cleaned.WriteByte('\n')
	}
	for _, ev := range []string{"run_started", "node_chosen", "attempt_result", "retry_scheduled",
		"alert_evaluated", "notify_result", "ping_result"} {
		if events[ev] == 0 {
			t.Errorf("no %s event in\n%s", ev, raw)
		}
	}
	out := cleaned.String()
	for _, bad := range sc.forbidden() {
		if strings.Contains(out, bad) {
			t.Errorf("logs contain %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, `"hit":true`) {
		t.Errorf("alert_evaluated lacks hit:\n%s", out)
	}
}

// --- heartbeat -----------------------------------------------------------------

type hbHarness struct {
	f     *fixture
	h     *heartbeat
	logs  *syncBuf
	tick  chan time.Time
	every time.Duration
	calls atomic.Int32
	stop  func()
}

func newHBHarness(t *testing.T, secret string) *hbHarness {
	t.Helper()
	f := newFixture(t)
	f.n.SetObserver(nil) // the fixture's observer is not goroutine-safe
	hh := &hbHarness{f: f, logs: &syncBuf{}, tick: make(chan time.Time)}
	log := slog.New(slog.NewTextHandler(hh.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	hh.h = newHeartbeat(f.n, secret, log)
	hh.h.ticker = func(d time.Duration) (<-chan time.Time, func()) {
		hh.every = d
		return hh.tick, func() {}
	}
	inner := hh.h.ping
	hh.h.ping = func(ctx context.Context) string {
		defer hh.calls.Add(1)
		return inner(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); hh.h.run(ctx) }()
	hh.stop = func() { cancel(); <-done }
	t.Cleanup(hh.stop)
	return hh
}

func (hh *hbHarness) waitCalls(t *testing.T, n int32) {
	t.Helper()
	waitFor(t, "heartbeat ping", func() bool { return hh.calls.Load() >= n })
}

func (hh *hbHarness) pingCount() int {
	hh.f.mu.Lock()
	defer hh.f.mu.Unlock()
	return len(hh.f.pings)
}

func countLines(s, sub string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, sub) {
			n++
		}
	}
	return n
}

func TestHeartbeatPingsOnStartAndHourly(t *testing.T) {
	hh := newHBHarness(t, "hc")
	hh.waitCalls(t, 1)
	if hh.pingCount() != 1 {
		t.Fatalf("pings at start = %d", hh.pingCount())
	}
	if hh.every != time.Hour {
		t.Fatalf("interval = %s", hh.every)
	}
	hh.tick <- time.Now()
	hh.waitCalls(t, 2)
	hh.tick <- time.Now()
	hh.waitCalls(t, 3)
	hh.f.mu.Lock()
	pings := append([]string(nil), hh.f.pings...)
	hh.f.mu.Unlock()
	if len(pings) != 3 {
		t.Fatalf("pings = %v", pings)
	}
	for _, p := range pings {
		if p != "GET /ping/uuid-1234" {
			t.Errorf("ping went to %q", p)
		}
	}
	logs := hh.logs.String()
	if c := countLines(logs, "ping_result"); c != 1 {
		t.Fatalf("ping_result logged %d times for an unchanged outcome:\n%s", c, logs)
	}
	if !strings.Contains(logs, "task=scheduler") || !strings.Contains(logs, "outcome=sent") {
		t.Fatalf("log fields: %s", logs)
	}

	// A change of outcome is logged, a repeat is not.
	hh.f.mu.Lock()
	hh.f.pingCode = 503
	hh.f.mu.Unlock()
	hh.tick <- time.Now()
	hh.waitCalls(t, 4)
	hh.tick <- time.Now()
	hh.waitCalls(t, 5)
	logs = hh.logs.String()
	if c := countLines(logs, "ping_result"); c != 2 {
		t.Fatalf("ping_result lines = %d:\n%s", c, logs)
	}
	if strings.Contains(logs, hh.f.pingSrv.URL) || strings.Contains(logs, "uuid-1234") {
		t.Fatalf("heartbeat log leaks the URL:\n%s", logs)
	}

	hh.stop()
	select {
	case hh.tick <- time.Now():
		t.Fatal("heartbeat still running after cancel")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHeartbeatMissingSecretLogsOnce(t *testing.T) {
	hh := newHBHarness(t, "absent")
	hh.waitCalls(t, 1)
	for i := int32(2); i <= 4; i++ {
		hh.tick <- time.Now()
		hh.waitCalls(t, i)
	}
	logs := hh.logs.String()
	if c := countLines(logs, "ping_result"); c != 1 {
		t.Fatalf("missing secret logged %d times:\n%s", c, logs)
	}
	if !strings.Contains(logs, "level=INFO") || !strings.Contains(logs, `outcome="`+failedNoSecret+`"`) {
		t.Fatalf("log line: %s", logs)
	}
	if hh.pingCount() != 0 {
		t.Fatal("pinged without a secret")
	}
}

// --- module env ----------------------------------------------------------------

func TestMetricsEnvValidation(t *testing.T) {
	mr := miniredis.RunT(t)
	for _, bad := range []string{"nonsense", "9091", ":abc", ":70000", "host:", "[::1"} {
		env := modEnv("redis://" + mr.Addr())
		env["TASKS_METRICS_ADDR"] = bad
		m := New(core.MapEnv(env), testNodes()).(*Module)
		err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet})
		if err == nil || !strings.Contains(err.Error(), "TASKS_METRICS_ADDR") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
	env := modEnv("redis://" + mr.Addr())
	env["TASKS_HEARTBEAT_SECRET"] = "not a name!"
	m := New(core.MapEnv(env), testNodes()).(*Module)
	if err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet}); err == nil ||
		!strings.Contains(err.Error(), "TASKS_HEARTBEAT_SECRET") {
		t.Errorf("bad heartbeat secret name: err = %v", err)
	}

	start := func(addr string) []string {
		env := modEnv("redis://" + mr.Addr())
		if addr != "" {
			env["TASKS_METRICS_ADDR"] = addr
		}
		m := New(core.MapEnv(env), testNodes()).(*Module)
		var mu sync.Mutex
		var got []string
		served := make(chan struct{}, 1)
		m.serveMetrics = func(ctx context.Context, a string, mm *Metrics) error {
			mu.Lock()
			got = append(got, a)
			mu.Unlock()
			if mm == nil {
				t.Error("no metrics passed to the listener")
			}
			served <- struct{}{}
			<-ctx.Done()
			return nil
		}
		m.newRunner = func() Runner { return &fakeRunner{} }
		m.newNotifier = func(*Store) Notifier { return &fakeNotifier{} }
		if err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet}); err != nil {
			t.Fatalf("Register: %v", err)
		}
		if err := m.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		waitFor(t, "store open", func() bool { s, _ := m.current(); return s != nil })
		if addr != "" {
			select {
			case <-served:
			case <-time.After(2 * time.Second):
				t.Fatal("metrics listener not started")
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Stop(ctx)
		mu.Lock()
		defer mu.Unlock()
		return got
	}
	if got := start(""); len(got) != 0 {
		t.Fatalf("unset TASKS_METRICS_ADDR started a listener on %v", got)
	}
	if got := start(":9091"); len(got) != 1 || got[0] != ":9091" {
		t.Fatalf("listener addr = %v", got)
	}
}

// pingingNotifier is a fake notifier that also takes heartbeat pings and an
// observer, like HTTPNotifier.
type pingingNotifier struct {
	fakeNotifier
	mu    sync.Mutex
	pings []string
	obs   func(task, kind string, ok bool)
}

func (p *pingingNotifier) PingSecret(_ context.Context, name string) string {
	p.mu.Lock()
	p.pings = append(p.pings, name)
	obs := p.obs
	p.mu.Unlock()
	if obs != nil {
		obs("scheduler", "ping", true)
	}
	return "sent"
}

func (p *pingingNotifier) SetObserver(f func(task, kind string, ok bool)) {
	p.mu.Lock()
	p.obs = f
	p.mu.Unlock()
}

func TestModuleWiresHeartbeatAndObserver(t *testing.T) {
	mr := miniredis.RunT(t)
	env := modEnv("redis://" + mr.Addr())
	env["TASKS_HEARTBEAT_SECRET"] = "HB_URL"
	m := New(core.MapEnv(env), testNodes()).(*Module)
	pn := &pingingNotifier{}
	m.newRunner = func() Runner { return &fakeRunner{} }
	m.newNotifier = func(*Store) Notifier { return pn }
	if err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet}); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		m.Stop(ctx)
	}()
	waitFor(t, "heartbeat at start", func() bool {
		pn.mu.Lock()
		defer pn.mu.Unlock()
		return len(pn.pings) == 1 && pn.pings[0] == "HB_URL"
	})
	waitFor(t, "observer feeds metrics", func() bool {
		return strings.Contains(metricsText(m.metrics), `task_alert_sent_total{task="scheduler",kind="ping",ok="true"} 1`)
	})
}
