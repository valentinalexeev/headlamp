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
	applyErr error // SetPolicy stores the text, then fails
	breakGet bool  // a failing SetPolicy also breaks later reads
	checkErr error
	gets     int
	sets     []string
	checks   int
	other    string        // a failing SetPolicy leaves this text live instead, if set
	noVer    bool          // GetPolicy reports an empty version
	entered  chan struct{} // signalled when SetPolicy starts, if set
	block    chan struct{} // SetPolicy waits for it (or its ctx), if set
}

func newFakeAPI(text string) *fakeAPI { return &fakeAPI{text: text, version: 1} }

func (f *fakeAPI) GetPolicy(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.getErr != nil {
		return "", "", f.getErr
	}
	if f.noVer {
		return f.text, "", nil
	}
	return f.text, fmt.Sprintf("v%d", f.version), nil
}

func (f *fakeAPI) SetPolicy(ctx context.Context, text string) error {
	if f.entered != nil {
		f.entered <- struct{}{}
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sets = append(f.sets, text)
	if f.applyErr != nil {
		f.text = text
		f.version++
		return f.applyErr
	}
	if f.setErr != nil {
		if f.other != "" {
			f.text = f.other
			f.version++
		}
		if f.breakGet {
			f.getErr = errors.New("read failed")
		}
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
	if err := confirmCur(svc); err != nil {
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
	if err := confirmCur(svc); !errors.Is(err, ErrNothingPending) {
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
	if err := confirmCur(svc); err != nil {
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
	if err := confirmCur(svc); err != nil {
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

func TestApplyAmbiguousErrorThenPolicyAppliedKeepsPending(t *testing.T) {
	api := newFakeAPI("old")
	api.applyErr = errors.New("timeout")
	svc, _, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	if svc.Outcome(id) != OutcomePending {
		t.Fatalf("outcome = %q", svc.Outcome(id))
	}
	if err := confirmCur(svc); err != nil {
		t.Fatal(err)
	}
}

func confirmCur(svc *Service) error {
	p, _, _ := svc.store.Pending()
	return svc.Confirm(context.Background(), "alice", p.ID)
}

func TestApplyAmbiguousErrorNotYetVisibleKeepsPendingAndReturnsErrUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(errors.New("connection reset"))
	svc, _, _ := newTestService(t, api)
	id, dl, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if !errors.Is(err, ErrUncertain) || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("err = %v", err)
	}
	if p, ok, _ := svc.store.Pending(); !ok || p.ID != id || id == 0 || dl.IsZero() {
		t.Fatalf("pending = %+v %v id=%d", p, ok, id)
	}
}

func TestApplyClientCancelStillWritesAndKeepsPending(t *testing.T) {
	api := newFakeAPI("old")
	api.entered, api.block = make(chan struct{}), make(chan struct{})
	svc, _, _ := newTestService(t, api)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, _, err := svc.Apply(ctx, "alice", "new", "v1", "edit")
		done <- err
	}()
	<-api.entered
	cancel()
	close(api.block)
	if err := <-done; err != nil {
		t.Fatalf("err = %v", err)
	}
	if api.current() != "new" {
		t.Fatalf("text = %q", api.current())
	}
	if _, ok, _ := svc.store.Pending(); !ok {
		t.Fatal("change not pending")
	}
}

func TestWatcherRevertOfNeverAppliedChangeIsHarmless(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(errors.New("reset"))
	svc, clk, _ := newTestService(t, api)
	id, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
	api.fail(nil)
	clk.Advance(testRevert)
	svc.checkRevert(context.Background())
	if api.current() != "old" || svc.Outcome(id) != OutcomeReverted {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc.Outcome(id))
	}
}

func TestApplyEmptyUpdatedAtIsRefused(t *testing.T) {
	api := newFakeAPI("old")
	api.noVer = true
	svc, _, _ := newTestService(t, api)
	if _, _, err := svc.Apply(context.Background(), "alice", "new", "", "edit"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("apply err = %v", err)
	}
	tr := func(string) (string, error) { return "x", nil }
	if _, _, err := svc.Change(context.Background(), "alice", tr, "r"); !errors.Is(err, ErrNoVersion) {
		t.Fatalf("change err = %v", err)
	}
	if api.setCount() != 0 {
		t.Fatal("a write was sent")
	}
}

func TestConfirmWrongIDIs409(t *testing.T) {
	svc, _, _ := newTestService(t, newFakeAPI("old"))
	id := applyOK(t, svc, "new")
	if err := svc.Confirm(context.Background(), "alice", id+1); !errors.Is(err, ErrNotPending) {
		t.Fatalf("err = %v", err)
	}
	if svc.Outcome(id) != OutcomePending {
		t.Fatal("confirmed anyway")
	}
}

func TestConfirmWhenLivePolicyDiffersIs409(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	api.mu.Lock()
	api.text = "somebody else's"
	api.mu.Unlock()
	if err := svc.Confirm(context.Background(), "alice", id); !errors.Is(err, ErrNotLive) {
		t.Fatalf("err = %v", err)
	}
	api.mu.Lock()
	api.getErr = errors.New("down")
	api.mu.Unlock()
	if err := svc.Confirm(context.Background(), "alice", id); err == nil || errors.Is(err, ErrNotLive) {
		t.Fatalf("err = %v", err)
	}
	if svc.Outcome(id) != OutcomePending {
		t.Fatal("confirmed anyway")
	}
}

func TestApplyAmbiguousErrorReReadFailsKeepsPendingAndReturnsErrUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(errors.New("timeout"))
	api.breakGet = true
	svc, clk, _ := newTestService(t, api)
	id, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if !errors.Is(err, ErrUncertain) || !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("err = %v", err)
	}
	if p, ok, _ := svc.store.Pending(); !ok || p.ID != id {
		t.Fatalf("pending = %+v %v", p, ok)
	}
	api.fail(nil)
	api.mu.Lock()
	api.getErr = nil
	api.mu.Unlock()
	clk.Advance(testRevert)
	svc.checkRevert(context.Background())
	if api.current() != "old" || svc.Outcome(id) != OutcomeReverted {
		t.Fatalf("text = %q, outcome = %q", api.current(), svc.Outcome(id))
	}
}

func TestApplyDefiniteRejectionStillAborts(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(&headscale.APIError{Status: 422, Message: "no"})
	svc, _, _ := newTestService(t, api)
	_, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	var ae *headscale.APIError
	if !errors.As(err, &ae) || errors.Is(err, ErrUncertain) || api.gets != 1 {
		t.Fatalf("err = %v, gets = %d", err, api.gets)
	}
	if _, ok, _ := svc.store.Pending(); ok {
		t.Fatal("pending")
	}
}

func TestAmbiguousErrorThroughChangeAndRevertTo(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	if err := confirmCur(svc); err != nil {
		t.Fatal(err)
	}
	api.applyErr = errors.New("timeout")
	rid, _, err := svc.RevertTo(context.Background(), "alice", id)
	if err != nil || svc.Outcome(rid) != OutcomePending || api.current() != "old" {
		t.Fatalf("revert: id=%d err=%v", rid, err)
	}
	if err := confirmCur(svc); err != nil {
		t.Fatal(err)
	}
	cid, _, err := svc.Change(context.Background(), "alice", func(string) (string, error) { return "third", nil }, "r")
	if err != nil || svc.Outcome(cid) != OutcomePending || api.current() != "third" {
		t.Fatalf("change: id=%d err=%v", cid, err)
	}
}

func handled500() error {
	return &headscale.APIError{Status: 500, Message: "setting policy: parsing policy: bad tag", Handled: true}
}

func TestApplyHandled500NotAppliedAbortsAndReturnsAPIError(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(handled500())
	svc, _, _ := newTestService(t, api)
	_, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	var ae *headscale.APIError
	if !errors.As(err, &ae) || errors.Is(err, ErrUncertain) || !strings.Contains(ae.Message, "parsing policy") {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := svc.store.Pending(); ok {
		t.Fatal("pending left behind")
	}
	if h, _ := svc.store.History(5); len(h) != 0 {
		t.Fatalf("history = %v", h)
	}
}

func TestApplyHandled500ButPolicyChangedKeepsPendingUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(handled500())
	api.other = "someone else's"
	svc, _, _ := newTestService(t, api)
	id, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
	if p, ok, _ := svc.store.Pending(); !ok || p.ID != id {
		t.Fatalf("pending = %+v %v", p, ok)
	}
}

func TestApplyHandled500ReReadFailsKeepsPendingUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(handled500())
	api.breakGet = true
	svc, _, _ := newTestService(t, api)
	if _, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
	if _, ok, _ := svc.store.Pending(); !ok {
		t.Fatal("not pending")
	}
}

