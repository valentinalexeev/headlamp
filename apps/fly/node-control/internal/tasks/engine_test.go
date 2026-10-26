package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/node-control/internal/headscale"
)

// --- fakes -------------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// runnerNode is an online node with the default runner tag whose given name
// equals its reported name.
func runnerNode(name, ip string) headscale.Node {
	return headscale.Node{Name: name, GivenName: name, Online: true, Tags: []string{DefaultRunnerTag}, IPAddresses: []string{ip}}
}

type fakeNodes struct {
	nodes []headscale.Node
	err   error
	calls atomic.Int32
}

func (f *fakeNodes) Nodes(context.Context) ([]headscale.Node, error) {
	f.calls.Add(1)
	return f.nodes, f.err
}

// fakeRunner succeeds by default; tests override start/status.
type fakeRunner struct {
	mu      sync.Mutex
	healthy map[string]bool  // addr -> healthy; missing means healthy
	hErr    map[string]error // addr -> Health error
	probed  []string         // addrs asked for health, in order
	start   func(addr string, req StartReq) (string, error)
	status  func(ctx context.Context, id string, call int) (RunStatus, error)

	health  int
	starts  []StartReq
	addrs   []string
	calls   int
	cancels []string
	cAddrs  []string
	nextID  int
}

func (r *fakeRunner) Health(_ context.Context, addr string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.health++
	r.probed = append(r.probed, addr)
	if err, ok := r.hErr[addr]; ok {
		return false, err
	}
	if h, ok := r.healthy[addr]; ok {
		return h, nil
	}
	return true, nil
}

func (r *fakeRunner) Start(_ context.Context, addr string, req StartReq) (string, error) {
	r.mu.Lock()
	r.starts = append(r.starts, req)
	r.addrs = append(r.addrs, addr)
	r.nextID++
	id := "rid-" + string(rune('a'+r.nextID-1))
	f := r.start
	r.mu.Unlock()
	if f != nil {
		return f(addr, req)
	}
	return id, nil
}

func (r *fakeRunner) Status(ctx context.Context, _, id string) (RunStatus, error) {
	r.mu.Lock()
	r.calls++
	n := r.calls
	f := r.status
	r.mu.Unlock()
	if f != nil {
		return f(ctx, id, n)
	}
	return RunStatus{State: "succeeded", Result: map[string]any{"ok": float64(1)}}, nil
}

func (r *fakeRunner) Cancel(_ context.Context, addr, id string) error {
	r.mu.Lock()
	r.cancels = append(r.cancels, id)
	r.cAddrs = append(r.cAddrs, addr)
	r.mu.Unlock()
	return nil
}

func (r *fakeRunner) startCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.starts)
}

func (r *fakeRunner) cancelCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.cancels)
}

type notifyCall struct {
	task, reason string
	result       map[string]any
	ctxErr       error
	deadline     bool
}

type fakeNotifier struct {
	mu        sync.Mutex
	successes []notifyCall
	failures  []notifyCall
	ret       NotifyResult
}

func (n *fakeNotifier) Success(ctx context.Context, task string, _ Task, result map[string]any) NotifyResult {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, dl := ctx.Deadline()
	n.successes = append(n.successes, notifyCall{task: task, result: result, ctxErr: ctx.Err(), deadline: dl})
	return n.ret
}

func (n *fakeNotifier) Failure(ctx context.Context, task string, _ Task, reason string) NotifyResult {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, dl := ctx.Deadline()
	n.failures = append(n.failures, notifyCall{task: task, reason: reason, ctxErr: ctx.Err(), deadline: dl})
	return n.ret
}

func (n *fakeNotifier) counts() (int, int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.successes), len(n.failures)
}

// --- fixture -----------------------------------------------------------------

var t0 = time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)

const jobScript = "#!/bin/sh\necho '{\"ok\":1}'\n"

func baseTask() Task {
	return Task{
		Script:   "job.sh",
		Target:   Target{Node: "n1"},
		Schedule: Schedule{Every: Duration(time.Hour)},
		Timeout:  Duration(time.Minute),
		Retry:    Retry{Attempts: 1},
	}
}

type engFixture struct {
	kv       *memKV
	store    *Store
	clock    *fakeClock
	nodes    *fakeNodes
	runner   *fakeRunner
	notifier *fakeNotifier
	rand     func() float64
	e        *Engine
}

func newEngFixture(t *testing.T, tasks map[string]Task) *engFixture {
	t.Helper()
	kv := newMemKV()
	s := openStore(t, kv)
	if err := s.PutConfig(&Config{Tasks: tasks}, map[string]string{"job.sh": jobScript}); err != nil {
		t.Fatal(err)
	}
	f := &engFixture{
		kv: kv, store: s,
		clock: &fakeClock{t: t0},
		nodes: &fakeNodes{nodes: []headscale.Node{
			runnerNode("n1", "100.64.0.1"),
			runnerNode("n2", "100.64.0.2"),
		}},
		runner:   &fakeRunner{},
		notifier: &fakeNotifier{},
		rand:     func() float64 { return 0.5 }, // no jitter
	}
	f.e = f.newEngine()
	return f
}

func (f *engFixture) newEngine() *Engine {
	e := NewEngine(Deps{
		Store: f.store, Nodes: f.nodes, Runner: f.runner, Notifier: f.notifier,
		Now: f.clock.Now, Rand: func() float64 { return f.rand() },
		PollEvery: 2 * time.Millisecond, Log: quiet,
	})
	e.startSpread = 0
	return e
}

