package controlsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

type result struct {
	cfg     Config
	changed bool
	err     error
}

type fakeSource struct {
	mu      sync.Mutex
	results []result
	etags   []string
	calls   int
}

func (f *fakeSource) Fetch(_ context.Context, etag string) (Config, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.etags = append(f.etags, etag)
	i := f.calls
	f.calls++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	r := f.results[i]
	return r.cfg, r.changed, r.err
}

type fakeReporter struct {
	mu     sync.Mutex
	states []State
	err    error
}

func (f *fakeReporter) Report(_ context.Context, s State) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.states = append(f.states, s)
	return f.err
}

type harness struct {
	r     *runner
	src   *fakeSource
	rep   *fakeReporter
	blob  *memBlob
	app   *Applier
	waits []time.Duration
	logs  []string
	now   time.Time
	ctx   context.Context
}

func newHarness(t *testing.T, cycles int, results ...result) *harness {
	t.Helper()
	h := &harness{src: &fakeSource{results: results}, rep: &fakeReporter{}, blob: &memBlob{}, app: newApplier(t), now: time.Unix(1_000_000, 0)}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	h.r = &runner{
		src: h.src, rep: h.rep, app: h.app, cache: NewCache(h.blob), interval: 30 * time.Second,
		state: func() State { return State{SnapshotLoaded: true, PeerCount: 2} },
		log:   func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) },
		now:   func() time.Time { return h.now },
		rnd:   func() float64 { return 0.5 }, // no jitter: factor 1.0
		wait: func(c context.Context, d time.Duration) bool {
			h.waits = append(h.waits, d)
			h.now = h.now.Add(d)
			if len(h.waits) >= cycles {
				cancel()
			}
			return c.Err() == nil
		},
	}
	h.ctx = ctx
	return h
}

func (h *harness) run() { h.r.run(h.ctx) }

func cfgV(v string) Config {
	c := goodConfig()
	c.Version = v
	return c
}

func (h *harness) domain() string {
	if d := h.app.Domain.Load(); d != nil {
		return *d
	}
	return ""
}

func TestRunAppliesCacheBeforeFirstFetch(t *testing.T) {
	h := newHarness(t, 1, result{err: errors.New("down")})
	if err := NewCache(h.blob).Save(cfgV("cached")); err != nil {
		t.Fatal(err)
	}
	h.blob.saves = 0
	var domainAtFetch string
	h.src.results = []result{{err: errors.New("down")}}
	orig := h.r.src
	h.r.src = srcFunc(func(ctx context.Context, etag string) (Config, bool, error) {
		domainAtFetch = h.domain() // what is served when the first fetch starts
		return orig.Fetch(ctx, etag)
	})
	h.run()
	if domainAtFetch != "example.com" {
		t.Fatalf("the cache was not applied before the first fetch (domain %q)", domainAtFetch)
	}
	if h.src.etags[0] != "cached" {
		t.Fatalf("the first fetch sent etag %q, want the cached version", h.src.etags[0])
	}
}

type srcFunc func(ctx context.Context, etag string) (Config, bool, error)

func (f srcFunc) Fetch(ctx context.Context, etag string) (Config, bool, error) { return f(ctx, etag) }

func TestRunAppliesNewVersionAndCachesItOnce(t *testing.T) {
	h := newHarness(t, 4, result{cfg: cfgV("v1"), changed: true}, result{changed: false})
	h.run()
	if h.domain() != "example.com" {
		t.Fatal("configuration not applied")
	}
	if h.blob.saves != 1 {
		t.Fatalf("saves = %d, want 1", h.blob.saves)
	}
	if got := h.src.etags; got[0] != "" || got[1] != "v1" || got[3] != "v1" {
		t.Fatalf("etags = %v", got)
	}
}

func TestRunKeepsLastConfigWhenSourceFails(t *testing.T) {
	failures := []error{errors.New("timeout"), errors.New("garbage"), errors.New("500")}
	res := []result{{cfg: cfgV("v1"), changed: true}}
	for i := 0; i < 8; i++ {
		res = append(res, result{err: failures[i%3]})
	}
	h := newHarness(t, 9, res...)
	h.run()
	if h.domain() != "example.com" || len(h.app.Table.All()) != 1 || !h.app.NS.Allowed("pdl", 9005) {
		t.Fatal("the last good configuration was dropped")
	}
	// Backoff doubles per failure up to the cap and never shrinks while failing.
	want := []time.Duration{30 * time.Second, 60 * time.Second, 120 * time.Second, 240 * time.Second, 300 * time.Second, 300 * time.Second, 300 * time.Second, 300 * time.Second}
	for i, w := range want {
		if h.waits[i] != w {
			t.Fatalf("wait %d = %v, want %v (all %v)", i, h.waits[i], w, h.waits)
		}
	}
	last := h.rep.states[len(h.rep.states)-1]
	if len(last.Errors) == 0 || last.AppliedVersion != "v1" {
		t.Fatalf("State.Errors / AppliedVersion not filled: %+v", last)
	}
}

