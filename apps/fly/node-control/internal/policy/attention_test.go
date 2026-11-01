package policy

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

var _ core.ActivitySource = (*Module)(nil)

func TestPolicyActivity(t *testing.T) {
	ctx := context.Background()
	byAction := func(got []core.ActivityEntry, action string) []core.ActivityEntry {
		var out []core.ActivityEntry
		for _, e := range got {
			if e.Action == action {
				out = append(out, e)
			}
		}
		return out
	}

	t.Run("applied, confirmed and reverted changes carry the recorded actor and time", func(t *testing.T) {
		m, svc, _, clk := attentionModule(t)
		kept := applyOK(t, svc, "kept")
		clk.Advance(time.Minute)
		if err := svc.Confirm(ctx, "alice@", kept); err != nil {
			t.Fatal(err)
		}
		confirmedAt := clk.Now()
		clk.Advance(time.Minute)
		lost := applyOK(t, svc, "lost")
		clk.Advance(testRevert + time.Second)
		svc.checkRevert(ctx)
		revertedAt := clk.Now()

		got, err := m.Activity(ctx, "alice@", 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range got {
			if e.Source != "Policy" || e.At.IsZero() || e.Actor == "" {
				t.Errorf("%+v", e)
			}
		}
		applied := byAction(got, "change.applied")
		if len(applied) != 2 || !strings.HasPrefix(applied[0].Detail, fmt.Sprint(lost)) || !strings.HasPrefix(applied[1].Detail, fmt.Sprint(kept)) {
			t.Fatalf("applied, newest first: %+v", applied)
		}
		if c := byAction(got, "change.confirmed"); len(c) != 1 || c[0].Actor != "alice@" || c[0].Detail != fmt.Sprint(kept) || !c[0].At.Equal(confirmedAt) {
			t.Errorf("confirmed: %+v", c)
		}
		if r := byAction(got, "change.reverted"); len(r) != 1 || r[0].Actor != "system" || r[0].Detail != fmt.Sprint(lost) || !r[0].At.Equal(revertedAt) {
			t.Errorf("reverted: %+v", r)
		}
		if strings.Contains(fmt.Sprint(got), "lost\n") {
			t.Error("the policy text must not appear")
		}
	})

	t.Run("a revert on request stays the operator's after the deadline passes, at the same time", func(t *testing.T) {
		m, svc, _, clk := attentionModule(t)
		id := applyOK(t, svc, "new")
		clk.Advance(10 * time.Second)
		if err := svc.RevertPending(ctx, "alice@", id); err != nil {
			t.Fatal(err)
		}
		at := clk.Now()
		read := func() core.ActivityEntry {
			got, err := m.Activity(ctx, "alice@", 20)
			if err != nil {
				t.Fatal(err)
			}
			r := byAction(got, "change.reverted_on_request")
			if len(r) != 1 || len(byAction(got, "change.reverted")) != 0 {
				t.Fatalf("%+v", got)
			}
			return r[0]
		}
		before := read()
		clk.Advance(testRevert + time.Hour)
		after := read()
		for _, e := range []core.ActivityEntry{before, after} {
			if e.Actor != "alice@" || e.Detail != fmt.Sprint(id) || !e.At.Equal(at) {
				t.Errorf("%+v, want actor alice@ at %v", e, at)
			}
		}
	})

	t.Run("a reverted change from before outcomes were recorded reads as a plain revert, never timed now", func(t *testing.T) {
		m, svc, _, clk := attentionModule(t)
		id := applyOK(t, svc, "old")
		deadline := clk.Now().Add(testRevert)
		clk.Advance(5 * time.Second)
		if err := svc.RevertPending(ctx, "bob@", id); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.store.db.Exec("DELETE FROM outcomes"); err != nil { // as if it ended before the table existed
			t.Fatal(err)
		}
		for _, advance := range []time.Duration{0, 3 * time.Hour} {
			clk.Advance(advance)
			got, err := m.Activity(ctx, "alice@", 20)
			if err != nil {
				t.Fatal(err)
			}
			r := byAction(got, "change.reverted_earlier")
			if len(r) != 1 || r[0].Actor != "alice" || r[0].Detail != fmt.Sprint(id) || !r[0].At.Equal(deadline) {
				t.Fatalf("%+v, want actor alice at %v", got, deadline)
			}
			if len(byAction(got, "change.confirmed")) != 0 {
				t.Errorf("%+v", got)
			}
		}
	})

	t.Run("a caller outside POLICY_ADMIN_USERS sees nothing", func(t *testing.T) {
		m, svc, _, _ := attentionModule(t)
		applyOK(t, svc, "new")
		got, err := m.Activity(ctx, "bob@", 20)
		if !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
			t.Fatalf("%+v %v", got, err)
		}
	})

	t.Run("an inactive module reports nothing", func(t *testing.T) {
		off := &Module{env: core.MapEnv(nil)}
		got, err := off.Activity(ctx, "alice@", 20)
		if err != nil || got != nil || off.HasActivity() {
			t.Fatalf("%+v %v", got, err)
		}
	})
}
