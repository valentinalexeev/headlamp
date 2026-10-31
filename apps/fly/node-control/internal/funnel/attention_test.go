package funnel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
)

var _ core.AttentionSource = (*Module)(nil)

var attentionNow = time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)

func attentionModule(t *testing.T) *Module {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "funnel.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return &Module{s: &Settings{Admins: authz.ParseAllowed("alice")}, st: st, svc: newFakePolicy(startPolicy),
		now: func() time.Time { return attentionNow }}
}

func report(t *testing.T, m *Module, at time.Time, routes int, errs ...string) {
	t.Helper()
	body := fmt.Sprintf(`{"applied_version":"v1","sent_at":%q,"snapshot_loaded":true,"peer_count":1,"routes":[%s],"errors":[%s]}`,
		at.Format(time.RFC3339), strings.TrimSuffix(strings.Repeat(`{"name":"a"},`, routes), ","), quoteAll(errs))
	if err := m.st.SaveState("100.64.0.9", []byte(body), at); err != nil {
		t.Fatal(err)
	}
}

func quoteAll(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = fmt.Sprintf("%q", s)
	}
	return strings.Join(q, ",")
}

func TestFunnelAttention(t *testing.T) {
	ctx := context.Background()

	t.Run("a fresh report without errors is normal", func(t *testing.T) {
		m := attentionModule(t)
		report(t, m, attentionNow.Add(-12*time.Second), 3)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
		if len(lines) != 1 || lines[0].Name != "Funnel" || lines[0].Path != "/funnel" || lines[0].State != "ok" ||
			!strings.Contains(lines[0].Summary, "3 routes") || !strings.Contains(lines[0].Summary, "12 s") {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("a report older than 90 seconds is a waiting item", func(t *testing.T) {
		m := attentionModule(t)
		report(t, m, attentionNow.Add(-91*time.Second), 3)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		if it := items[0]; it.Level != core.LevelWaiting || it.Source != "funnel" || it.ActionPath != "/funnel" || it.ActionLabel == "" {
			t.Errorf("%+v", it)
		}
		if lines[0].State != "waiting" {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("a report at exactly 90 seconds is not stale yet", func(t *testing.T) {
		m := attentionModule(t)
		report(t, m, attentionNow.Add(-90*time.Second), 1)
		if items, _, _ := m.Attention(ctx, "alice@"); len(items) != 0 {
			t.Fatalf("%+v", items)
		}
	})

	t.Run("a report with errors is a waiting item that never repeats the error text", func(t *testing.T) {
		m := attentionModule(t)
		report(t, m, attentionNow.Add(-5*time.Second), 3, "route a: dial refused", "route b: dial refused")
		items, _, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		if it := items[0]; it.Level != core.LevelWaiting || it.ActionPath != "/funnel" || !strings.Contains(it.Title, "2 errors") ||
			strings.Contains(it.Title+it.Detail, "dial refused") {
			t.Errorf("%+v", it)
		}
	})

	t.Run("a failed namespace is a waiting item that opens the configuration", func(t *testing.T) {
		m := attentionModule(t)
		report(t, m, attentionNow.Add(-5*time.Second), 1)
		if err := m.st.CreateNamespace("demo", []Range{{Lo: 8080, Hi: 8080}}, "", 7); err != nil {
			t.Fatal(err)
		}
		if err := m.st.MarkFailed("demo"); err != nil {
			t.Fatal(err)
		}
		items, _, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		if it := items[0]; it.Level != core.LevelWaiting || it.ActionPath != "/funnel/config" || !strings.Contains(it.Title, "demo") ||
			!strings.Contains(it.Detail, "reverted") {
			t.Errorf("%+v", it)
		}
	})

	t.Run("no report yet is unknown, not normal", func(t *testing.T) {
		m := attentionModule(t)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 1 || lines[0].State != "unknown" {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("a caller outside FUNNEL_ADMIN_USERS sees nothing", func(t *testing.T) {
		m := attentionModule(t)
		items, lines, err := m.Attention(ctx, "bob@")
		if !errors.Is(err, core.ErrNotVisible) || len(items) != 0 || len(lines) != 0 {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("an inactive module reports nothing at all", func(t *testing.T) {
		items, lines, err := (&Module{}).Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 0 {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("an unreadable store is an error", func(t *testing.T) {
		m := attentionModule(t)
		m.st.Close()
		if _, _, err := m.Attention(ctx, "alice@"); err == nil {
			t.Fatal("want an error")
		}
	})
}
