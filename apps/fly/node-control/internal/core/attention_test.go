package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type attMod struct {
	stubModule
	items []AttentionItem
	lines []SystemLine
	err   error
	block bool
	boom  bool
}

func (m *attMod) Attention(ctx context.Context, _ string) ([]AttentionItem, []SystemLine, error) {
	if m.boom {
		panic("boom")
	}
	if m.block {
		<-ctx.Done()
		select {} // ignore cancellation: a source that never returns
	}
	return m.items, m.lines, m.err
}

type searchMod struct {
	stubModule
	items []SearchItem
	err   error
}

func (m *searchMod) Search(context.Context, string) ([]SearchItem, error) { return m.items, m.err }

func attHandler(tn Tailnet, nav []NavItem, mods ...Module) http.Handler {
	mux := http.NewServeMux()
	registerAttention(mux, mods, nav, tn)
	return mux
}

var alice = whoTailnet{login: "alice"}

func named(n string) stubModule { return stubModule{name: n} }

func getJSON(t *testing.T, h http.Handler, path string, v any) *httptest.ResponseRecorder {
	t.Helper()
	rec := do(h, "GET", path)
	if rec.Code != 200 {
		t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("%s: %v: %s", path, err, rec.Body.String())
	}
	return rec
}

type attResp struct {
	Items   []AttentionItem `json:"items"`
	Systems []SystemLine    `json:"systems"`
	Hidden  bool            `json:"hidden_systems"`
	Updated string          `json:"updated_at"`
}

func TestAttentionOrder(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := &attMod{stubModule: named("a"), items: []AttentionItem{
		{ID: "i1", Level: "info", Title: "i1", Since: t0},
		{ID: "w2", Level: "waiting", Title: "w2", Since: t0.Add(2 * time.Hour)},
		{ID: "w1", Level: "waiting", Title: "w1", Since: t0.Add(time.Hour)},
		{ID: "c1", Level: "critical", Title: "c1", Since: t0},
		{ID: "t1", Level: "timed", Title: "t1", Since: t0, Deadline: t0.Add(time.Hour)},
		{ID: "w0", Level: "waiting", Title: "w0"},
	}}
	var r attResp
	getJSON(t, attHandler(alice, nil, m), "/_attention.json", &r)
	var ids []string
	for _, it := range r.Items {
		ids = append(ids, it.ID)
	}
	if got := strings.Join(ids, ","); got != "t1,c1,w1,w2,w0,i1" {
		t.Fatal(got)
	}
	if r.Updated == "" {
		t.Fatal("no updated_at")
	}
}

func TestAttentionSourceTimeoutBecomesUnknown(t *testing.T) {
	old := attentionTimeout
	attentionTimeout = 100 * time.Millisecond
	defer func() { attentionTimeout = old }()
	slow := &attMod{stubModule: named("slowmod"), block: true}
	ok := &attMod{stubModule: named("fine"), items: []AttentionItem{{ID: "x", Level: "info", Title: "x"}},
		lines: []SystemLine{{Name: "Fine", State: "ok"}}}
	start := time.Now()
	var r attResp
	getJSON(t, attHandler(alice, []NavItem{{Label: "Slow", Path: "/slow", Source: "slowmod"}}, slow, ok), "/_attention.json", &r)
	if time.Since(start) > attentionTimeout+time.Second {
		t.Fatal("took too long")
	}
	var crit, fine, unknown int
	for _, it := range r.Items {
		if it.Level == "critical" && strings.Contains(it.Title, "slowmod") && it.Source == "slowmod" {
			crit++
		}
		if it.ID == "x" {
			fine++
		}
	}
	for _, s := range r.Systems {
		if s.State == "unknown" && s.Name == "slowmod" && s.Path == "/slow" {
			unknown++
		}
	}
	if crit != 1 || fine != 1 || unknown != 1 || len(r.Systems) != 2 {
		t.Fatalf("%d %d %d %+v", crit, fine, unknown, r)
	}
}

func TestAttentionSourceErrorBecomesUnknown(t *testing.T) {
	for name, m := range map[string]*attMod{
		"error": {stubModule: named("broken"), err: errors.New("secret detail")},
		"panic": {stubModule: named("broken"), boom: true},
	} {
		t.Run(name, func(t *testing.T) {
			var r attResp
			rec := getJSON(t, attHandler(alice, nil, m), "/_attention.json", &r)
			if len(r.Items) != 1 || r.Items[0].Level != "critical" || !strings.Contains(r.Items[0].Title, "broken") ||
				len(r.Systems) != 1 || r.Systems[0].State != "unknown" {
				t.Fatalf("%+v", r)
			}
			if strings.Contains(rec.Body.String(), "secret detail") {
				t.Fatal("error text leaked")
			}
		})
	}
}