// step launches what is due now and waits for it to finish.
func (f *engFixture) step() time.Time {
	next := f.e.tick(context.Background())
	f.e.wait()
	return next
}

func (f *engFixture) runs(task string) []Run { return f.store.Runs(task) }

func oneTask(mod func(*Task)) map[string]Task {
	t := baseTask()
	if mod != nil {
		mod(&t)
	}
	return map[string]Task{"job": t}
}

func failStatus(msg string) func(context.Context, string, int) (RunStatus, error) {
	return func(context.Context, string, int) (RunStatus, error) {
		return RunStatus{State: "failed", Error: msg}, nil
	}
}

// --- tests -------------------------------------------------------------------

func TestBackoff(t *testing.T) {
	r := Retry{Attempts: 4, Backoff: []Duration{Duration(time.Minute), Duration(5 * time.Minute)}}
	for attempt, want := range map[int]time.Duration{1: time.Minute, 2: 5 * time.Minute, 3: 5 * time.Minute, 9: 5 * time.Minute} {
		if got := Backoff(r, attempt); got != want {
			t.Errorf("Backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
	if got := Backoff(Retry{Attempts: 1}, 1); got != 0 {
		t.Errorf("empty backoff = %s", got)
	}
}

func TestEngineRunsOnScheduleAndReschedules(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))

	next := f.step() // zero NextRun: due at once
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunSucceeded || rs[0].Node != "n1" || rs[0].Attempt != 1 {
		t.Fatalf("runs = %+v", rs)
	}
	st := f.store.State("job")
	if !st.NextRun.Equal(t0.Add(time.Hour)) || !st.LastSuccess.Equal(t0) || st.Attempt != 0 {
		t.Fatalf("state = %+v", st)
	}
	if !next.IsZero() {
		t.Fatalf("tick that launched the only task reported wake %s", next)
	}
	if s, _ := f.notifier.counts(); s != 1 {
		t.Fatalf("success notifications = %d", s)
	}

	f.clock.Set(t0.Add(30 * time.Minute))
	if next := f.step(); !next.Equal(t0.Add(time.Hour)) {
		t.Fatalf("wake = %s, want next run", next)
	}
	if n := len(f.runs("job")); n != 1 {
		t.Fatalf("ran early: %d runs", n)
	}

	f.clock.Set(t0.Add(time.Hour))
	f.step()
	if n := len(f.runs("job")); n != 2 {
		t.Fatalf("runs after the interval = %d", n)
	}
	if st := f.store.State("job"); !st.NextRun.Equal(t0.Add(2 * time.Hour)) {
		t.Fatalf("not rescheduled: %+v", st)
	}
}

func TestEngineJitterWithinBounds(t *testing.T) {
	cases := []struct {
		every, jitter time.Duration
		rand          float64
		want          time.Duration
	}{
		{time.Hour, 10 * time.Minute, 0, 50 * time.Minute},
		{time.Hour, 10 * time.Minute, 0.5, time.Hour},
		{time.Hour, 10 * time.Minute, 0.75, 65 * time.Minute},
		{time.Hour, 45 * time.Minute, 0, 30 * time.Minute}, // never earlier than every/2
	}
	for _, c := range cases {
		f := newEngFixture(t, oneTask(func(tk *Task) {
			tk.Schedule = Schedule{Every: Duration(c.every), Jitter: Duration(c.jitter)}
		}))
		r := c.rand
		f.rand = func() float64 { return r }
		f.step()
		got := f.store.State("job").NextRun.Sub(t0)
		if got != c.want {
			t.Errorf("every %s jitter %s rand %v: next in %s, want %s", c.every, c.jitter, c.rand, got, c.want)
		}
	}

	// Many random draws stay inside [every-jitter, every+jitter].
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Schedule = Schedule{Every: Duration(time.Hour), Jitter: Duration(10 * time.Minute)}
	}))
	f.rand = func() float64 { return float64(time.Now().UnixNano()%1000) / 1000 }
	for i := 0; i < 20; i++ {
		f.clock.Set(f.store.State("job").NextRun.Add(time.Second))
		start := f.clock.Now()
		f.step()
		d := f.store.State("job").NextRun.Sub(start)
		if d < 50*time.Minute || d > 70*time.Minute {
			t.Fatalf("draw %d: next in %s", i, d)
		}
	}
}

func TestEngineRetriesWithBackoffThenNotifiesOnce(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 3, Backoff: []Duration{Duration(time.Minute), Duration(5 * time.Minute)}}
	}))
	f.runner.status = failStatus("exit 1")

	f.step()
	st := f.store.State("job")
	if st.Attempt != 1 || !st.AttemptAt.Equal(t0.Add(time.Minute)) || st.Streak != 0 {
		t.Fatalf("after attempt 1: %+v", st)
	}
	f.clock.Set(t0.Add(30 * time.Second))
	if next := f.step(); !next.Equal(t0.Add(time.Minute)) {
		t.Fatalf("wake = %s", next)
	}
	if n := len(f.runs("job")); n != 1 {
		t.Fatalf("retried before backoff: %d runs", n)
	}

	f.clock.Set(t0.Add(time.Minute))
	f.step()
	st = f.store.State("job")
	if st.Attempt != 2 || !st.AttemptAt.Equal(t0.Add(6*time.Minute)) {
		t.Fatalf("after attempt 2: %+v", st)
	}
	if _, fl := f.notifier.counts(); fl != 0 {
		t.Fatalf("failure notified before the last attempt: %d", fl)
	}

	f.clock.Set(t0.Add(6 * time.Minute))
	f.step()
	st = f.store.State("job")
	if st.Attempt != 0 || !st.AttemptAt.IsZero() || st.Streak != 1 || !st.NextRun.Equal(t0.Add(6*time.Minute+time.Hour)) {
		t.Fatalf("after last attempt: %+v", st)
	}
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failure notifications = %d, want 1", fl)
	}
	rs := f.runs("job")
	if len(rs) != 3 {
		t.Fatalf("runs = %d", len(rs))
	}
	for i, r := range rs {
		if r.Attempt != 3-i || r.State != RunFailed || r.Reason != "exit 1" {
			t.Fatalf("run %d = %+v", i, r)
		}
	}
	// Nothing more happens before the next interval.
	f.step()
	if n := len(f.runs("job")); n != 3 {
		t.Fatalf("extra run: %d", n)
	}
}

