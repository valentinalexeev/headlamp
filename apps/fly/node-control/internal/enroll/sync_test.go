package enroll

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type put struct {
	creds        []Credential
	confirmEmpty bool
}

type fakeAgent struct {
	mu     sync.Mutex
	pushes []put
	fail   error
	onPut  func()
	stats  map[string]any
}

func (a *fakeAgent) PutCredentials(ctx context.Context, creds []Credential, confirmEmpty bool) error {
	if a.onPut != nil {
		a.onPut()
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	a.pushes = append(a.pushes, put{append([]Credential{}, creds...), confirmEmpty})
	return nil
}

func (a *fakeAgent) Stats(ctx context.Context) map[string]any { return map[string]any{} }

func (a *fakeAgent) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pushes)
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func fixedState(now *int64) *SyncState {
	return NewSyncState(func() time.Time { return time.Unix(atomic.LoadInt64(now), 0) })
}

func TestSyncStateInitial(t *testing.T) {
	var now int64 = 100
	got := fixedState(&now).Snapshot()
	if got.Pending || got.State != "unknown" || got.Since != nil {
		t.Fatalf("%+v", got)
	}
}

func TestSyncStateSinceSemantics(t *testing.T) {
	var now int64 = 1000
	s := fixedState(&now)
	s.Failed("unreachable")
	atomic.StoreInt64(&now, 1300)
	s.Failed("rejected")
	got := s.Snapshot()
	if !got.Pending || got.State != "rejected" || got.Since == nil || *got.Since != 1000 {
		t.Fatalf("%+v", got)
	}
	s.OK("ok")
	got = s.Snapshot()
	if got.Pending || got.State != "ok" || got.Since != nil {
		t.Fatalf("%+v", got)
	}
	atomic.StoreInt64(&now, 2000)
	s.Failed("error")
	if got = s.Snapshot(); *got.Since != 2000 {
		t.Fatalf("%+v", got)
	}
}

func TestEmptyStoreNeverPushed(t *testing.T) {
	e := newEnv(t)
	agent := &fakeAgent{}
	var now int64 = 5
	state := fixedState(&now)
	if !Sync(context.Background(), e.store, agent, state) {
		t.Fatal("want true")
	}
	if agent.count() != 0 {
		t.Fatal("pushed an empty store")
	}
	if got := state.Snapshot(); got.Pending || got.State != "empty" || got.Since != nil {
		t.Fatalf("%+v", got)
	}
}

func TestSyncPushesCredentials(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	agent := &fakeAgent{}
	state := NewSyncState(nil)
	if !Sync(context.Background(), e.store, agent, state) {
		t.Fatal("want true")
	}
	if agent.count() != 1 || len(agent.pushes[0].creds) != 3 || agent.pushes[0].confirmEmpty {
		t.Fatalf("%+v", agent.pushes)
	}
	if got := state.Snapshot(); got.State != "ok" || got.Pending {
		t.Fatalf("%+v", got)
	}
}

func TestAllRevokedPushesConfirmedEmpty(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	if err := e.store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	agent := &fakeAgent{}
	if !Sync(context.Background(), e.store, agent, NewSyncState(nil)) {
		t.Fatal("want true")
	}
	if agent.count() != 1 || len(agent.pushes[0].creds) != 0 || !agent.pushes[0].confirmEmpty {
		t.Fatalf("%+v", agent.pushes)
	}
}