func TestAttentionNotVisibleSetsHidden(t *testing.T) {
	hid := &attMod{stubModule: named("hid"), err: ErrNotVisible, items: []AttentionItem{{ID: "h", Level: "info", Title: "h"}},
		lines: []SystemLine{{Name: "Hid", State: "ok"}}}
	vis := &attMod{stubModule: named("vis"), items: []AttentionItem{{ID: "v", Level: "info", Title: "v"}}}
	var r attResp
	getJSON(t, attHandler(alice, nil, hid, vis), "/_attention.json", &r)
	if !r.Hidden || len(r.Items) != 1 || r.Items[0].ID != "v" || len(r.Systems) != 0 {
		t.Fatalf("%+v", r)
	}
	r = attResp{}
	getJSON(t, attHandler(alice, nil, vis), "/_attention.json", &r)
	if r.Hidden {
		t.Fatal("hidden without cause")
	}
}

func TestAttentionPartialUserAndRefusals(t *testing.T) {
	m := &attMod{stubModule: named("a")}
	for name, tn := range map[string]Tailnet{
		"tagged": whoTailnet{login: "alice", tags: []string{"tag:x"}},
		"error":  whoTailnet{err: errors.New("no")},
		"nil":    nil,
	} {
		for _, p := range []string{"/_attention.json", "/_activity.json", "/_search.json"} {
			rec := do(attHandler(tn, nil, m), "GET", p)
			if rec.Code != 403 || rec.Body.Len() != 0 {
				t.Fatalf("%s %s: %d %q", name, p, rec.Code, rec.Body.String())
			}
		}
	}
}

func TestAttentionTextIsScrubbed(t *testing.T) {
	m := &attMod{stubModule: named("a"), items: []AttentionItem{{
		ID: "x", Level: "info",
		Title:  "ab‮cd\x00ef\n" + strings.Repeat("t", 300),
		Detail: "d​\x07" + strings.Repeat("é", 700),
	}}, lines: []SystemLine{{Name: "N‮", Summary: "s\x01" + strings.Repeat("s", 600), State: "ok"}}}
	var r attResp
	getJSON(t, attHandler(alice, nil, m), "/_attention.json", &r)
	it := r.Items[0]
	if n := len([]rune(it.Title)); n > 200 || n < 150 {
		t.Fatal(n)
	}
	if n := len([]rune(it.Detail)); n > 500 || n < 400 {
		t.Fatal(n)
	}
	for _, s := range []string{it.Title, it.Detail, r.Systems[0].Name, r.Systems[0].Summary} {
		for _, c := range s {
			if c < 0x20 || c == 0x7f || (c >= 0x200b && c <= 0x200f) || (c >= 0x202a && c <= 0x202e) || (c >= 0x2066 && c <= 0x2069) {
				t.Fatalf("control rune %U in %q", c, s)
			}
		}
	}
	if !strings.HasPrefix(it.Title, "abcdef") {
		t.Fatal(it.Title)
	}
}

func TestAttentionRejectsUnsafeActionPath(t *testing.T) {
	good := map[string]bool{
		"/nodes": true, "/policy?change=12": true, "/a/b?x=1&y=2": true,
		"//evil.example": false, "https://evil.example/": false, "javascript:alert(1)": false,
		"/a b": false, "/a#frag": false, "/a?x=\"": false, "/a\\b": false, "nodes": false, "/a?x=\x01": false,
	}
	var items []AttentionItem
	for p := range good {
		items = append(items, AttentionItem{ID: p, Level: "info", Title: "t", ActionLabel: "Open", ActionPath: p})
	}
	var r attResp
	getJSON(t, attHandler(alice, nil, &attMod{stubModule: named("a"), items: items}), "/_attention.json", &r)
	for _, it := range r.Items {
		if got := it.ActionPath != ""; got != good[it.ID] {
			t.Errorf("%q kept=%v", it.ID, got)
		}
		if it.ActionPath == "" && it.ActionLabel != "" {
			t.Errorf("%q label without path", it.ID)
		}
	}
}

type actResp struct {
	Entries []ActivityEntry `json:"entries"`
	Partial bool            `json:"partial"`
	Hidden  bool            `json:"hidden_systems"`
}

