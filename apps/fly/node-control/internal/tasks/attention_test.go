package tasks

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
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
	if len(items) != 1 || items[0].Level != core.LevelCritical || !strings.Contains(items[0].Title, "could not be checked") {
		t.Errorf("items: %+v", items)
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

var _ = headscale.Node{}