func TestEngineSuccessAfterFailureResetsStreak(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.runner.status = failStatus("boom")
	f.step()
	if st := f.store.State("job"); st.Streak != 1 {
		t.Fatalf("streak = %d", st.Streak)
	}
	f.runner.mu.Lock()
	f.runner.status = nil
	f.runner.mu.Unlock()
	f.clock.Set(t0.Add(time.Hour))
	f.step()
	st := f.store.State("job")
	if st.Streak != 0 || !st.LastSuccess.Equal(t0.Add(time.Hour)) {
		t.Fatalf("state = %+v", st)
	}
	if s, fl := f.notifier.counts(); s != 1 || fl != 1 {
		t.Fatalf("notifications success=%d failure=%d", s, fl)
	}
}

func TestEngineNoEligibleNodeCountsAsFailedAttempt(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	}))
	f.runner.healthy = map[string]bool{"100.64.0.1": false}
	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunFailed || rs[0].Reason != "no eligible node" || rs[0].Node != "" {
		t.Fatalf("runs = %+v", rs)
	}
	if f.runner.startCount() != 0 {
		t.Fatal("started without an eligible node")
	}
	if st := f.store.State("job"); st.Attempt != 1 {
		t.Fatalf("state = %+v", st)
	}
	f.clock.Set(t0.Add(time.Minute))
	f.step()
	if st := f.store.State("job"); st.Streak != 1 || st.Attempt != 0 {
		t.Fatalf("state = %+v", st)
	}
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failures = %d", fl)
	}
}

func TestEngineLostWhenRunnerForgetsRun(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		return RunStatus{}, ErrRunUnknown
	}
	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunLost || rs[0].Reason != "runner lost the run" {
		t.Fatalf("runs = %+v", rs)
	}
	if st := f.store.State("job"); st.Streak != 1 {
		t.Fatalf("lost run not counted as failed: %+v", st)
	}
}

func TestEngineLostWhenRunnerUnreachableBeyondTimeoutPlusMargin(t *testing.T) {
	f := newEngFixture(t, oneTask(nil)) // timeout 1m: lost at 1m30s
	var lastAt time.Time
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		f.clock.Advance(20 * time.Second)
		lastAt = f.clock.Now()
		return RunStatus{}, errors.New("dial tcp: connection refused")
	}
	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunLost || rs[0].Reason != "runner unreachable" {
		t.Fatalf("runs = %+v", rs)
	}
	if d := lastAt.Sub(t0); d < 90*time.Second || d >= 110*time.Second {
		t.Fatalf("gave up after %s, want at timeout+30s", d)
	}
	if f.runner.cancelCount() != 0 {
		t.Fatal("cancel sent to an unreachable runner")
	}
}

func TestEngineTimeoutCancelsAndFails(t *testing.T) {
	f := newEngFixture(t, oneTask(nil)) // timeout 1m: cancel at 1m10s
	var lastAt time.Time
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		f.clock.Advance(20 * time.Second)
		lastAt = f.clock.Now()
		return RunStatus{State: "running"}, nil
	}
	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunFailed || rs[0].Reason != "timeout" {
		t.Fatalf("runs = %+v", rs)
	}
	if f.runner.cancelCount() != 1 {
		t.Fatalf("cancels = %d", f.runner.cancelCount())
	}
	if d := lastAt.Sub(t0); d < 70*time.Second || d >= 90*time.Second {
		t.Fatalf("timed out after %s", d)
	}
	f.runner.mu.Lock()
	timeout := f.runner.starts[0].TimeoutSeconds
	f.runner.mu.Unlock()
	if timeout != 60 {
		t.Fatalf("TimeoutSeconds = %d", timeout)
	}
}

