package policy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/node-control/internal/headscale"
)

const testRevert = 2 * time.Minute

type fakeAPI struct {
	mu       sync.Mutex
	text     string
	version  int
	getErr   error
	setErr   error
	checkErr error
	gets     int
	sets     []string
	checks   int
}

func newFakeAPI(text string) *fakeAPI { return &fakeAPI{text: text, version: 1} }

func (f *fakeAPI) GetPolicy(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return "", "", f.getErr
	}
	return f.text, fmt.Sprintf("v%d", f.version), nil
}

func (f *fakeAPI) SetPolicy(_ context.Context, text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, text)
	if f.setErr != nil {
		return f.setErr
	}
	f.text = text
	f.version++
	return nil
}

func (f *fakeAPI) CheckPolicy(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks++
	return f.checkErr
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.gets + len(f.sets) + f.checks
}

func (f *fakeAPI) current() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text
}

func (f *fakeAPI) setCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sets)
}

func (f *fakeAPI) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setErr = err
}

func newTestService(t *testing.T, api API) (*Service, *fakeClock, string) {
	t.Helper()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "policy.db")
	st := openTestStore(t, path, clk)
	return NewService(api, st, testRevert, 1024, clk.Now), clk, path
}

func applyOK(t *testing.T, svc *Service, text string) int64 {
	t.Helper()
	_, upd, err := svc.Current(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := svc.Apply(context.Background(), "alice", text, upd, "edit")
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return id
}

func TestApplySetsPendingAndCallsSetOnce(t *testing.T) {
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	_, upd, _ := svc.Current(context.Background())
	id, deadline, err := svc.Apply(context.Background(), "alice", "new", upd, "edit")
	if err != nil {
		t.Fatal(err)
	}
	if !deadline.Equal(clk.Now().Add(testRevert)) {
		t.Fatalf("deadline = %v", deadline)
	}
	if api.setCount() != 1 || api.current() != "new" {
		t.Fatalf("sets = %d, text = %q", api.setCount(), api.current())
	}
	p, ok, _ := svc.store.Pending()
	if !ok || p.ID != id || p.Before != "old" || p.Actor != "alice" {
		t.Fatalf("pending = %+v %v", p, ok)
	}
	if svc.Outcome(id) != OutcomePending {
		t.Fatalf("outcome = %q", svc.Outcome(id))
	}
}

func TestApplyStaleBaseIsConflict(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	_, _, err := svc.Apply(context.Background(), "alice", "new", "v0", "edit")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v", err)
	}
	if api.setCount() != 0 {
		t.Fatal("SetPolicy called")
	}
	if h, _ := svc.store.History(5); len(h) != 0 {
		t.Fatalf("history = %v", h)
	}
}

func TestApplyWhilePendingIsBusy(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	applyOK(t, svc, "new")
	before := api.calls()
	_, _, err := svc.Apply(context.Background(), "bob", "newer", "v2", "edit")
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v", err)
	}
	if api.calls() != before {
		t.Fatal("API called while busy")
	}
}

func TestApplyOversizeBeforeAnyAPICall(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	_, _, err := svc.Apply(context.Background(), "alice", strings.Repeat("x", 1025), "v1", "edit")
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	if api.calls() != 0 {
		t.Fatal("API called")
	}
}

func TestApplyRejectedByHeadscaleStoresNothing(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	rej := &headscale.APIError{Status: 400, Message: "bad policy"}
	api.fail(rej)
	_, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if err != error(rej) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := svc.store.Pending(); ok {
		t.Fatal("pending left behind")
	}
	if h, _ := svc.store.History(5); len(h) != 0 {
		t.Fatalf("history = %v", h)
	}
	api.fail(nil)
	applyOK(t, svc, "new") // not busy afterwards
}

func TestChangeAppliesTransformOfCurrentText(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	id, _, err := svc.Change(context.Background(), "alice", func(cur string) (string, error) {
		return cur + "+more", nil
	}, "tweak")
	if err != nil {
		t.Fatal(err)
	}
	if api.current() != "old+more" || svc.Outcome(id) != OutcomePending {
		t.Fatalf("text = %q", api.current())
	}
}

func TestChangeTransformErrorAppliesNothing(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	boom := errors.New("boom")
	_, _, err := svc.Change(context.Background(), "alice", func(string) (string, error) { return "", boom }, "r")
	if err != boom || api.setCount() != 0 {
		t.Fatalf("err = %v, sets = %d", err, api.setCount())
	}
	if _, ok, _ := svc.store.Pending(); ok {
		t.Fatal("pending")
	}
}

func TestChangeNoChange(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	_, _, err := svc.Change(context.Background(), "alice", func(c string) (string, error) { return c, nil }, "r")
	if !errors.Is(err, ErrNoChange) || api.setCount() != 0 {
		t.Fatalf("err = %v", err)
	}
}

func TestConfirmClearsPending(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	if err := svc.Confirm(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	if svc.Outcome(id) != OutcomeConfirmed {
		t.Fatalf("outcome = %q", svc.Outcome(id))
	}
	if _, ok, _ := svc.store.Pending(); ok {
		t.Fatal("pending")
	}
}

func TestConfirmWithNothingPending(t *testing.T) {
	svc, _, _ := newTestService(t, newFakeAPI("old"))
	if err := svc.Confirm(context.Background(), "alice"); !errors.Is(err, ErrNothingPending) {
		t.Fatalf("err = %v", err)
	}
}

func TestWatcherRevertsAfterDeadline(t *testing.T) {
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	clk.Advance(testRevert)
	svc.checkRevert(context.Background())
	if api.current() != "old" || svc.Outcome(id) != OutcomeReverted {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc.Outcome(id))
	}
}

