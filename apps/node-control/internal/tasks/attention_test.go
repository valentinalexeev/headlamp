package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

var _ core.AttentionSource = (*Module)(nil)

func attentionHarness(t *testing.T) (*modHarness, *Store) {
	t.Helper()
	h := newModHarness(t, nil)
	s := h.waitOpen()
	h.pushSimple()
	return h, s
}

func TestTasksAttention(t *testing.T) {
	ctx := context.Background()
	now := time.Now()

	t.Run("a task with no success yet is not counted as passing and raises nothing", func(t *testing.T) {
		h, s := attentionHarness(t)
		s.SaveState("job", TaskState{NextRun: now.Add(time.Hour)})
		items, lines, err := h.m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 1 || lines[0].State != "ok" || !strings.Contains(lines[0].Summary, "0 of 1 passing") {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("a task that passed is normal", func(t *testing.T) {
		h, s := attentionHarness(t)
		s.SaveState("job", TaskState{LastSuccess: now.Add(-time.Minute), NextRun: now.Add(time.Hour)})
		items, lines, err := h.m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
		if len(lines) != 1 || lines[0].Name != "Tasks" || lines[0].Path != "/tasks" || lines[0].State != "ok" || !strings.Contains(lines[0].Summary, "1 of 1 passing") {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("a failing streak is a waiting item with the streak", func(t *testing.T) {
		h, s := attentionHarness(t)
		s.SaveState("job", TaskState{LastSuccess: now.Add(-time.Minute), Streak: 3, NextRun: now.Add(time.Hour)})
		items, lines, err := h.m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		it := items[0]
		if it.Level != core.LevelWaiting || it.Source != "tasks" || it.ActionPath != "/tasks?sel=job" || !strings.Contains(it.Title, "3 times in a row") || !strings.Contains(it.Title, "job") {
			t.Errorf("%+v", it)
		}
		if lines[0].State != "waiting" || !strings.Contains(lines[0].Summary, "0 of 1 passing") {
			t.Errorf("lines: %+v", lines)
		}
		if it.AckKey != "task-job|failing" {
			t.Errorf("a longer streak means the same, overdue does not: %q", it.AckKey)
		}
	})

	t.Run("a single failure still states the streak", func(t *testing.T) {
		h, s := attentionHarness(t)
		s.SaveState("job", TaskState{LastSuccess: now.Add(-time.Minute), Streak: 1, NextRun: now.Add(time.Hour)})
		items, _, err := h.m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 || !strings.Contains(items[0].Title, "1 time in a row") {
			t.Fatalf("%+v %v", items, err)
		}
	})

	t.Run("a stale task is a waiting item", func(t *testing.T) {
		h, s := attentionHarness(t)
		s.SaveState("job", TaskState{LastSuccess: now.Add(-3 * time.Hour), NextRun: now.Add(time.Hour)})
		items, _, err := h.m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		if it := items[0]; it.Level != core.LevelWaiting || it.ActionPath != "/tasks?sel=job" || !strings.Contains(it.Title, "overdue") || it.AckKey != "task-job|overdue" {
			t.Errorf("%+v", it)
		}
	})

	t.Run("a task id with unsafe characters still gives a safe path", func(t *testing.T) {
		if p := taskPath("a b&c"); p != "/tasks?sel=a+b%26c" {
			t.Errorf("%q", p)
		}
	})

	t.Run("a caller outside TASKS_ADMIN_USERS sees nothing", func(t *testing.T) {
		h, _ := attentionHarness(t)
		items, lines, err := h.m.Attention(ctx, "bob@")
		if !errors.Is(err, core.ErrNotVisible) || len(items) != 0 || len(lines) != 0 {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("an inactive module reports nothing at all", func(t *testing.T) {
		items, lines, err := New(core.MapEnv(nil), nil).(*Module).Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 0 {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})
}

func TestTasksAttentionStoreUnavailableIsUnknown(t *testing.T) {
	h := newSessHarness(t, false) // Redis is unreachable: the store never opens
	items, lines, err := h.m.Attention(context.Background(), "alice@")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Errorf("a closed store is unknown, not at risk: no item expected: %+v", items)
	}
	if len(lines) != 1 || lines[0].State != "unknown" {
		t.Errorf("lines: %+v", lines)
	}
}

func TestTasksAttentionRunningSession(t *testing.T) {
	ctx := context.Background()
	h := newSessHarness(t, true, sessNode("runner-1", "100.64.0.10", sessTag), sessNode("runner-2", "100.64.0.11", sessTag))
	h.m.now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC) }
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Token: sessTok, Port: 6080,
		ExpiresAt: time.Date(2026, 10, 31, 12, 42, 0, 0, time.UTC)}
	h.fake.curErr["100.64.0.11"] = errors.New("unreachable")

	items, lines, err := h.m.Attention(ctx, "alice@")
	if err != nil {
		t.Fatal(err)
	}
	var sess *core.AttentionItem
	for i := range items {
		if items[i].ActionPath == "/sessions" {
			sess = &items[i]
		}
	}
	if sess == nil {
		t.Fatalf("no session item: %+v", items)
	}
	if sess.Level != core.LevelInfo || sess.Source != "tasks" || !strings.Contains(sess.Title, "runner-1") ||
		!strings.Contains(sess.Detail, "42 min left") || strings.Contains(sess.Title+sess.Detail+sess.ActionPath, sessTok) {
		t.Errorf("%+v", *sess)
	}
	found := false
	for _, l := range lines {
		if l.Name == "Sessions" && l.Path == "/sessions" && strings.Contains(l.Summary, "1 running") {
			found = true
		}
	}
	if !found {
		t.Errorf("lines: %+v", lines)
	}
}

func TestTasksAttentionNodeListFailureMakesSessionsUnknown(t *testing.T) {
	h := newSessHarness(t, true)
	h.m.src = &fakeNodes{err: errors.New("headscale down")}
	_, lines, err := h.m.Attention(context.Background(), "alice@")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range lines {
		if l.Name == "Sessions" && l.State == "unknown" {
			return
		}
	}
	t.Errorf("lines: %+v", lines)
}

func TestTasksAttentionCachesSessionCalls(t *testing.T) {
	h := newSessHarness(t, true, sessNode("runner-1", "100.64.0.10", sessTag))
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h.m.now = func() time.Time { return now }
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Token: sessTok, Port: 6080, ExpiresAt: now.Add(time.Hour)}
	for i := 0; i < 3; i++ {
		if _, _, err := h.m.Attention(context.Background(), "alice@"); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(h.fake.callList()); n != 1 {
		t.Fatalf("three polls called the runner %d times, want 1", n)
	}
	now = now.Add(21 * time.Second)
	h.m.Attention(context.Background(), "alice@")
	if n := len(h.fake.callList()); n != 2 {
		t.Fatalf("after the cache expired: %d calls, want 2", n)
	}
}

var (
	_ core.ActivitySource = (*Module)(nil)
	_ core.SearchSource   = (*Module)(nil)
)

func TestTasksActivity(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	run := func(id string, state RunState, ended time.Duration, manual bool, reason string) Run {
		return Run{ID: id, Task: "job", State: state, Started: now.Add(ended - time.Second), Ended: now.Add(ended), Manual: manual, Reason: reason,
			Result: map[string]any{"balance": 12.5}}
	}

	t.Run("manual and failed runs are listed, passing scheduled runs are not, results never", func(t *testing.T) {
		h, s := attentionHarness(t)
		for _, r := range []Run{
			run("r1", RunSucceeded, -5*time.Minute, false, ""), // scheduled and passing: left out
			run("r2", RunFailed, -4*time.Minute, false, "login rejected"),
			run("r3", RunSucceeded, -3*time.Minute, true, ""), // from before the actor was recorded
			run("r4", RunCancelled, -2*time.Minute, false, ""),
			run("r5", RunFailed, -time.Minute, false, reasonShutdown), // a deploy, not a failure
			{ID: "r6", Task: "job", State: RunRunning, Started: now},  // not finished
		} {
			if err := s.AppendRun(r); err != nil {
				t.Fatal(err)
			}
		}
		got, err := h.m.Activity(ctx, "alice@", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("%+v", got)
		}
		want := []struct{ actor, action, detail string }{
			{"operator", "task.run", "job: cancelled"},
			{"operator", "task.manual", "job: succeeded"},
			{"system", "task.run", "job: failed"},
		}
		for i, w := range want {
			e := got[i]
			if e.Source != "Tasks" || e.Actor != w.actor || e.Action != w.action || e.Detail != w.detail || e.At.IsZero() {
				t.Errorf("%d: %+v", i, e)
			}
		}
		if strings.Contains(fmt.Sprint(got), "12.5") || strings.Contains(fmt.Sprint(got), "login rejected") {
			t.Errorf("a result or a reason leaked: %+v", got)
		}
		if one, _ := h.m.Activity(ctx, "alice@", 1); len(one) != 1 {
			t.Errorf("limit: %+v", one)
		}
	})

	t.Run("a caller outside TASKS_ADMIN_USERS sees nothing", func(t *testing.T) {
		h, _ := attentionHarness(t)
		if got, err := h.m.Activity(ctx, "bob@", 10); !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
		if got, err := h.m.Search(ctx, "bob@"); !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("an inactive module reports nothing", func(t *testing.T) {
		off := &Module{}
		if got, err := off.Activity(ctx, "alice@", 10); got != nil || err != nil || off.HasActivity() {
			t.Fatalf("%+v %v", got, err)
		}
		if got, err := off.Search(ctx, "alice@"); got != nil || err != nil {
			t.Fatalf("%+v %v", got, err)
		}
	})
}

func TestTasksSearchListsTaskIDs(t *testing.T) {
	h, _ := attentionHarness(t)
	got, err := h.m.Search(context.Background(), "alice@")
	if err != nil || len(got) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if it := got[0]; it.Label != "job" || it.Kind != "Task" || it.Path != "/tasks?sel=job" {
		t.Errorf("%+v", it)
	}
}

func TestTasksActivityNamesWhoStartedAManualRun(t *testing.T) {
	h, s := attentionHarness(t)
	now := time.Now()
	s.AppendRun(Run{ID: "r1", Task: "job", State: RunSucceeded, Started: now.Add(-time.Minute), Ended: now, Manual: true, Actor: "alice@"})
	got, err := h.m.Activity(context.Background(), "alice@", 10)
	if err != nil || len(got) != 1 || got[0].Actor != "alice@" || got[0].Action != "task.manual" {
		t.Fatalf("%+v %v", got, err)
	}
}

// slowSessions holds every Current call until release is closed or the call's context ends.
type slowSessions struct {
	*fakeSessions
	entered chan struct{}
	release chan struct{}
}

func (s *slowSessions) Current(ctx context.Context, addr string) (*SessionInfo, error) {
	s.entered <- struct{}{}
	select {
	case <-s.release:
		return s.fakeSessions.Current(ctx, addr)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func slowSessionHarness(t *testing.T) (*sessHarness, *slowSessions) {
	t.Helper()
	h := newSessHarness(t, true, sessNode("runner-1", "100.64.0.10", sessTag))
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	h.m.now = func() time.Time { return now }
	h.fake.cur["100.64.0.10"] = &SessionInfo{ID: "s1", State: "running", Token: sessTok, Port: 6080, ExpiresAt: now.Add(time.Hour)}
	slow := &slowSessions{fakeSessions: h.fake, entered: make(chan struct{}, 16), release: make(chan struct{})}
	h.m.sess.c = slow
	return h, slow
}

func sessionItems(items []core.AttentionItem) int {
	n := 0
	for _, it := range items {
		if it.ActionPath == "/sessions" {
			n++
		}
	}
	return n
}

// A poll that has given up must not wait for a read another poll started.
func TestTasksAttentionDoesNotQueueBehindASlowSessionRead(t *testing.T) {
	h, slow := slowSessionHarness(t)
	go h.m.Attention(context.Background(), "alice@")
	<-slow.entered

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		h.m.Attention(ctx, "alice@")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("a cancelled poll queued behind the running read")
	}
	close(slow.release)
}

// A poll that goes away mid-read must not leave "None running" behind for the next poll.
func TestTasksAttentionCancelledFirstReadIsNotCached(t *testing.T) {
	h, slow := slowSessionHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		h.m.Attention(ctx, "alice@")
		close(done)
	}()
	<-slow.entered
	cancel()
	<-done
	close(slow.release)

	items, _, err := h.m.Attention(context.Background(), "alice@")
	if err != nil {
		t.Fatal(err)
	}
	if sessionItems(items) != 1 {
		t.Fatalf("the running session is missing after a cancelled read: %+v", items)
	}
}

func TestTasksAttentionPollsShareOneSessionRead(t *testing.T) {
	h, slow := slowSessionHarness(t)
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if items, _, err := h.m.Attention(context.Background(), "alice@"); err != nil || sessionItems(items) != 1 {
				t.Errorf("%+v %v", items, err)
			}
		}()
	}
	<-slow.entered
	time.Sleep(50 * time.Millisecond) // let the others arrive
	close(slow.release)
	wg.Wait()
	if n := len(h.fake.callList()); n != 1 {
		t.Fatalf("concurrent polls called the runner %d times, want 1", n)
	}
}

func mustOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestTasksAttentionFailureClass(t *testing.T) {
	now := time.Now()
	t.Run("a structural failure names its class and node counts", func(t *testing.T) {
		h, s := attentionHarness(t)
		mustOK(t, s.SaveState("job", TaskState{LastSuccess: now.Add(-time.Minute), Streak: 1, NextRun: now.Add(time.Hour)}))
		mustOK(t, s.AppendRun(Run{ID: "r1", Task: "job", State: RunFailed, Reason: "no eligible node", Class: ClassStructural,
			Counts: &NodeCounts{}, Started: now, Ended: now}))
		items, _, err := h.m.Attention(context.Background(), "alice@")
		if err != nil || len(items) != 1 || !strings.Contains(items[0].Detail, "Last failure (structural): no eligible node (tagged 0).") {
			t.Fatalf("%+v %v", items, err)
		}
	})

	t.Run("a script error shows only its class", func(t *testing.T) {
		h, s := attentionHarness(t)
		mustOK(t, s.SaveState("job", TaskState{LastSuccess: now.Add(-time.Minute), Streak: 1, NextRun: now.Add(time.Hour)}))
		mustOK(t, s.AppendRun(Run{ID: "r1", Task: "job", State: RunFailed, Reason: "script text", Class: ClassUnknown, Started: now, Ended: now}))
		items, _, err := h.m.Attention(context.Background(), "alice@")
		if err != nil || len(items) != 1 || !strings.HasSuffix(items[0].Detail, "Last failure: unknown.") || strings.Contains(items[0].Detail, "script text") {
			t.Fatalf("%+v %v", items, err)
		}
	})
}