func TestEngineResumesBackoffAfterRestart(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 3, Backoff: []Duration{Duration(time.Minute), Duration(5 * time.Minute)}}
	}))
	if err := f.store.SaveState("job", TaskState{Attempt: 2, AttemptAt: t0.Add(5 * time.Minute), NextRun: t0.Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// "Restart": reopen the store from the same backing KV and build a new engine.
	f.store = openStore(t, f.kv)
	f.runner.status = failStatus("exit 2")
	f.e = f.newEngine()

	if next := f.step(); !next.Equal(t0.Add(5 * time.Minute)) {
		t.Fatalf("wake = %s, want the pending AttemptAt", next)
	}
	if f.runner.startCount() != 0 || len(f.runs("job")) != 0 {
		t.Fatal("ran before the persisted backoff ended")
	}
	f.clock.Set(t0.Add(5 * time.Minute))
	f.step()
	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].Attempt != 3 {
		t.Fatalf("runs = %+v", rs)
	}
	st := f.store.State("job")
	if st.Streak != 1 || st.Attempt != 0 {
		t.Fatalf("state = %+v", st)
	}
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failures = %d", fl)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestEngineOverdueRunsAfterStart(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if err := f.store.SaveState("job", TaskState{NextRun: t0.Add(-2 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.e.Run(ctx); close(done) }()
	waitFor(t, "overdue run", func() bool {
		rs := f.runs("job")
		return len(rs) == 1 && rs[0].State == RunSucceeded
	})
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if n := len(f.runs("job")); n != 1 {
		t.Fatalf("runs = %d", n)
	}
	if st := f.store.State("job"); !st.NextRun.Equal(t0.Add(time.Hour)) {
		t.Fatalf("state = %+v", st)
	}
}

func TestEngineTriggerWhileRunningReturnsErrAlreadyRunning(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	release := make(chan struct{})
	f.runner.status = func(ctx context.Context, _ string, _ int) (RunStatus, error) {
		select {
		case <-release:
			return RunStatus{State: "succeeded", Result: map[string]any{}}, nil
		case <-ctx.Done():
			return RunStatus{}, ctx.Err()
		}
	}
	id, err := f.e.Trigger("job")
	if err != nil || len(id) != 16 {
		t.Fatalf("trigger = %q, %v", id, err)
	}
	waitFor(t, "running record", func() bool {
		r, ok := f.e.Running("job")
		return ok && r.ID == id && r.State == RunRunning && r.Node == "n1"
	})
	// The stored record names the runner's run, so a successor can cancel it.
	waitFor(t, "stored runner id", func() bool {
		rs := f.runs("job")
		return len(rs) == 1 && rs[0].RunnerID == "rid-a" && rs[0].RunnerAddr == "100.64.0.1" && rs[0].State == RunRunning
	})
	if _, err := f.e.Trigger("job"); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second trigger: %v", err)
	}
	if _, err := f.e.Trigger("nope"); !errors.Is(err, ErrUnknownTask) {
		t.Fatalf("unknown task: %v", err)
	}
	// The scheduler does not start a second copy either.
	f.e.tick(context.Background())
	close(release)
	f.e.wait()
	if _, ok := f.e.Running("job"); ok {
		t.Fatal("still running after the attempt ended")
	}
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].ID != id || rs[0].State != RunSucceeded {
		t.Fatalf("runs = %+v", rs)
	}
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatalf("trigger after finish: %v", err)
	}
	f.e.wait()
}

func TestEngineStaleNextRunNeverFiresTwice(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if err := f.store.SaveState("job", TaskState{NextRun: t0.Add(-5 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	f.e.tick(context.Background())
	f.e.tick(context.Background()) // while the first is still in flight
	f.e.wait()
	f.step()
	f.step()
	if n := len(f.runs("job")); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	if st := f.store.State("job"); !st.NextRun.Equal(t0.Add(time.Hour)) {
		t.Fatalf("NextRun = %s", st.NextRun)
	}
}

func TestEngineScriptSHARecorded(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.step()
	sum := sha256.Sum256([]byte(jobScript))
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].ScriptSHA != hex.EncodeToString(sum[:]) {
		t.Fatalf("runs = %+v", rs)
	}
	f.runner.mu.Lock()
	defer f.runner.mu.Unlock()
	if f.runner.starts[0].Script != jobScript || f.runner.addrs[0] != "100.64.0.1" {
		t.Fatalf("start = %+v at %s", f.runner.starts[0], f.runner.addrs[0])
	}
}

func TestEngineSecretsPassedOnlyListedAndScrubbedFromResult(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Secrets = []string{"token"}
		tk.Params = map[string]any{"mode": "fast"}
	}))
	for n, v := range map[string]string{"token": "tok-s3cret-value", "other": "other-value"} {
		if err := f.store.PutSecret(n, v); err != nil {
			t.Fatal(err)
		}
	}
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		return RunStatus{State: "succeeded", Result: map[string]any{
			"msg":    "got tok-s3cret-value here",
			"n":      float64(42),
			"nested": map[string]any{"list": []any{"tok-s3cret-value", float64(1)}},
		}}, nil
	}
	f.step()

	f.runner.mu.Lock()
	req := f.runner.starts[0]
	f.runner.mu.Unlock()
	if len(req.Secrets) != 1 || req.Secrets["token"] != "tok-s3cret-value" || req.Params["mode"] != "fast" {
		t.Fatalf("start request secrets=%v params=%v", req.Secrets, req.Params)
	}
	r := f.runs("job")[0]
	if r.Result["msg"] != "got *** here" || r.Result["n"] != float64(42) {
		t.Fatalf("result = %v", r.Result)
	}
	list := r.Result["nested"].(map[string]any)["list"].([]any)
	if list[0] != "***" || list[1] != float64(1) {
		t.Fatalf("nested = %v", list)
	}
	f.notifier.mu.Lock()
	got := f.notifier.successes[0].result["msg"]
	f.notifier.mu.Unlock()
	if got != "got *** here" {
		t.Fatalf("notifier saw %v", got)
	}

	// A failure reason carrying the secret is scrubbed too.
	f.runner.mu.Lock()
	f.runner.status = failStatus("auth failed for tok-s3cret-value")
	f.runner.mu.Unlock()
	f.clock.Set(t0.Add(time.Hour))
	f.step()
	r = f.runs("job")[0]
	if strings.Contains(r.Reason, "tok-s3cret-value") || r.Reason != "auth failed for ***" {
		t.Fatalf("reason = %q", r.Reason)
	}
	f.notifier.mu.Lock()
	reason := f.notifier.failures[0].reason
	f.notifier.mu.Unlock()
	if strings.Contains(reason, "tok-s3cret-value") {
		t.Fatalf("failure notification carried the secret: %q", reason)
	}
}