func TestEachFailedSyncIsClassifiedAndLogged(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		outcome string
		status  string
	}{
		{"unreachable", &AgentUnavailable{Outcome: OutcomeUnreachable}, "unreachable", ""},
		{"unauthorized", &AgentUnavailable{Outcome: OutcomeUnauthorized, Status: 401}, "unauthorized", "401"},
		{"rejected", &AgentUnavailable{Outcome: OutcomeRejected, Status: 503}, "rejected", "503"},
		{"conflict", &AgentUnavailable{Outcome: OutcomeConflict, Status: 409}, "conflict", "409"},
		{"other", errors.New("boom SECRETPAYLOAD"), "error", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t)
			e.make("a", 0)
			creds, _ := e.store.ActiveCredentials()
			buf := captureLogs(t)
			var now int64 = 77
			state := fixedState(&now)
			if Sync(context.Background(), e.store, &fakeAgent{fail: c.err}, state) {
				t.Fatal("want false")
			}
			out := buf.String()
			if c.outcome != "error" && !strings.Contains(out, "enrolment sync failed: "+c.outcome+" (AgentUnavailable") {
				t.Fatalf("log %q", out)
			}
			if c.status != "" && !strings.Contains(out, c.status) {
				t.Fatalf("status missing in %q", out)
			}
			if strings.Contains(out, "SECRETPAYLOAD") {
				t.Fatalf("payload leaked: %q", out)
			}
			for _, cr := range creds {
				if strings.Contains(out, cr.UUID) {
					t.Fatalf("uuid leaked: %q", out)
				}
			}
			got := state.Snapshot()
			if !got.Pending || got.State != c.outcome || got.Since == nil || *got.Since != 77 {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestUnreadableStoreNeverPushes(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	e.store.Close()
	agent := &fakeAgent{}
	state := NewSyncState(nil)
	buf := captureLogs(t)
	if Sync(context.Background(), e.store, agent, state) {
		t.Fatal("want false")
	}
	if agent.count() != 0 {
		t.Fatal("pushed after read error")
	}
	if got := state.Snapshot(); !got.Pending || got.State != "error" {
		t.Fatalf("%+v", got)
	}
	if buf.Len() == 0 {
		t.Fatal("expected a warning")
	}
}

func TestSinceKeptWhileFailingClearedOnSuccess(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	var now int64 = 1000
	state := fixedState(&now)
	agent := &fakeAgent{fail: &AgentUnavailable{Outcome: OutcomeUnreachable}}
	captureLogs(t)
	Sync(context.Background(), e.store, agent, state)
	atomic.StoreInt64(&now, 1300)
	agent.fail = &AgentUnavailable{Outcome: OutcomeRejected, Status: 503}
	Sync(context.Background(), e.store, agent, state)
	if got := state.Snapshot(); got.State != "rejected" || *got.Since != 1000 {
		t.Fatalf("%+v", got)
	}
	agent.fail = nil
	if !Sync(context.Background(), e.store, agent, state) {
		t.Fatal("want true")
	}
	if got := state.Snapshot(); got.Pending || got.State != "ok" || got.Since != nil {
		t.Fatalf("%+v", got)
	}
}

func TestConcurrentSyncsEndWithNewestState(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	started, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	agent := &fakeAgent{}
	agent.onPut = func() {
		first.Do(func() { close(started); <-release })
	}
	state := NewSyncState(nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); Sync(context.Background(), e.store, agent, state) }()
	<-started
	if err := e.store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	go func() { defer wg.Done(); Sync(context.Background(), e.store, agent, state) }()
	time.Sleep(200 * time.Millisecond) // let the second run ahead if nothing serialises it
	close(release)
	wg.Wait()
	if agent.count() != 2 || len(agent.pushes[0].creds) != 3 || len(agent.pushes[1].creds) != 0 {
		t.Fatalf("pushes %+v", agent.pushes)
	}
}

func TestLogsNeverContainToken(t *testing.T) {
	const token = "TOPSECRETTOKEN-xyz"
	e := newEnv(t)
	e.make("a", 0)
	buf := captureLogs(t)
	// a real agent client against an unreachable and a rejecting server
	srv := newStatusServer(503)
	agent := NewAgent(srv.URL, token, srv.Client())
	Sync(context.Background(), e.store, agent, NewSyncState(nil))
	srv.Close()
	Sync(context.Background(), e.store, agent, NewSyncState(nil))
	if buf.Len() == 0 {
		t.Fatal("no logs captured")
	}
	if strings.Contains(buf.String(), token) {
		t.Fatalf("token leaked: %s", buf.String())
	}
}

func TestReconcileRecoversFromPanic(t *testing.T) {
	buf := captureLogs(t)
	restore := setReconcileForTest(5*time.Millisecond, 5*time.Millisecond)
	defer restore()
	var calls atomic.Int32
	oldSync := syncFn
	syncFn = func(ctx context.Context, s *Store, a Agent, st *SyncState) bool {
		if calls.Add(1) == 1 {
			panic(fmt.Errorf("db locked SECRETPAYLOAD"))
		}
		st.OK("ok")
		return true
	}
	defer func() { syncFn = oldSync }()
	state := NewSyncState(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ReconcileLoop(ctx, nil, nil, state); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
	if calls.Load() < 3 {
		t.Fatalf("loop did not continue: %d calls", calls.Load())
	}
	out := buf.String()
	if !strings.Contains(out, "enrolment reconcile failed") || !strings.Contains(out, "*errors.errorString") {
		t.Fatalf("log %q", out)
	}
	if strings.Contains(out, "SECRETPAYLOAD") {
		t.Fatalf("payload leaked: %q", out)
	}
}

func TestReconcilePanicMarksError(t *testing.T) {
	captureLogs(t)
	restore := setReconcileForTest(5*time.Millisecond, time.Hour)
	defer restore()
	oldSync := syncFn
	syncFn = func(ctx context.Context, s *Store, a Agent, st *SyncState) bool { panic("x") }
	defer func() { syncFn = oldSync }()
	state := NewSyncState(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ReconcileLoop(ctx, nil, nil, state); close(done) }()
	defer func() { cancel(); <-done }() // stop the loop before the package vars are restored
	deadline := time.Now().Add(2 * time.Second)
	for !state.Snapshot().Pending && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := state.Snapshot(); !got.Pending || got.State != "error" {
		t.Fatalf("%+v", got)
	}
}

func TestFirstSyncBeforeFullPeriod(t *testing.T) {
	restore := setReconcileForTest(5*time.Millisecond, time.Hour)
	defer restore()
	var calls atomic.Int32
	oldSync := syncFn
	syncFn = func(ctx context.Context, s *Store, a Agent, st *SyncState) bool { calls.Add(1); return true }
	defer func() { syncFn = oldSync }()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { ReconcileLoop(ctx, nil, nil, NewSyncState(nil)); close(done) }()
	time.Sleep(200 * time.Millisecond)
	cancel()
	<-done
	if calls.Load() != 1 {
		t.Fatalf("calls %d", calls.Load())
	}
}

func TestDefaultReconcileTimings(t *testing.T) {
	if firstReconcileDelay != 20*time.Second || reconcilePeriod != 5*time.Minute {
		t.Fatalf("%v %v", firstReconcileDelay, reconcilePeriod)
	}
}

func TestConcurrentCreateDevice(t *testing.T) {
	e := newEnv(t)
	const n = 40
	ids := make(chan int64, n)
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			d, _, err := e.store.CreateDevice(fmt.Sprintf("dev-%d", i), "alice", "sing-box", e.clock.Now().Unix()+3600)
			if err != nil {
				errs <- err
				return
			}
			ids <- d.ID
		}(i)
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		t.Errorf("CreateDevice: %v", err)
	}
	seen := map[int64]bool{}
	for id := range ids {
		if seen[id] {
			t.Errorf("duplicate id %d", id)
		}
		seen[id] = true
	}
	if len(seen) != n {
		t.Fatalf("got %d ids", len(seen))
	}
	devs, err := e.store.ListDevices()
	if err != nil || len(devs) != n {
		t.Fatalf("listed %d, %v", len(devs), err)
	}
}