func TestActivityMergesNewestFirst(t *testing.T) {
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a := &actM{stubModule: named("a"), entries: []ActivityEntry{
		{At: t0.Add(1 * time.Hour), Action: "a1", Source: "a"}, {At: t0.Add(3 * time.Hour), Action: "a3", Source: "a"}}}
	b := &actM{stubModule: named("b"), entries: []ActivityEntry{{At: t0.Add(2 * time.Hour), Action: "b2"}}}
	bad := &actM{stubModule: named("bad"), err: errors.New("x")}
	h := attHandler(alice, nil, a, b, bad)
	var r actResp
	getJSON(t, h, "/_activity.json", &r)
	if len(r.Entries) != 3 || r.Entries[0].Action != "a3" || r.Entries[1].Action != "b2" || r.Entries[2].Action != "a1" || !r.Partial {
		t.Fatalf("%+v", r)
	}
	if r.Entries[1].Source != "b" {
		t.Fatal("source not defaulted to module name")
	}
	if a.limit != 200 {
		t.Fatal(a.limit)
	}
	r = actResp{}
	getJSON(t, h, "/_activity.json?limit=2", &r)
	if len(r.Entries) != 2 || a.limit != 2 {
		t.Fatalf("%+v %d", r, a.limit)
	}
	getJSON(t, h, "/_activity.json?limit=9999", &r)
	if a.limit != 500 {
		t.Fatal(a.limit)
	}
	getJSON(t, h, "/_activity.json?limit=junk", &r)
	if a.limit != 200 {
		t.Fatal(a.limit)
	}
	r = actResp{}
	getJSON(t, h, "/_activity.json?source=b", &r)
	if len(r.Entries) != 1 || r.Entries[0].Action != "b2" {
		t.Fatalf("%+v", r)
	}
	hid := &actM{stubModule: named("hid"), err: ErrNotVisible}
	r = actResp{}
	getJSON(t, attHandler(alice, nil, a, hid), "/_activity.json", &r)
	if r.Partial || !r.Hidden {
		t.Fatalf("%+v", r)
	}
}

type actM struct {
	stubModule
	entries []ActivityEntry
	err     error
	limit   int
}

func (m *actM) Activity(_ context.Context, _ string, limit int) ([]ActivityEntry, error) {
	m.limit = limit
	return m.entries, m.err
}

func TestSearchMergesAndNeverIncludesFailures(t *testing.T) {
	a := &searchMod{stubModule: named("a"), items: []SearchItem{{Label: "Alpha", Kind: "node", Path: "/n/a"}, {Label: "Evil", Kind: "node", Path: "//evil.example"}}}
	b := &searchMod{stubModule: named("b"), items: []SearchItem{{Label: "Beta‮", Kind: "key", Path: "/k/b"}}}
	bad := &searchMod{stubModule: named("bad"), err: errors.New("x"), items: []SearchItem{{Label: "Leak", Kind: "k", Path: "/leak"}}}
	hid := &searchMod{stubModule: named("hid"), err: ErrNotVisible, items: []SearchItem{{Label: "Hid", Kind: "k", Path: "/hid"}}}
	var r struct {
		Items []SearchItem `json:"items"`
	}
	getJSON(t, attHandler(alice, nil, a, b, bad, hid), "/_search.json", &r)
	if len(r.Items) != 2 || r.Items[0].Label != "Alpha" || r.Items[1].Label != "Beta" {
		t.Fatalf("%+v", r)
	}
}

func TestEndpointsNoStoreJSON(t *testing.T) {
	h := NavMiddleware(nil, nil, alice, attHandler(alice, []NavItem{{Label: "A", Path: "/a"}, {Label: "B", Path: "/b"}}, &attMod{stubModule: named("a")}))
	for _, p := range []string{"/_attention.json", "/_activity.json", "/_search.json"} {
		rec := do(h, "GET", p)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" ||
			!strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") || strings.Contains(rec.Body.String(), "data-nc-nav") {
			t.Fatalf("%s: %d %v %s", p, rec.Code, rec.Header(), rec.Body.String())
		}
	}
	if rec := do(h, "POST", "/_search.json"); rec.Code == 200 {
		t.Fatal("POST allowed")
	}
}

// gatedAct shows its entries to alice only, as the funnel, policy and tasks modules do.
type gatedAct struct {
	stubModule
	entries []ActivityEntry
}

func (m *gatedAct) Activity(_ context.Context, login string, _ int) ([]ActivityEntry, error) {
	if login != "alice" {
		return nil, ErrNotVisible
	}
	return m.entries, nil
}

func TestActivityRespectsVisibility(t *testing.T) {
	open := &actM{stubModule: named("open"), entries: []ActivityEntry{{At: time.Now(), Action: "node.stop", Source: "Nodes"}}}
	gated := &gatedAct{stubModule: named("gated"), entries: []ActivityEntry{{At: time.Now(), Action: "key.mint", Source: "Funnel"}}}
	var r actResp
	getJSON(t, attHandler(alice, nil, open, gated), "/_activity.json", &r)
	if len(r.Entries) != 2 || r.Hidden {
		t.Fatalf("alice sees both: %+v", r)
	}
	r = actResp{}
	getJSON(t, attHandler(whoTailnet{login: "bob"}, nil, open, gated), "/_activity.json", &r)
	if len(r.Entries) != 1 || r.Entries[0].Action != "node.stop" || !r.Hidden || r.Partial {
		t.Fatalf("bob sees the open source only, and is told something is hidden: %+v", r)
	}
}