func TestEngineIdleDoesNoNodeListing(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if err := f.store.SaveState("job", TaskState{NextRun: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.e.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	f.e.Reload()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if n := f.nodes.calls.Load(); n != 0 {
		t.Fatalf("idle engine listed nodes %d times", n)
	}

	f.e = f.newEngine() // an Engine runs once
	f.clock.Set(t0.Add(time.Hour))
	f.step()
	if n := f.nodes.calls.Load(); n != 1 {
		t.Fatalf("listings for one run = %d", n)
	}
	for i := 0; i < 5; i++ {
		f.step()
	}
	if n := f.nodes.calls.Load(); n != 1 {
		t.Fatalf("listings between runs = %d", n)
	}
}

func TestEngineBadResultIsFailedAttempt(t *testing.T) {
	for name, result := range map[string]map[string]any{
		"missing": {"other": float64(3)},
		"string":  {"free": "12"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newEngFixture(t, oneTask(func(tk *Task) {
				tk.Alert = &Alert{When: Condition{Field: "free", Op: "<", Value: 10}, Notify: Notify{Topic: "topic"}}
			}))
			f.runner.status = func(context.Context, string, int) (RunStatus, error) {
				return RunStatus{State: "succeeded", Result: result}, nil
			}
			f.step()
			r := f.runs("job")[0]
			if r.State != RunFailed || !strings.HasPrefix(r.Reason, "bad result: ") || !strings.Contains(r.Reason, `"free"`) {
				t.Fatalf("run = %+v", r)
			}
			if strings.Contains(r.Reason, "12") {
				t.Fatalf("reason leaks the value: %q", r.Reason)
			}
			if s, fl := f.notifier.counts(); s != 0 || fl != 1 {
				t.Fatalf("success=%d failure=%d", s, fl)
			}
			if st := f.store.State("job"); st.Streak != 1 || !st.LastSuccess.IsZero() {
				t.Fatalf("state = %+v", st)
			}
		})
	}
}

func TestEngineStoresNotifyOutcome(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.notifier.ret = NotifyResult{Alert: "sent", Ping: "sent"}
	f.step()
	if got := f.runs("job")[0].NotifyOutcome; got != "alert=sent ping=sent" {
		t.Fatalf("outcome = %q", got)
	}

	f.notifier.mu.Lock()
	f.notifier.ret = NotifyResult{Ping: "failed: timeout"}
	f.notifier.mu.Unlock()
	f.runner.status = failStatus("x")
	f.clock.Set(t0.Add(time.Hour))
	f.step()
	if got := f.runs("job")[0].NotifyOutcome; got != "ping=failed: timeout" {
		t.Fatalf("failure outcome = %q", got)
	}
}

func TestEngineManualTriggerDoesNotTouchNextRun(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	}))
	next := t0.Add(30 * time.Minute)
	if err := f.store.SaveState("job", TaskState{NextRun: next}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatal(err)
	}
	f.e.wait()
	st := f.store.State("job")
	if !st.NextRun.Equal(next) || !st.LastSuccess.Equal(t0) {
		t.Fatalf("after manual success: %+v", st)
	}
	if s, _ := f.notifier.counts(); s != 1 {
		t.Fatalf("manual success not notified: %d", s)
	}

	// A failing manual run retries through the same flow, still leaving NextRun.
	f.runner.mu.Lock()
	f.runner.status = failStatus("nope")
	f.runner.mu.Unlock()
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatal(err)
	}
	f.e.wait()
	if st := f.store.State("job"); st.Attempt != 1 || !st.NextRun.Equal(next) {
		t.Fatalf("after manual attempt 1: %+v", st)
	}
	f.clock.Set(t0.Add(time.Minute))
	f.step()
	st = f.store.State("job")
	if st.Attempt != 0 || st.Streak != 1 || !st.NextRun.Equal(next) {
		t.Fatalf("after manual attempt 2: %+v", st)
	}
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failures = %d", fl)
	}

	// Trigger while only a retry is pending: runs now as attempt 1.
	if err := f.store.SaveState("job", TaskState{NextRun: next, Attempt: 1, AttemptAt: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatalf("trigger with pending retry: %v", err)
	}
	f.e.wait()
	if r := f.runs("job")[0]; r.Attempt != 1 {
		t.Fatalf("attempt = %d", r.Attempt)
	}
}

