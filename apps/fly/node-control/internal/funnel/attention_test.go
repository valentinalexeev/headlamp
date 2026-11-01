package funnel

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
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

var (
	_ core.ActivitySource = (*Module)(nil)
	_ core.SearchSource   = (*Module)(nil)
)

func TestFunnelActivityMapsTheAuditLog(t *testing.T) {
	ctx := context.Background()
	m := attentionModule(t)
	m.audit1("alice@", "namespace.create", "demo ports 9000-9010")
	m.audit1("alice@", "key.mint", "lab id 14 ephemeral=false reusable=true ttl=24h")
	got, err := m.Activity(ctx, "alice@", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if e := got[0]; e.Source != "Funnel" || e.Action != "key.mint" || e.Actor != "alice@" || e.At.IsZero() ||
		e.Detail != "lab id 14 ephemeral=false reusable=true ttl=24h" {
		t.Errorf("newest first: %+v", e)
	}
	if got[1].Action != "namespace.create" {
		t.Errorf("%+v", got)
	}
	if one, _ := m.Activity(ctx, "alice@", 1); len(one) != 1 {
		t.Errorf("limit: %+v", one)
	}
}

func TestFunnelActivityAndSearchRespectVisibility(t *testing.T) {
	ctx := context.Background()
	m := attentionModule(t)
	m.audit1("alice@", "namespace.create", "demo")
	if got, err := m.Activity(ctx, "bob@", 10); !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
		t.Errorf("activity: %+v %v", got, err)
	}
	if got, err := m.Search(ctx, "bob@"); !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
		t.Errorf("search: %+v %v", got, err)
	}
	if got, err := m.Activity(ctx, "", 10); !errors.Is(err, core.ErrNotVisible) || len(got) != 0 {
		t.Errorf("an unknown caller: %+v %v", got, err)
	}
	off := &Module{}
	if got, err := off.Activity(ctx, "alice@", 10); got != nil || err != nil || off.HasActivity() {
		t.Errorf("an inactive module: %+v %v", got, err)
	}
	if got, err := off.Search(ctx, "alice@"); got != nil || err != nil {
		t.Errorf("an inactive module: %+v %v", got, err)
	}
}

func TestFunnelSearchListsNamespacesAndServices(t *testing.T) {
	m := attentionModule(t)
	if err := m.st.CreateNamespace("demo", []Range{{Lo: 8080, Hi: 8080}}, "", 7); err != nil {
		t.Fatal(err)
	}
	if err := m.st.PutManual(ManualRoute{NS: "demo", Service: "wiki", Target: "100.64.0.5", Port: 80}); err != nil {
		t.Fatal(err)
	}
	body := `{"routes":[{"ns":"demo","service":"wiki"},{"ns":"demo","service":"grafana"}]}`
	if err := m.st.SaveState("100.64.0.9", []byte(body), attentionNow); err != nil {
		t.Fatal(err)
	}
	got, err := m.Search(context.Background(), "alice@")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"demo": "Namespace", "wiki.demo": "Service", "grafana.demo": "Service"}
	if len(got) != len(want) {
		t.Fatalf("a service in both lists appears once: %+v", got)
	}
	for _, it := range got {
		if want[it.Label] != it.Kind {
			t.Errorf("%+v", it)
		}
		if it.Kind == "Namespace" && it.Path != "/funnel/config" || it.Kind == "Service" && it.Path != "/funnel" {
			t.Errorf("%+v", it)
		}
	}
}