func TestActivitySourceFilterAsksOnlyThatSource(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	nav := []NavItem{{Label: "Nodes", Path: "/nodes", Source: "nodes"}, {Label: "Policy", Path: "/policy", Source: "policy"}, {Label: "Funnel", Path: "/funnel", Source: "funnel"}}
	nodes := &actM{stubModule: named("nodes"), entries: []ActivityEntry{{At: at, Action: "node.stop", Source: "Nodes"}}}
	broken := &actM{stubModule: named("funnel"), err: errors.New("down")}
	hidden := &actM{stubModule: named("policy"), err: ErrNotVisible}
	h := attHandler(alice, nav, nodes, broken, hidden)

	var r actResp
	getJSON(t, h, "/_activity.json?source=Nodes", &r)
	if len(r.Entries) != 1 || r.Partial || r.Hidden {
		t.Fatalf("a source that was not asked for says nothing about this list: %+v", r)
	}
	r = actResp{}
	getJSON(t, h, "/_activity.json?source=Funnel", &r)
	if len(r.Entries) != 0 || !r.Partial {
		t.Fatalf("the chosen source failing does: %+v", r)
	}
	r = actResp{}
	getJSON(t, h, "/_activity.json", &r)
	if len(r.Entries) != 1 || !r.Partial || !r.Hidden {
		t.Fatalf("with no filter every source counts: %+v", r)
	}
	r = actResp{}
	getJSON(t, h, "/_activity.json?source=Other", &r)
	if len(r.Entries) != 0 || !r.Partial {
		t.Fatalf("a name no page owns still asks everyone, as it could be any module's: %+v", r)
	}
}

func TestAttentionThatCannotBeEncodedIsAnError(t *testing.T) {
	m := &attMod{stubModule: named("a"), items: []AttentionItem{{ID: "x", Level: LevelWaiting, Title: "x", Since: time.Date(12026, 1, 1, 0, 0, 0, 0, time.UTC)}}}
	rec := do(attHandler(alice, nil, m), "GET", "/_attention.json")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("an empty 200 reads as nothing to report: %d %q", rec.Code, rec.Body.String())
	}
}

func TestSearchIsCapped(t *testing.T) {
	many := func(prefix string, n int) []SearchItem {
		out := make([]SearchItem, n)
		for i := range out {
			out[i] = SearchItem{Label: prefix, Kind: "k", Path: "/p"}
		}
		return out
	}
	flood := &searchMod{stubModule: named("flood"), items: many("flood", maxSearchPerSource+50)}
	quiet := &searchMod{stubModule: named("quiet"), items: many("quiet", 1)}
	var r struct {
		Items []SearchItem `json:"items"`
	}
	getJSON(t, attHandler(alice, nil, flood, quiet), "/_search.json", &r)
	if len(r.Items) != maxSearchPerSource+1 || r.Items[len(r.Items)-1].Label != "quiet" {
		t.Fatalf("one flooding source: %d items, last %+v", len(r.Items), r.Items[len(r.Items)-1])
	}

	var mods []Module
	for i := 0; i <= maxSearchItems/maxSearchPerSource; i++ {
		mods = append(mods, &searchMod{stubModule: named("m"), items: many("x", maxSearchPerSource)})
	}
	getJSON(t, attHandler(alice, nil, mods...), "/_search.json", &r)
	if len(r.Items) != maxSearchItems {
		t.Fatalf("many sources: %d items, want %d", len(r.Items), maxSearchItems)
	}
}

func TestEmptyAttentionIDIsDefaulted(t *testing.T) {
	m := &attMod{stubModule: named("mod"), items: []AttentionItem{
		{Level: "info", Title: "one"},
		{Level: "info", Title: "two"},
		{ID: "​", Level: "info", Title: "only a format character"},
		{ID: "given", Level: "info", Title: "three"},
	}}
	var first, again attResp
	h := attHandler(alice, nil, m)
	getJSON(t, h, "/_attention.json", &first)
	getJSON(t, h, "/_attention.json", &again)
	seen := map[string]bool{}
	for i, it := range first.Items {
		if it.ID == "" || seen[it.ID] {
			t.Errorf("item %d: id %q is empty or repeated", i, it.ID)
		}
		seen[it.ID] = true
		if it.ID != again.Items[i].ID {
			t.Errorf("item %d: id changed between polls: %q, %q", i, it.ID, again.Items[i].ID)
		}
	}
	if !seen["given"] {
		t.Errorf("an id the module gave was replaced: %+v", first.Items)
	}
}