func TestEngineShutdownDoesNotNotifyOrCountStreak(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 3, Backoff: []Duration{Duration(time.Minute)}}
	}))
	pending := TaskState{Attempt: 1, AttemptAt: t0.Add(-time.Second), NextRun: t0.Add(-time.Hour)}
	if err := f.store.SaveState("job", pending); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{}, 1)
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return RunStatus{State: "running"}, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	f.e.tick(ctx)
	<-polled
	cancel()
	f.e.wait()

	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunFailed || rs[0].Reason != "shutdown" || rs[0].Attempt != 2 {
		t.Fatalf("runs = %+v", rs)
	}
	st := f.store.State("job")
	if st.Attempt != 1 || !st.AttemptAt.Equal(pending.AttemptAt) || st.Streak != 0 || !st.NextRun.Equal(pending.NextRun) {
		t.Fatalf("state changed on shutdown: %+v", st)
	}
	if s, fl := f.notifier.counts(); s != 0 || fl != 0 {
		t.Fatalf("notified on shutdown: success=%d failure=%d", s, fl)
	}
	if f.runner.cancelCount() != 1 {
		t.Fatalf("cancels = %d", f.runner.cancelCount())
	}
	// The next start retries the same attempt number.
	f.e = f.newEngine()
	f.runner.mu.Lock()
	f.runner.status = nil
	f.runner.mu.Unlock()
	f.step()
	if r := f.runs("job")[0]; r.Attempt != 2 || r.State != RunSucceeded {
		t.Fatalf("resumed run = %+v", r)
	}
}

func TestEngineMissingSecretOrScriptFailsAttempt(t *testing.T) {
	t.Run("secret", func(t *testing.T) {
		f := newEngFixture(t, oneTask(func(tk *Task) { tk.Secrets = []string{"token", "other"} }))
		if err := f.store.PutSecret("token", "tok-value-xyz"); err != nil {
			t.Fatal(err)
		}
		f.step()
		r := f.runs("job")[0]
		if r.State != RunFailed || r.Reason != "secret not configured: other" {
			t.Fatalf("run = %+v", r)
		}
		if f.runner.startCount() != 0 || f.nodes.calls.Load() != 0 {
			t.Fatal("went on to the node")
		}
		f.notifier.mu.Lock()
		defer f.notifier.mu.Unlock()
		if len(f.notifier.failures) != 1 || strings.Contains(f.notifier.failures[0].reason, "tok-value-xyz") {
			t.Fatalf("failures = %+v", f.notifier.failures)
		}
	})
	t.Run("script", func(t *testing.T) {
		f := newEngFixture(t, oneTask(func(tk *Task) { tk.Script = "gone.sh" }))
		f.step()
		r := f.runs("job")[0]
		if r.State != RunFailed || r.Reason != "script not found: gone.sh" {
			t.Fatalf("run = %+v", r)
		}
		if f.runner.startCount() != 0 {
			t.Fatal("started without a script")
		}
	})
}

func TestEngineConcurrentTasksRunInParallel(t *testing.T) {
	a, b := baseTask(), baseTask()
	b.Target = Target{Node: "n2"}
	f := newEngFixture(t, map[string]Task{"a": a, "b": b})
	var started atomic.Int32
	both := make(chan struct{})
	f.runner.start = func(addr string, _ StartReq) (string, error) {
		if started.Add(1) == 2 {
			close(both)
		}
		return "id-" + addr, nil
	}
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		select {
		case <-both:
			return RunStatus{State: "succeeded", Result: map[string]any{}}, nil
		case <-time.After(2 * time.Second):
			return RunStatus{State: "failed", Error: "ran one at a time"}, nil
		}
	}
	f.step()
	for _, task := range []string{"a", "b"} {
		rs := f.runs(task)
		if len(rs) != 1 || rs[0].State != RunSucceeded {
			t.Fatalf("%s runs = %+v", task, rs)
		}
	}
}

func TestEngineRunnerBusyIsFailedAttempt(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.runner.start = func(string, StartReq) (string, error) { return "", ErrRunnerBusy }
	f.step()
	r := f.runs("job")[0]
	if r.State != RunFailed || r.Reason != "node busy" || r.Node != "n1" {
		t.Fatalf("run = %+v", r)
	}
	if st := f.store.State("job"); st.Streak != 1 {
		t.Fatalf("state = %+v", st)
	}
}

func TestEngineTriggerDuringRetryChainSetsNextRun(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	}))
	f.runner.status = failStatus("exit 1")
	f.step() // scheduled attempt 1 fails; NextRun is still zero
	if st := f.store.State("job"); st.Attempt != 1 || !st.NextRun.IsZero() {
		t.Fatalf("state = %+v", st)
	}
	f.runner.mu.Lock()
	f.runner.status = nil
	f.runner.mu.Unlock()
	f.clock.Set(t0.Add(10 * time.Second))
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatal(err)
	}
	f.e.wait()
	st := f.store.State("job")
	if st.Attempt != 0 || !st.NextRun.Equal(t0.Add(10*time.Second+time.Hour)) {
		t.Fatalf("after manual success: %+v", st)
	}
	f.step()
	if n := len(f.runs("job")); n != 2 {
		t.Fatalf("runs = %d, want 2 (no extra scheduled sequence)", n)
	}
	if _, fl := f.notifier.counts(); fl != 0 {
		t.Fatalf("failures = %d", fl)
	}
}

func TestEngineTriggerBeforeFirstRunRunsOnce(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatal(err)
	}
	f.e.wait()
	f.step()
	if n := len(f.runs("job")); n != 1 {
		t.Fatalf("runs = %d, want 1", n)
	}
	if st := f.store.State("job"); !st.NextRun.Equal(t0.Add(time.Hour)) {
		t.Fatalf("NextRun = %s", st.NextRun)
	}
}