func TestRunBackoffResetsAfterSuccess(t *testing.T) {
	h := newHarness(t, 4, result{err: errors.New("down")}, result{err: errors.New("down")}, result{cfg: cfgV("v1"), changed: true}, result{changed: false})
	h.run()
	want := []time.Duration{60 * time.Second, 120 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, w := range want {
		if h.waits[i] != w {
			t.Fatalf("waits = %v, want %v", h.waits, want)
		}
	}
	if last := h.rep.states[len(h.rep.states)-1]; len(last.Errors) != 0 {
		t.Fatalf("errors not cleared after recovery: %v", last.Errors)
	}
}

func TestRunJitterStaysWithinTwentyPercent(t *testing.T) {
	h := newHarness(t, 1, result{changed: false})
	for _, f := range []float64{0, 0.5, 0.999999} {
		h.r.rnd = func() float64 { return f }
		d := h.r.delay(0)
		if d < 24*time.Second || d > 36*time.Second {
			t.Errorf("rnd %v: delay %v outside 30s +/-20%%", f, d)
		}
	}
}

func TestRunRejectedConfigKeepsPreviousAndIsReported(t *testing.T) {
	bad := cfgV("v2")
	bad.Namespaces["pdl"] = []config.PortRange{{Lo: 9100, Hi: 9100}} // the ingress's own API port
	h := newHarness(t, 3, result{cfg: cfgV("v1"), changed: true}, result{cfg: bad, changed: true}, result{cfg: bad, changed: true})
	h.run()
	if !h.app.NS.Allowed("pdl", 9005) || h.app.NS.Allowed("pdl", 9100) {
		t.Fatal("a rejected configuration changed the namespaces")
	}
	if h.blob.saves != 1 {
		t.Fatalf("a rejected configuration was cached (saves=%d)", h.blob.saves)
	}
	last := h.rep.states[len(h.rep.states)-1]
	if last.AppliedVersion != "v1" || len(last.Errors) == 0 || !strings.Contains(last.Errors[0], "own ports") {
		t.Fatalf("report: %+v", last)
	}
}

func TestRunStartsEmptyWhenCacheCorruptAndSourceDown(t *testing.T) {
	h := newHarness(t, 3, result{err: errors.New("down")})
	h.blob.b, h.blob.has = []byte("garbage"), true
	h.run()
	if h.domain() != "" || len(h.app.Table.All()) != 0 {
		t.Fatal("something was served from a corrupt cache")
	}
	if string(h.blob.b) != "garbage" || h.blob.saves != 0 {
		t.Fatal("the corrupt cache was overwritten before a good configuration arrived")
	}
	if len(h.logs) == 0 {
		t.Fatal("nothing logged")
	}
}

func TestRunCorruptCacheIsReplacedByFirstGoodConfig(t *testing.T) {
	h := newHarness(t, 2, result{cfg: cfgV("v1"), changed: true}, result{changed: false})
	h.blob.b, h.blob.has = []byte("garbage"), true
	h.run()
	if _, ok, err := NewCache(h.blob).Load(); err != nil || !ok {
		t.Fatalf("cache not repaired: %v %v", ok, err)
	}
}

func TestRunUnchangedWritesNothingToTheStateStore(t *testing.T) {
	h := newHarness(t, 50, result{cfg: cfgV("v1"), changed: true}, result{changed: false})
	h.run()
	if h.blob.saves != 1 {
		t.Fatalf("saves = %d across 50 cycles, want 1", h.blob.saves)
	}
	// A source that insists on re-sending the same version also writes nothing.
	h2 := newHarness(t, 20, result{cfg: cfgV("v1"), changed: true})
	h2.run()
	if h2.blob.saves != 1 {
		t.Fatalf("repeated identical version: saves = %d", h2.blob.saves)
	}
}

func TestRunReportsAfterEveryCycleEvenWhenUnchanged(t *testing.T) {
	h := newHarness(t, 5, result{cfg: cfgV("v1"), changed: true}, result{changed: false})
	h.run()
	if len(h.rep.states) != 5 {
		t.Fatalf("%d reports for 5 cycles", len(h.rep.states))
	}
	s := h.rep.states[4]
	if !s.SnapshotLoaded || s.PeerCount != 2 || s.AppliedVersion != "v1" || s.SentAt.IsZero() {
		t.Fatalf("report: %+v", s)
	}
}

func TestRunReportFailureDoesNotStopSync(t *testing.T) {
	h := newHarness(t, 4, result{cfg: cfgV("v1"), changed: true}, result{cfg: cfgV("v2"), changed: true}, result{changed: false})
	h.rep.err = errors.New("node-control down")
	h.run()
	if len(h.rep.states) != 4 || h.domain() != "example.com" {
		t.Fatalf("sync stopped after a report failure (%d reports)", len(h.rep.states))
	}
	if h.src.calls != 4 {
		t.Fatalf("fetches = %d", h.src.calls)
	}
	n := 0
	for _, l := range h.logs {
		if strings.Contains(l, "node-control down") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the same report failure was logged %d times, want once", n)
	}
}

func TestRunWithoutReporter(t *testing.T) {
	h := newHarness(t, 2, result{cfg: cfgV("v1"), changed: true})
	h.r.rep = nil
	h.run()
	if h.domain() != "example.com" {
		t.Fatal("file-source style run did not apply")
	}
}

func TestRunReportsConfigAge(t *testing.T) {
	ok := result{cfg: cfgV("v1"), changed: true}
	h := newHarness(t, 4, ok, result{err: errors.New("down")})
	h.run()
	ages := []time.Duration{}
	for _, s := range h.rep.states {
		ages = append(ages, s.ConfigAge)
	}
	// Cycle 1 succeeds (age 0); every later cycle fails, so the age grows by the waits between cycles.
	if ages[0] != 0 || ages[1] <= 0 || ages[2] <= ages[1] || ages[3] <= ages[2] {
		t.Fatalf("ages = %v", ages)
	}
}

func TestRunErrorsAreBoundedAndDeduplicated(t *testing.T) {
	var res []result
	for i := 0; i < 30; i++ {
		res = append(res, result{err: fmt.Errorf("failure %d", i)})
	}
	h := newHarness(t, 30, res...)
	h.run()
	last := h.rep.states[len(h.rep.states)-1]
	if len(last.Errors) != maxErrors {
		t.Fatalf("errors = %d, want the last %d", len(last.Errors), maxErrors)
	}
	if last.Errors[maxErrors-1] != "failure 29" {
		t.Fatalf("errors = %v", last.Errors)
	}
	h2 := newHarness(t, 10, result{err: errors.New("same")})
	h2.run()
	if got := h2.rep.states[9].Errors; len(got) != 1 {
		t.Fatalf("identical errors repeated: %v", got)
	}
}

func TestRunStopsWhenContextIsDone(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	h := newHarness(t, 1, result{changed: false})
	go func() {
		Run(ctx, h.src, nil, h.app, NewCache(h.blob), nil, time.Hour, nil)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after the context was cancelled")
	}
}

func TestStateNeverContainsAuthKey(t *testing.T) {
	s := State{
		AppliedVersion: "v1", SentAt: time.Unix(1, 0), SnapshotLoaded: true, PeerCount: 1, ConfigAge: time.Minute,
		Routes: []RouteState{{Route: routes.Route{Key: routes.Key{NS: "pdl", Service: "a"}, Target: "100.64.0.5", Port: 80, Scheme: "http", NodeID: "n1"}, OwnerName: "box", TargetOnline: true}},
		Errors: []string{"x"},
	}
	b, err := encodeState(s)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"authkey", "tskey", "password", "secret", "redis", "state_key"} {
		if strings.Contains(strings.ToLower(string(b)), key) {
			t.Errorf("report mentions %q: %s", key, b)
		}
	}
	for _, want := range []string{`"applied_version":"v1"`, `"snapshot_loaded":true`, `"peer_count":1`, `"config_age_seconds":60`, `"owner_node_id":"n1"`, `"owner_name":"box"`, `"target_online":true`, `"last_reconcile":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("report lacks %s: %s", want, b)
		}
	}
}
