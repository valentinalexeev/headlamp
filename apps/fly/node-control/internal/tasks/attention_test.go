package tasks

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
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
		if it := items[0]; it.Level != core.LevelWaiting || it.ActionPath != "/tasks?sel=job" || !strings.Contains(it.Title, "overdue") {
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
			run("r3", RunSucceeded, -3*time.Minute, true, ""),
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