func TestEngineStartupSweepsGhostRuns(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	pending := TaskState{NextRun: t0.Add(time.Hour), Attempt: 1, AttemptAt: t0.Add(time.Hour), Streak: 2}
	if err := f.store.SaveState("job", pending); err != nil {
		t.Fatal(err)
	}
	ghosts := []Run{
		{ID: "g1", Task: "job", Node: "n1", Attempt: 1, State: RunRunning, Started: t0.Add(-time.Minute),
			RunnerID: "orphan-1", RunnerAddr: "100.64.0.1"},
		{ID: "g2", Task: "job", Attempt: 1, State: RunQueued, Started: t0.Add(-time.Minute)},
		{ID: "done", Task: "job", Node: "n1", Attempt: 1, State: RunSucceeded, Ended: t0.Add(-2 * time.Hour),
			RunnerID: "old", RunnerAddr: "100.64.0.1"},
		{ID: "bad", Task: "job", Node: "n1", Attempt: 1, State: RunFailed, Reason: "exit 1", RunnerID: "old2", RunnerAddr: "100.64.0.1"},
	}
	for _, g := range ghosts {
		if err := f.store.AppendRun(g); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.e.Run(ctx); close(done) }()
	waitFor(t, "ghosts finalised", func() bool {
		for _, r := range f.runs("job") {
			if r.State == RunRunning || r.State == RunQueued {
				return false
			}
		}
		return true
	})
	cancel()
	<-done

	byID := map[string]Run{}
	for _, r := range f.runs("job") {
		byID[r.ID] = r
	}
	for _, id := range []string{"g1", "g2"} {
		r := byID[id]
		if r.State != RunLost || r.Reason != "node-control restarted" || !r.Ended.Equal(t0) {
			t.Fatalf("%s = %+v", id, r)
		}
	}
	if byID["done"].State != RunSucceeded || byID["bad"].State != RunFailed || byID["bad"].Reason != "exit 1" {
		t.Fatalf("finished runs touched: %+v %+v", byID["done"], byID["bad"])
	}
	f.runner.mu.Lock()
	cancels, addrs := f.runner.cancels, f.runner.cAddrs
	f.runner.mu.Unlock()
	if len(cancels) != 1 || cancels[0] != "orphan-1" || addrs[0] != "100.64.0.1" {
		t.Fatalf("cancels = %v at %v", cancels, addrs)
	}
	if st := f.store.State("job"); st != pending {
		t.Fatalf("sweep changed state: %+v", st)
	}
	if s, fl := f.notifier.counts(); s != 0 || fl != 0 {
		t.Fatalf("sweep notified: success=%d failure=%d", s, fl)
	}
	if len(f.runs("job")) != 4 || f.runner.startCount() != 0 {
		t.Fatal("sweep started or added runs")
	}
}

func TestEngineStartupSpreadPerTask(t *testing.T) {
	f := newEngFixture(t, map[string]Task{"a": baseTask(), "b": baseTask()})
	for _, task := range []string{"a", "b"} {
		if err := f.store.SaveState(task, TaskState{NextRun: t0.Add(-time.Hour)}); err != nil {
			t.Fatal(err)
		}
	}
	draws := []float64{0.2, 0.7} // a, b in name order
	f.rand = func() float64 {
		if len(draws) == 0 {
			return 0.5
		}
		d := draws[0]
		draws = draws[1:]
		return d
	}
	f.e.startSpread = 10 * time.Second
	f.e.spreadStart()

	if next := f.step(); !next.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("wake = %s, want a's delay", next)
	}
	if len(f.runs("a"))+len(f.runs("b")) != 0 {
		t.Fatal("ran before its startup delay")
	}
	f.clock.Set(t0.Add(2 * time.Second))
	if next := f.step(); !next.Equal(t0.Add(7 * time.Second)) {
		t.Fatalf("wake = %s, want b's delay", next)
	}
	if len(f.runs("a")) != 1 || len(f.runs("b")) != 0 {
		t.Fatalf("after 2s: a=%d b=%d", len(f.runs("a")), len(f.runs("b")))
	}
	f.clock.Set(t0.Add(7 * time.Second))
	f.step()
	if len(f.runs("b")) != 1 {
		t.Fatal("b did not run after its delay")
	}
}

func TestEngineOverlappingSecretsScrubbedLongestFirst(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) { tk.Secrets = []string{"short", "long"} }))
	for n, v := range map[string]string{"short": "abc", "long": "abcdef"} {
		if err := f.store.PutSecret(n, v); err != nil {
			t.Fatal(err)
		}
	}
	f.runner.status = failStatus("saw abcdef and abc")
	f.step()
	if r := f.runs("job")[0]; r.Reason != "saw *** and ***" {
		t.Fatalf("reason = %q", r.Reason)
	}
}

func TestEngineNotifierSurvivesShutdown(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	ctx, cancel := context.WithCancel(context.Background())
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		cancel() // shutdown arrives just as the run ends
		return RunStatus{State: "failed", Error: "exit 1"}, nil
	}
	f.e.tick(ctx)
	f.e.wait()
	f.notifier.mu.Lock()
	defer f.notifier.mu.Unlock()
	if len(f.notifier.failures) != 1 {
		t.Fatalf("failures = %d", len(f.notifier.failures))
	}
	if c := f.notifier.failures[0]; c.ctxErr != nil || !c.deadline {
		t.Fatalf("notifier ctx err=%v deadline=%v", c.ctxErr, c.deadline)
	}
}

