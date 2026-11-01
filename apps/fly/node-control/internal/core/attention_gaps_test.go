package core

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAttentionUnknownLevelBecomesInfo(t *testing.T) {
	m := &attMod{stubModule: named("a"), items: []AttentionItem{
		{ID: "bogus", Level: "urgent", Title: "bogus"},
		{ID: "empty", Level: "", Title: "empty"},
		{ID: "upper", Level: "CRITICAL", Title: "upper"}, // levels are exact, not case-folded
		{ID: "ok", Level: "critical", Title: "ok"},
	}}
	var r attResp
	getJSON(t, attHandler(alice, nil, m), "/_attention.json", &r)
	got := map[string]AttentionLevel{}
	for _, it := range r.Items {
		got[it.ID] = it.Level
	}
	want := map[string]AttentionLevel{"bogus": LevelInfo, "empty": LevelInfo, "upper": LevelInfo, "ok": LevelCritical}
	for id, l := range want {
		if got[id] != l {
			t.Errorf("%s: level %q, want %q", id, got[id], l)
		}
	}
	if r.Items[0].ID != "ok" {
		t.Errorf("a coerced item sorts as info, after the critical one: %+v", r.Items)
	}
}

func TestSystemLineUnknownStateBecomesUnknown(t *testing.T) {
	var lines []SystemLine
	for _, s := range []string{"ok", "waiting", "critical", "unknown", "off", "degraded", "", "OK"} {
		lines = append(lines, SystemLine{Name: "n-" + s, State: s})
	}
	var r attResp
	getJSON(t, attHandler(alice, nil, &attMod{stubModule: named("a"), lines: lines}), "/_attention.json", &r)
	if len(r.Systems) != len(lines) {
		t.Fatalf("%+v", r.Systems)
	}
	for i, l := range r.Systems {
		want := lines[i].State
		switch want {
		case "ok", "waiting", "critical", "unknown", "off":
		default:
			want = "unknown"
		}
		if l.State != want {
			t.Errorf("%q came out as %q, want %q", lines[i].State, l.State, want)
		}
	}
}

func TestActivityTextIsScrubbed(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &actM{stubModule: named("mod"), entries: []ActivityEntry{{
		At:     at,
		Actor:  "al‮ice\x00" + strings.Repeat("a", 200),
		Source: "So​urce\x07",
		Action: "node.\x1bstop" + strings.Repeat("x", 200),
		Detail: "de‮tail\n" + strings.Repeat("é", 700),
	}}}
	var r actResp
	getJSON(t, attHandler(alice, nil, m), "/_activity.json", &r)
	if len(r.Entries) != 1 {
		t.Fatalf("%+v", r)
	}
	e := r.Entries[0]
	for name, f := range map[string]struct {
		s   string
		max int
	}{"actor": {e.Actor, 100}, "source": {e.Source, 100}, "action": {e.Action, 100}, "detail": {e.Detail, 500}} {
		if n := len([]rune(f.s)); n > f.max {
			t.Errorf("%s has %d runes, over %d", name, n, f.max)
		}
		for _, c := range f.s {
			if c < 0x20 || c == 0x7f || (c >= 0x200b && c <= 0x200f) || (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069) {
				t.Errorf("%s: control rune %U in %q", name, c, f.s)
			}
		}
	}
	if !strings.HasPrefix(e.Actor, "alice") || e.Source != "Source" || !strings.HasPrefix(e.Action, "node.stop") || !strings.HasPrefix(e.Detail, "detail") {
		t.Errorf("%+v", e)
	}
	if !strings.HasSuffix(e.Detail, "…") || !strings.HasSuffix(e.Actor, "…") {
		t.Errorf("a cut value ends in an ellipsis: %q %q", e.Actor, e.Detail)
	}
}

func TestEndpointsAnswerHEADWithHeadersOnly(t *testing.T) {
	m := &attMod{stubModule: named("a"), items: []AttentionItem{{ID: "x", Level: "info", Title: "x"}}}
	h := attHandler(alice, nil, m)
	for _, p := range []string{"/_attention.json", "/_activity.json", "/_search.json"} {
		get := do(h, "GET", p)
		head := do(h, "HEAD", p)
		if head.Code != 200 {
			t.Fatalf("HEAD %s: %d", p, head.Code)
		}
		if head.Body.Len() != 0 {
			t.Errorf("HEAD %s sent a body: %q", p, head.Body.String())
		}
		if cl := head.Header().Get("Content-Length"); cl != strconv.Itoa(get.Body.Len()) { // updated_at is whole seconds, so the length is stable
			t.Errorf("HEAD %s: Content-Length %q, GET body is %d bytes", p, cl, get.Body.Len())
		}
		if head.Header().Get("Content-Type") != "application/json" || head.Header().Get("Cache-Control") != "no-store" {
			t.Errorf("HEAD %s: %v", p, head.Header())
		}
	}
}

func TestAttentionOmitsZeroFields(t *testing.T) {
	m := &attMod{stubModule: named("a"), items: []AttentionItem{{ID: "x", Level: "info", Title: "x"}}}
	rec := do(attHandler(alice, nil, m), "GET", "/_attention.json")
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Items) != 1 {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
	for _, k := range []string{"detail", "action_label", "action_path", "since", "deadline", "applied_at", "change_id"} {
		if v, ok := raw.Items[0][k]; ok {
			t.Errorf("zero %s is sent as %s; the page reads a missing key as nothing", k, v)
		}
	}
	for _, k := range []string{"id", "source", "level", "title"} {
		if _, ok := raw.Items[0][k]; !ok {
			t.Errorf("%s is missing", k)
		}
	}

	// Set values are sent.
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m = &attMod{stubModule: named("a"), items: []AttentionItem{{ID: "y", Level: "timed", Title: "y", Detail: "d",
		Since: at, Deadline: at, AppliedAt: at, ChangeID: 7, ActionLabel: "Open", ActionPath: "/p"}}}
	rec = do(attHandler(alice, nil, m), "GET", "/_attention.json")
	raw.Items = nil
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil || len(raw.Items) != 1 {
		t.Fatalf("%v %s", err, rec.Body.String())
	}
	for _, k := range []string{"detail", "action_label", "action_path", "since", "deadline", "applied_at", "change_id"} {
		if _, ok := raw.Items[0][k]; !ok {
			t.Errorf("set %s is not sent", k)
		}
	}
}
