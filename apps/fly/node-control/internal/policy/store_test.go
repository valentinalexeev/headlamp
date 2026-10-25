package policy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func openTestStore(t *testing.T, path string, clk *fakeClock) *Store {
	t.Helper()
	s, err := OpenStore(path, clk.Now)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newTestStore(t *testing.T) (*Store, *fakeClock, string) {
	t.Helper()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "policy.db")
	return openTestStore(t, path, clk), clk, path
}

func mustBegin(t *testing.T, s *Store, clk *fakeClock) int64 {
	t.Helper()
	id, err := s.Begin("alice", "edit", "before", "after", clk.Now().Add(2*time.Minute))
	if err != nil {
		t.Fatalf("Begin: %v", err)
	}
	return id
}

func TestBeginTwiceIsBusy(t *testing.T) {
	s, clk, _ := newTestStore(t)
	mustBegin(t, s, clk)
	_, err := s.Begin("bob", "x", "b", "a", clk.Now().Add(time.Minute))
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
}

func TestConfirmClearsPendingAndOutcomeConfirmed(t *testing.T) {
	s, clk, _ := newTestStore(t)
	id := mustBegin(t, s, clk)
	if got := s.Outcome(id); got != OutcomePending {
		t.Fatalf("outcome = %q", got)
	}
	if err := s.Confirm(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.Pending(); ok || err != nil {
		t.Fatalf("pending = %v, %v", ok, err)
	}
	if got := s.Outcome(id); got != OutcomeConfirmed {
		t.Fatalf("outcome = %q", got)
	}
	mustBegin(t, s, clk) // a new change is allowed now
}

func TestRevertedOutcome(t *testing.T) {
	s, clk, _ := newTestStore(t)
	id := mustBegin(t, s, clk)
	if err := s.MarkReverted(id, true); err != nil {
		t.Fatal(err)
	}
	if got := s.Outcome(id); got != OutcomeReverted {
		t.Fatalf("outcome = %q", got)
	}
	if _, ok, _ := s.Pending(); ok {
		t.Fatal("still pending")
	}
}

func TestFailedRevertMarkStaysPendingAndCountsAttempts(t *testing.T) {
	s, clk, _ := newTestStore(t)
	id := mustBegin(t, s, clk)
	for i := 1; i <= 2; i++ {
		if err := s.MarkReverted(id, false); err != nil {
			t.Fatal(err)
		}
		p, ok, err := s.Pending()
		if err != nil || !ok || p.ID != id || p.Attempts != i {
			t.Fatalf("pending = %+v %v %v, want attempts %d", p, ok, err, i)
		}
	}
	if got := s.Outcome(id); got != OutcomePending {
		t.Fatalf("outcome = %q", got)
	}
}

func TestUnknownIDOutcomeUnknown(t *testing.T) {
	s, _, _ := newTestStore(t)
	if got := s.Outcome(42); got != OutcomeUnknown {
		t.Fatalf("outcome = %q", got)
	}
}

func TestPendingSurvivesReopen(t *testing.T) {
	s, clk, path := newTestStore(t)
	id := mustBegin(t, s, clk)
	s.Close()
	s2 := openTestStore(t, path, clk)
	p, ok, err := s2.Pending()
	if err != nil || !ok {
		t.Fatalf("pending = %v %v", ok, err)
	}
	if p.ID != id || p.Actor != "alice" || p.Reason != "edit" || p.Before != "before" ||
		!p.Deadline.Equal(clk.Now().Add(2*time.Minute)) {
		t.Fatalf("pending = %+v", p)
	}
}

func TestHistoryOrderLimitAndNoText(t *testing.T) {
	s, clk, _ := newTestStore(t)
	var ids []int64
	for i := 0; i < 3; i++ {
		ids = append(ids, mustBegin(t, s, clk))
		if err := s.Confirm(ids[i]); err != nil {
			t.Fatal(err)
		}
		clk.Advance(time.Minute)
	}
	h, err := s.History(2)
	if err != nil || len(h) != 2 {
		t.Fatalf("history = %v %v", h, err)
	}
	if h[0].ID != ids[2] || h[1].ID != ids[1] {
		t.Fatalf("order = %d, %d", h[0].ID, h[1].ID)
	}
	if h[0].Actor != "alice" || h[0].Reason != "edit" || h[0].State != OutcomeConfirmed || !h[0].At.After(h[1].At) {
		t.Fatalf("entry = %+v", h[0])
	}
}

func TestTextOfRoundtrip(t *testing.T) {
	s, clk, _ := newTestStore(t)
	id := mustBegin(t, s, clk)
	if got, err := s.TextOf(id, "before"); err != nil || got != "before" {
		t.Fatalf("before = %q %v", got, err)
	}
	if got, err := s.TextOf(id, "after"); err != nil || got != "after" {
		t.Fatalf("after = %q %v", got, err)
	}
	if _, err := s.TextOf(id, "other"); err == nil {
		t.Fatal("expected error for unknown column")
	}
	if _, err := s.TextOf(id+99, "before"); err == nil {
		t.Fatal("expected error for unknown id")
	}
}

func TestAbortRemovesRow(t *testing.T) {
	s, clk, _ := newTestStore(t)
	id := mustBegin(t, s, clk)
	if err := s.Abort(id); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Pending(); ok {
		t.Fatal("still pending")
	}
	if h, _ := s.History(10); len(h) != 0 {
		t.Fatalf("history = %v", h)
	}
	if got := s.Outcome(id); got != OutcomeUnknown {
		t.Fatalf("outcome = %q", got)
	}
}

func TestCorruptDBErrorsAndFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.db")
	garbage := []byte(strings.Repeat("this is not a database\n", 200))
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenStore(path, nil)
	if err == nil {
		s.Close()
		t.Fatal("expected an error")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || !bytes.Equal(got, garbage) {
		t.Fatalf("file changed: %v", rerr)
	}
}

func TestConcurrentBeginExactlyOneWins(t *testing.T) {
	s, clk, _ := newTestStore(t)
	var wins, busy atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Begin("alice", "r", "b", "a", clk.Now().Add(time.Minute))
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrBusy):
				busy.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins.Load() != 1 || busy.Load() != 19 {
		t.Fatalf("wins=%d busy=%d", wins.Load(), busy.Load())
	}
}