func TestEngineTriggerBeforeRunStopsWithRun(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	}))
	if err := f.store.SaveState("job", TaskState{NextRun: t0.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{}, 1)
	f.runner.status = func(context.Context, string, int) (RunStatus, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return RunStatus{State: "running"}, nil
	}
	if _, err := f.e.Trigger("job"); err != nil {
		t.Fatal(err)
	}
	<-polled
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.e.Run(ctx); close(done) }()
	cancel()
	<-done
	if r := f.runs("job")[0]; r.State != RunFailed || r.Reason != "shutdown" {
		t.Fatalf("run = %+v", r)
	}
	if _, err := f.e.Trigger("job"); !errors.Is(err, ErrStopped) {
		t.Fatalf("trigger after stop: %v", err)
	}
}

func TestEngineShutdownWaitIsBounded(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	release := make(chan struct{})
	defer close(release)
	entered := make(chan struct{})
	f.runner.start = func(string, StartReq) (string, error) {
		close(entered)
		<-release // ignores ctx, like a hung connection
		return "x", nil
	}
	f.e.shutdownWait = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.e.Run(ctx); close(done) }()
	<-entered
	began := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within its shutdown bound")
	}
	if d := time.Since(began); d < 40*time.Millisecond {
		t.Fatalf("Run returned after %s without waiting", d)
	}
}

// blockRunning keeps every run "running" until its context ends.
func blockRunning(polled chan<- struct{}) func(context.Context, string, int) (RunStatus, error) {
	return func(ctx context.Context, _ string, _ int) (RunStatus, error) {
		select {
		case polled <- struct{}{}:
		default:
		}
		return RunStatus{State: "running"}, nil
	}
}

func TestEngineCancelByOperatorEndsSequence(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 3, Backoff: []Duration{Duration(time.Minute)}}
		tk.OnFailure = &OnFailure{Notify: Notify{Topic: "TOPIC"}}
	}))
	// A scheduled retry sequence is in progress: attempt 2 is due now.
	pending := TaskState{Attempt: 1, AttemptAt: t0.Add(-time.Second), NextRun: t0.Add(-time.Hour), Streak: 2}
	if err := f.store.SaveState("job", pending); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{}, 1)
	f.runner.status = blockRunning(polled)
	f.e.tick(context.Background())
	<-polled
	r, ok := f.e.Running("job")
	if !ok || r.State != RunRunning {
		t.Fatalf("running = %+v, %v", r, ok)
	}
	if err := f.e.Cancel(r.ID); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	f.e.wait()

	rs := f.runs("job")
	if len(rs) != 1 || rs[0].ID != r.ID || rs[0].State != RunCancelled || rs[0].Reason != "cancelled by operator" {
		t.Fatalf("runs = %+v", rs)
	}
	st := f.store.State("job")
	if st.Attempt != 0 || !st.AttemptAt.IsZero() || st.Streak != 2 {
		t.Fatalf("state after cancel: %+v", st)
	}
	if want := t0.Add(time.Hour); !st.NextRun.Equal(want) {
		t.Fatalf("NextRun = %s, want %s", st.NextRun, want)
	}
	if s, fl := f.notifier.counts(); s != 0 || fl != 0 {
		t.Fatalf("notified on cancel: success=%d failure=%d", s, fl)
	}
	if f.runner.cancelCount() != 1 {
		t.Fatalf("runner cancels = %d", f.runner.cancelCount())
	}
	// Nothing is retried: the next due time is the schedule.
	if next := f.step(); !next.Equal(t0.Add(time.Hour)) || f.runner.startCount() != 1 {
		t.Fatalf("after cancel: next=%s starts=%d", next, f.runner.startCount())
	}
}

func TestEngineCancelManualKeepsFutureNextRun(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	future := t0.Add(30 * time.Minute)
	if err := f.store.SaveState("job", TaskState{NextRun: future}); err != nil {
		t.Fatal(err)
	}
	polled := make(chan struct{}, 1)
	f.runner.status = blockRunning(polled)
	id, err := f.e.Trigger("job")
	if err != nil {
		t.Fatal(err)
	}
	<-polled
	if err := f.e.Cancel(id); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	f.e.wait()
	if st := f.store.State("job"); !st.NextRun.Equal(future) || st.Attempt != 0 {
		t.Fatalf("state = %+v", st)
	}
	if rs := f.runs("job"); len(rs) != 1 || rs[0].State != RunCancelled {
		t.Fatalf("runs = %+v", rs)
	}
}

func TestEngineCancelUnknownOrFinishedRun(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if err := f.e.Cancel("nope"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("unknown: %v", err)
	}
	id, err := f.e.Trigger("job")
	if err != nil {
		t.Fatal(err)
	}
	f.e.wait()
	if rs := f.runs("job"); len(rs) != 1 || rs[0].State != RunSucceeded {
		t.Fatalf("runs = %+v", rs)
	}
	if err := f.e.Cancel(id); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("finished: %v", err)
	}
}

// The core's shared Stop context is 5 s and the tasks module stops last: the
// engine's shutdown wait (which contains each attempt's node Cancel) must fit.
func TestShutdownDefaultsFitCoreStopContext(t *testing.T) {
	if defaultShutdownWait+defaultCancelBudget > 4*time.Second || defaultCancelBudget >= defaultShutdownWait {
		t.Fatalf("shutdown wait %s, cancel budget %s do not fit a 5 s stop", defaultShutdownWait, defaultCancelBudget)
	}
	e := NewEngine(Deps{})
	if e.shutdownWait != defaultShutdownWait || e.cancelBudget != defaultCancelBudget {
		t.Fatalf("engine uses %s / %s", e.shutdownWait, e.cancelBudget)
	}
}