func TestApplyHandled500ButActuallyAppliedIsSuccess(t *testing.T) {
	api := newFakeAPI("old")
	api.applyErr = handled500()
	svc, _, _ := newTestService(t, api)
	id := applyOK(t, svc, "new")
	if svc.Outcome(id) != OutcomePending {
		t.Fatalf("outcome = %q", svc.Outcome(id))
	}
}

func TestApplyPlain500StillKeepsPendingUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(errors.New("setting the policy: unexpected status 500"))
	svc, _, _ := newTestService(t, api)
	id, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit")
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
	if p, ok, _ := svc.store.Pending(); !ok || p.ID != id {
		t.Fatalf("pending = %+v %v", p, ok)
	}
}

func TestApplyTimeoutStillKeepsPendingUncertain(t *testing.T) {
	api := newFakeAPI("old")
	api.fail(context.DeadlineExceeded)
	svc, _, _ := newTestService(t, api)
	if _, _, err := svc.Apply(context.Background(), "alice", "new", "v1", "edit"); !errors.Is(err, ErrUncertain) {
		t.Fatalf("err = %v", err)
	}
}

func TestEmptyPolicyIsRefusedBeforeAnyAPICall(t *testing.T) {
	api := newFakeAPI("old")
	svc, _, _ := newTestService(t, api)
	ctx := context.Background()
	for _, txt := range []string{"", " \n\t "} {
		if _, _, err := svc.Apply(ctx, "alice", txt, "v1", "edit"); !errors.Is(err, ErrEmpty) {
			t.Errorf("apply %q: %v", txt, err)
		}
	}
	if _, _, err := svc.Change(ctx, "alice", func(string) (string, error) { return "  ", nil }, "r"); !errors.Is(err, ErrEmpty) {
		t.Errorf("change: %v", err)
	}
	if api.setCount() != 0 {
		t.Error("a write was sent")
	}
	// A history entry whose before text is empty cannot be reverted to.
	id, err := svc.store.Begin("alice", "edit", "", "x", svc.clock().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	svc.store.Confirm(id)
	calls := api.calls()
	if _, _, err := svc.RevertTo(ctx, "alice", id); !errors.Is(err, ErrEmpty) {
		t.Errorf("revert: %v", err)
	}
	if api.calls() != calls {
		t.Error("revert called the API")
	}
	p, err := svc.Preview(ctx, "  ")
	if err != nil || p.Valid || p.Error != "the policy is empty" {
		t.Errorf("preview = %+v %v", p, err)
	}
	if api.calls() != calls {
		t.Error("preview called the API")
	}
}
