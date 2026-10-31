package policy

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/core"
)

var _ core.AttentionSource = (*Module)(nil)

func attentionModule(t *testing.T) (*Module, *Service, *fakeAPI, *fakeClock) {
	t.Helper()
	api := newFakeAPI("old")
	svc, clk, _ := newTestService(t, api)
	m := &Module{env: core.MapEnv(map[string]string{"POLICY_ADMIN_USERS": "alice"}), store: svc.store, svc: svc}
	return m, svc, api, clk
}

func TestPolicyAttention(t *testing.T) {
	ctx := context.Background()

	t.Run("a pending change is a timed item with its apply time and deadline", func(t *testing.T) {
		m, svc, _, clk := attentionModule(t)
		id := applyOK(t, svc, "new")
		appliedAt := clk.Now()
		clk.Advance(30 * time.Second)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 {
			t.Fatalf("items: %+v", items)
		}
		it := items[0]
		if it.Level != core.LevelTimed || it.Source != "policy" || it.ActionPath != "/policy" || it.ActionLabel == "" {
			t.Errorf("%+v", it)
		}
		if it.ChangeID != id || !it.AppliedAt.Equal(appliedAt) || !it.Deadline.Equal(appliedAt.Add(testRevert)) {
			t.Errorf("change %d applied %v deadline %v: %+v", id, appliedAt, appliedAt.Add(testRevert), it)
		}
		if !strings.Contains(it.Title, "#") || !strings.Contains(it.Detail, "Reverts by itself") {
			t.Errorf("copy: %q / %q", it.Title, it.Detail)
		}
		if len(lines) != 1 || lines[0].Name != "Policy" || lines[0].Path != "/policy" || lines[0].State != "waiting" {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("an overdue change whose revert keeps failing is critical", func(t *testing.T) {
		m, svc, api, clk := attentionModule(t)
		applyOK(t, svc, "new")
		api.fail(errors.New("headscale is down"))
		clk.Advance(testRevert + time.Second)
		svc.checkRevert(ctx)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Level != core.LevelCritical || items[0].ActionPath != "/policy" {
			t.Fatalf("items: %+v", items)
		}
		if !strings.Contains(items[0].Title, "neither confirmed nor reverted") {
			t.Errorf("title %q", items[0].Title)
		}
		if len(lines) != 1 || lines[0].State != "critical" {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("an overdue change not yet tried again is still timed", func(t *testing.T) {
		m, svc, _, clk := attentionModule(t)
		applyOK(t, svc, "new")
		clk.Advance(testRevert + time.Second)
		items, _, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 || items[0].Level != core.LevelTimed {
			t.Fatalf("%+v %v", items, err)
		}
	})

	t.Run("a confirmed change leaves no item and an ok line", func(t *testing.T) {
		m, svc, _, _ := attentionModule(t)
		id := applyOK(t, svc, "new")
		if err := svc.Confirm(ctx, "alice", id); err != nil {
			t.Fatal(err)
		}
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
		if len(lines) != 1 || lines[0].State != "ok" || !strings.Contains(lines[0].Summary, "confirmed") {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("a caller outside POLICY_ADMIN_USERS sees nothing", func(t *testing.T) {
		m, svc, _, _ := attentionModule(t)
		applyOK(t, svc, "new")
		for _, login := range []string{"bob@", ""} {
			items, lines, err := m.Attention(ctx, login)
			if !errors.Is(err, core.ErrNotVisible) || len(items) != 0 || len(lines) != 0 {
				t.Errorf("login %q: %+v %+v %v", login, items, lines, err)
			}
		}
	})

	t.Run("an inactive console reports nothing at all", func(t *testing.T) {
		m := &Module{env: core.MapEnv(nil)}
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 0 {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("an unreadable store is an error, not a calm answer", func(t *testing.T) {
		m, svc, _, _ := attentionModule(t)
		svc.store.Close()
		if _, _, err := m.Attention(ctx, "alice@"); err == nil {
			t.Fatal("want an error")
		}
	})
}