func TestWatcherDoesNothingBeforeDeadline(t *testing.T) {
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	clk.Advance(testRevert - time.Second)
	svc.checkRevert(context.Background())
	if api.current() != "new" || svc.Outcome(id) != OutcomePending {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc.Outcome(id))
	}
}

func TestWatcherRevertsAtStartupWhenDeadlinePassed(t *testing.T) {
	api := newFakeAPI("old")
	svc, clk, path := newTestService(t, api)
	id := applyOK(t, svc, "new")
	svc.store.Close()
	clk.Advance(time.Hour)
	st2 := openTestStore(t, path, clk)
	svc2 := NewService(api, st2, testRevert, 1024, clk.Now)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // the synchronous first check must still run
	svc2.RunWatcher(ctx, time.Hour)
	if api.current() != "old" || svc2.Outcome(id) != OutcomeReverted {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc2.Outcome(id))
	}
}

func TestWatcherTicksUntilCancelled(t *testing.T) {
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.RunWatcher(ctx, 5*time.Millisecond); close(done) }()
	clk.Advance(testRevert)
	deadline := time.Now().Add(3 * time.Second)
	for svc.Outcome(id) != OutcomeReverted && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	<-done
	if svc.Outcome(id) != OutcomeReverted {
		t.Fatal("not reverted by the ticker")
	}
}

func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func TestWatcherFailedRevertIsRetriedAndLogged(t *testing.T) {
	logs := captureLogs(t)
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	api.fail(errors.New("unreachable"))
	clk.Advance(testRevert)
	svc.checkRevert(context.Background())
	svc.checkRevert(context.Background())
	p, ok, _ := svc.store.Pending()
	if !ok || p.ID != id || p.Attempts != 2 {
		t.Fatalf("pending = %+v %v", p, ok)
	}
	out := logs.String()
	if !strings.Contains(out, "policy revert failed") || !strings.Contains(out, "level=ERROR") ||
		!strings.Contains(out, fmt.Sprintf("id=%d", id)) || !strings.Contains(out, "attempts=2") {
		t.Fatalf("logs = %s", out)
	}
	api.fail(nil)
	svc.checkRevert(context.Background())
	if api.current() != "old" || svc.Outcome(id) != OutcomeReverted {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc.Outcome(id))
	}
}

func TestRevertToReappliesBefore(t *testing.T) {
	api := newFakeAPI("v-one")
	svc, _, _ := newTestService(t, api)
	first := applyOK(t, svc, "v-two")
	if err := svc.Confirm(context.Background(), "alice"); err != nil {
		t.Fatal(err)
	}
	id, _, err := svc.RevertTo(context.Background(), "alice", first)
	if err != nil {
		t.Fatal(err)
	}
	if id == first || api.current() != "v-one" || svc.Outcome(id) != OutcomePending {
		t.Fatalf("id=%d text=%q", id, api.current())
	}
	h, _ := svc.store.History(1)
	if h[0].Reason != fmt.Sprintf("revert of #%d", first) {
		t.Fatalf("reason = %q", h[0].Reason)
	}
	if _, _, err := svc.RevertTo(context.Background(), "alice", 999); err == nil {
		t.Fatal("expected error for unknown history id")
	}
}

func TestPolicyTextNeverLogged(t *testing.T) {
	logs := captureLogs(t)
	const marker = "SECRET-MARKER-9f3a"
	old := `{"note":"` + marker + `-old"}`
	api := newFakeAPI(old)
	svc, clk, _ := newTestService(t, api)
	ctx := context.Background()
	id := applyOK(t, svc, `{"note":"`+marker+`-new"}`)
	if err := svc.Confirm(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.RevertTo(ctx, "alice", id); err != nil {
		t.Fatal(err)
	}
	api.fail(&headscale.APIError{Status: 400, Message: "rejected " + marker})
	clk.Advance(testRevert)
	svc.checkRevert(ctx)
	api.fail(nil)
	svc.checkRevert(ctx)
	api.fail(&headscale.APIError{Status: 400, Message: "rejected " + marker})
	_, _, _ = svc.Apply(ctx, "alice", `{"note":"`+marker+`-x"}`, "v0", "edit")
	if logs.Len() == 0 {
		t.Fatal("nothing was logged at all")
	}
	if strings.Contains(logs.String(), marker) || strings.Contains(logs.String(), `"note"`) {
		t.Fatalf("policy text leaked into logs:\n%s", logs.String())
	}
}

func TestConcurrentApplyOnlyOneSucceeds(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	var wins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := func() (int64, error) {
				id, _, err := svc.Apply(context.Background(), "alice", fmt.Sprintf("new-%d", i), "v1", "edit")
				return id, err
			}()
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrBusy), errors.Is(err, ErrConflict):
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if wins.Load() != 1 || api.setCount() != 1 {
		t.Fatalf("wins=%d sets=%d", wins.Load(), api.setCount())
	}
}
