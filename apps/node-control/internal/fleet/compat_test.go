package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/enroll"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/tasks"
)

// compatDeps: one bridge of each kind, a funnel ingress and two runners, against the embedded matrix (all
// interfaces at version 1).
func compatDeps(strict bool) Deps {
	yes := true
	return Deps{
		Bridges: func(context.Context) []enroll.IngressView {
			return []enroll.IngressView{
				{Name: "good", Source: "discovered", Active: true, Online: &yes, AgentVersion: "0.18.0", AgentAPI: 1},
				{Name: "ahead", Source: "discovered", Active: true, Online: &yes, AgentVersion: "0.19.0", AgentAPI: 2},
				{Name: "old", Source: "discovered", Active: true, Online: &yes, AgentVersion: "0.17.0"},
				{Name: "waiting", Source: "discovered", Online: &yes, AgentAPI: 9}, // not active: not checked
			}
		},
		Funnel: func() (funnel.IngressStatus, bool) {
			return funnel.IngressStatus{Version: "0.19.0", API: 2, ReportedAt: time.Now()}, true
		},
		Runners: func() map[string]tasks.RunnerAPI {
			return map[string]tasks.RunnerAPI{"100.64.0.7": {API: 1}, "100.64.0.8": {API: 5}}
		},
		Strict: strict,
	}
}

func fleetJSON(t *testing.T, m core.Module) Report {
	t.Helper()
	mux := core.NewMux()
	_ = m.Register(&core.Routes{Admin: mux})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet.json", nil))
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// #300: each part is checked against the matrix; incompatible is a problem with what to do, unknown is a note.
func TestFleetCompat(t *testing.T) {
	r := fleetJSON(t, New(compatDeps(false)))
	want := []string{
		"bridge ahead: agent-api version 2 is newer than node-control accepts: upgrade node-control (node-control accepts 1 to 1)",
		"funnel ingress: funnel-api version 2 is newer than node-control accepts: upgrade node-control (node-control accepts 1 to 1)",
		"task runner 100.64.0.8: runner-api version 5 is newer than node-control accepts: upgrade node-control (node-control accepts 1 to 1)",
	}
	if !slices.Equal(r.Problems, want) {
		t.Errorf("problems\n got %q\nwant %q", r.Problems, want)
	}
	if !slices.Equal(r.Notes, []string{"bridge old: reports no agent-api version (before 0.18.0)"}) {
		t.Errorf("notes %q", r.Notes)
	}
	if r.Bridges[0].Compat == nil || r.Bridges[0].Compat.State != "ok" || r.Bridges[3].Compat != nil {
		t.Errorf("bridges %+v", r.Bridges)
	}
	if len(r.Runners) != 2 || r.Runners[0].Compat.State != "ok" || r.Runners[1].Compat.State != "incompatible" {
		t.Errorf("runners %+v", r.Runners)
	}
	if r.Strict {
		t.Error("strict is off by default")
	}
}

// The Overview items: waiting by default, critical under COMPAT_STRICT; a part that reports nothing is not an item.
func TestFleetCompatAttention(t *testing.T) {
	for _, strict := range []bool{false, true} {
		m := New(compatDeps(strict)).(core.AttentionSource)
		items, lines, err := m.Attention(context.Background(), "admin@")
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, it := range items {
			ids = append(ids, it.ID)
			want := core.LevelWaiting
			if strict {
				want = core.LevelCritical
			}
			if it.Level != want || it.Source != "fleet" || it.ActionPath != "/fleet" {
				t.Errorf("strict=%v: %+v", strict, it)
			}
		}
		if !slices.Equal(ids, []string{"compat-bridge-ahead", "compat-funnel", "compat-runner-100.64.0.8"}) {
			t.Errorf("strict=%v: ids %v", strict, ids)
		}
		if !strings.Contains(items[0].Detail, "Run the ingress-agent role on ahead") {
			t.Errorf("detail %q", items[0].Detail)
		}
		wantLine := "waiting"
		if strict {
			wantLine = "critical"
			if !strings.Contains(items[0].Title, "refused") {
				t.Errorf("title %q", items[0].Title)
			}
		}
		if len(lines) != 1 || lines[0].State != wantLine || lines[0].Summary != "3 incompatible parts" {
			t.Errorf("strict=%v: line %+v", strict, lines)
		}
	}
	ok := New(Deps{Bridges: func(context.Context) []enroll.IngressView { return nil }}).(core.AttentionSource)
	items, lines, _ := ok.Attention(context.Background(), "admin@")
	if len(items) != 0 || lines[0].State != "ok" {
		t.Errorf("all compatible: %+v %+v", items, lines)
	}
}
