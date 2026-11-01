package core

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// memAcks is an in-memory AckStore with the semantics of the real ones (the contract tests in the enroll package).
type memAcks struct {
	mu      sync.Mutex
	acks    map[ackRef]Acknowledgement
	log     []AuditEntry
	readErr error
}

func (s *memAcks) Acknowledgements() ([]Acknowledgement, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.readErr != nil {
		return nil, s.readErr
	}
	out := []Acknowledgement{}
	for _, a := range s.acks {
		out = append(out, a)
	}
	return out, nil
}

func (s *memAcks) PutAcknowledgement(a Acknowledgement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.acks == nil {
		s.acks = map[ackRef]Acknowledgement{}
	}
	s.acks[ackRef{a.Module, a.ID}] = a
	return nil
}

func (s *memAcks) DeleteAcknowledgement(a Acknowledgement) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := ackRef{a.Module, a.ID}
	if cur, ok := s.acks[r]; !ok || cur != a {
		return false, nil
	}
	delete(s.acks, r)
	return true, nil
}

func (s *memAcks) Audit(actor, action, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append([]AuditEntry{{At: ackNow(), Actor: actor, Action: action, Detail: detail}}, s.log...)
	return nil
}

func (s *memAcks) AuditLog(int) ([]AuditEntry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuditEntry(nil), s.log...), nil
}

func (s *memAcks) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.acks)
}

// storeMod is a module that offers the store and reports nothing itself.
type storeMod struct {
	stubModule
	store AckStore
}

func (m *storeMod) AckStore() AckStore { return m.store }

// gatedMod hides itself from everyone but allow.
type gatedMod struct {
	attMod
	allow string
}

func (m *gatedMod) Attention(ctx context.Context, login string) ([]AttentionItem, []SystemLine, error) {
	if login != m.allow {
		return nil, nil, ErrNotVisible
	}
	return m.attMod.Attention(ctx, login)
}

// callerTailnet is a caller that can change between requests.
type callerTailnet struct {
	Tailnet
	mu    sync.Mutex
	login string
	tags  []string
}

func (c *callerTailnet) WhoIs(context.Context, string) (string, []string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.login, c.tags, nil
}

func (c *callerTailnet) as(login string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.login = login
}

type attJSON struct {
	Items   []AttentionItem `json:"items"`
	Systems []SystemLine    `json:"systems"`
	Hidden  bool            `json:"hidden_systems"`
}

// ackFx is one console with a store and a movable clock.
type ackFx struct {
	t     *testing.T
	store *memAcks
	tn    *callerTailnet
	h     http.Handler
	now   time.Time
}

func newAckFx(t *testing.T, mods ...Module) *ackFx {
	t.Helper()
	f := &ackFx{t: t, store: &memAcks{}, tn: &callerTailnet{login: "alice"}, now: time.Unix(1_800_000_000, 0)}
	old := ackNow
	ackNow = func() time.Time { return f.now }
	t.Cleanup(func() { ackNow = old })
	all := append([]Module{&storeMod{stubModule: named("store"), store: f.store}}, mods...)
	f.h = attHandler(f.tn, []NavItem{{Label: "Nodes", Path: "/nodes", Source: "nodes"}}, all...)
	return f
}

func (f *ackFx) read() attJSON {
	f.t.Helper()
	var v attJSON
	getJSON(f.t, f.h, "/_attention.json", &v)
	return v
}

func (f *ackFx) item(id string) AttentionItem {
	f.t.Helper()
	for _, it := range f.read().Items {
		if it.ID == id {
			return it
		}
	}
	f.t.Fatalf("no item %s", id)
	return AttentionItem{}
}

func (f *ackFx) post(path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.ServeHTTP(rec, req)
	return rec
}

func ackBody(id, token string, seconds int64) string {
	b, _ := json.Marshal(ackRequest{ID: id, Token: token, ExpiresInSeconds: seconds})
	return string(b)
}

func (f *ackFx) ack(id string, seconds int64) {
	f.t.Helper()
	if rec := f.post("/_attention/acknowledge", ackBody(id, f.item(id).AckToken, seconds)); rec.Code != 200 {
		f.t.Fatalf("acknowledge %s: %d %s", id, rec.Code, rec.Body.String())
	}
}

func oneItem(level AttentionLevel) *attMod {
	return &attMod{stubModule: named("mod"), items: []AttentionItem{{ID: "a", Source: "nodes", Level: level, Title: "Bridge is out"}},
		lines: []SystemLine{{Name: "Mod", State: "waiting"}}}
}

func TestAckOffWithoutStore(t *testing.T) {
	m := oneItem(LevelCritical)
	h := attHandler(alice, nil, m, &storeMod{stubModule: named("store")}) // a module that has no store right now
	var v attJSON
	getJSON(t, h, "/_attention.json", &v)
	if v.Items[0].AckToken != "" {
		t.Errorf("nothing can be acknowledged: %+v", v.Items[0])
	}
	if rec := do(h, "POST", "/_attention/acknowledge"); rec.Code != 404 && rec.Code != 405 {
		t.Errorf("the route should not exist: %d", rec.Code)
	}
}

func TestAckMarksItemAndKeepsItInTheList(t *testing.T) {
	m := oneItem(LevelCritical)
	f := newAckFx(t, m)
	if it := f.item("a"); it.AckToken == "" || it.Acknowledged != nil {
		t.Fatalf("%+v", it)
	}
	f.ack("a", 3600)
	it := f.item("a")
	if it.Acknowledged == nil || it.Acknowledged.By != "alice" || !it.Acknowledged.At.Equal(f.now) || !it.Acknowledged.Until.Equal(f.now.Add(time.Hour)) {
		t.Fatalf("%+v", it.Acknowledged)
	}
	if len(f.store.log) != 1 || f.store.log[0].Actor != "alice" || f.store.log[0].Action != ActionAcknowledge || f.store.log[0].Detail != "mod/nodes: Bridge is out" {
		t.Errorf("%+v", f.store.log)
	}
	// The JSON says who, when and until, and never what the item's key is.
	raw := do(f.h, "GET", "/_attention.json").Body.String()
	for _, want := range []string{`"acknowledged":{"by":"alice","at":`, `"until":`, `"ack_token":`} {
		if !strings.Contains(raw, want) {
			t.Errorf("missing %s in %s", want, raw)
		}
	}
	if strings.Contains(raw, "Meaning") || strings.Contains(raw, "AckKey") {
		t.Error(raw)
	}
	// Acknowledging twice is refused and leaves the first.
	if rec := f.post("/_attention/acknowledge", ackBody("a", it.AckToken, 0)); rec.Code != 409 {
		t.Errorf("%d", rec.Code)
	}
	if got := f.item("a"); !got.Acknowledged.Until.Equal(f.now.Add(time.Hour)) {
		t.Errorf("%+v", got.Acknowledged)
	}
}

func TestAckEndsWhenTheItemChangesMeaning(t *testing.T) {
	t.Run("level, by default", func(t *testing.T) {
		m := oneItem(LevelCritical)
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.items[0].Level = LevelWaiting
		if it := f.item("a"); it.Acknowledged != nil {
			t.Errorf("%+v", it)
		}
		if f.store.count() != 0 {
			t.Error("it ended, so it must not come back when the level does")
		}
		m.items[0].Level = LevelCritical
		if it := f.item("a"); it.Acknowledged != nil {
			t.Errorf("%+v", it)
		}
	})
	t.Run("key", func(t *testing.T) {
		m := oneItem(LevelCritical)
		m.items[0].AckKey = "a|unreachable"
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.items[0].AckKey = "a|rejected"
		if it := f.item("a"); it.Acknowledged != nil {
			t.Errorf("%+v", it)
		}
	})
	t.Run("not a count or an age in the title", func(t *testing.T) {
		m := oneItem(LevelWaiting)
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.items[0].Title, m.items[0].Detail, m.items[0].Since = "Now 12 of them", "Other words", f.now
		if it := f.item("a"); it.Acknowledged == nil {
			t.Errorf("%+v", it)
		}
	})
	t.Run("into timed", func(t *testing.T) {
		m := oneItem(LevelCritical)
		m.items[0].AckKey = "stays" // even a source that keeps its key cannot park a timed item
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.items[0].Level = LevelTimed
		if it := f.item("a"); it.Acknowledged != nil || it.AckToken != "" {
			t.Errorf("%+v", it)
		}
	})
}

func TestAckExpiry(t *testing.T) {
	m := oneItem(LevelWaiting)
	f := newAckFx(t, m)
	f.ack("a", 60)
	f.now = f.now.Add(59 * time.Second)
	if it := f.item("a"); it.Acknowledged == nil {
		t.Error("before the expiry it holds")
	}
	f.now = f.now.Add(time.Second)
	if it := f.item("a"); it.Acknowledged != nil {
		t.Error("at the expiry it ends")
	}
	if f.store.count() != 0 {
		t.Error("an ended one is removed")
	}
}

func TestAckClearsOnlyWhenTheSourceAnsweredWithoutIt(t *testing.T) {
	t.Run("answered and absent", func(t *testing.T) {
		m := oneItem(LevelWaiting)
		f := newAckFx(t, m)
		f.ack("a", 0)
		items := m.items
		m.items = nil
		f.read()
		if f.store.count() != 0 {
			t.Error("it cleared")
		}
		m.items = items
		if it := f.item("a"); it.Acknowledged != nil {
			t.Error("the same item coming back later is new")
		}
	})
	t.Run("errored", func(t *testing.T) {
		m := oneItem(LevelWaiting)
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.err = errors.New("no answer")
		f.read()
		if f.store.count() != 1 {
			t.Fatal("a source that errored ended it")
		}
		m.err = nil
		if it := f.item("a"); it.Acknowledged == nil {
			t.Error("it holds again when the source answers")
		}
	})
	t.Run("hidden from this caller", func(t *testing.T) {
		m := &gatedMod{attMod: *oneItem(LevelWaiting), allow: "alice"}
		f := newAckFx(t, m)
		f.ack("a", 0)
		f.tn.as("bob")
		if v := f.read(); !v.Hidden || len(v.Items) != 0 {
			t.Fatalf("%+v", v)
		}
		if f.store.count() != 1 {
			t.Fatal("a hidden source ended it")
		}
	})
	t.Run("the module could not fully look", func(t *testing.T) {
		m := oneItem(LevelWaiting)
		f := newAckFx(t, m)
		f.ack("a", 0)
		m.items, m.lines = nil, []SystemLine{{Name: "Mod", State: "unknown"}}
		f.read()
		if f.store.count() != 1 {
			t.Fatal("an unknown line ended it")
		}
	})
	t.Run("a store that cannot be read ends nothing and offers nothing", func(t *testing.T) {
		m := oneItem(LevelWaiting)
		f := newAckFx(t, m)
		f.ack("a", 0)
		f.store.mu.Lock()
		f.store.readErr = errors.New("down")
		f.store.mu.Unlock()
		m.items = append(m.items, AttentionItem{ID: "b", Level: LevelWaiting, Title: "B"})
		for _, it := range f.read().Items {
			if it.Acknowledged != nil || it.AckToken != "" {
				t.Errorf("%+v", it)
			}
		}
		f.store.mu.Lock()
		f.store.readErr = nil
		f.store.mu.Unlock()
		if f.store.count() != 1 {
			t.Fatal("it ended")
		}
	})
}

func TestAckTwoModulesWithTheSameItemID(t *testing.T) {
	a, b := oneItem(LevelWaiting), oneItem(LevelWaiting)
	b.stubModule = named("other")
	f := newAckFx(t, a, b)
	v := f.read()
	if v.Items[0].AckToken == v.Items[1].AckToken {
		t.Fatal("the token says which module")
	}
	if rec := f.post("/_attention/acknowledge", ackBody("a", v.Items[1].AckToken, 0)); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	acked := 0
	for _, it := range f.read().Items {
		if it.Acknowledged != nil {
			acked++
		}
	}
	if acked != 1 {
		t.Errorf("%d acknowledged", acked)
	}
}

func TestAckRefused(t *testing.T) {
	timed := &attMod{stubModule: named("policy"), items: []AttentionItem{{ID: "t", Level: LevelTimed, Title: "Reverts soon", Deadline: time.Now().Add(time.Minute)}}}
	broken := &attMod{stubModule: named("broken"), err: errors.New("x")}
	gated := &gatedMod{attMod: *oneItem(LevelWaiting), allow: "alice"}
	f := newAckFx(t, timed, broken, gated)
	v := f.read()
	byID := map[string]AttentionItem{}
	for _, it := range v.Items {
		byID[it.ID] = it
	}
	if byID["t"].AckToken != "" || byID["unknown-broken"].AckToken != "" {
		t.Fatalf("a timed item and an unknown source offer nothing: %+v", v.Items)
	}
	if byID["a"].AckToken == "" {
		t.Fatal("the other item can be acknowledged")
	}
	for _, id := range []string{"t", "unknown-broken"} {
		if rec := f.post("/_attention/acknowledge", ackBody(id, "guess", 0)); rec.Code != 409 {
			t.Errorf("%s: %d %s", id, rec.Code, rec.Body.String())
		}
		if rec := f.post("/_attention/acknowledge", ackBody(id, ackMeaning("policy", byID[id]), 0)); rec.Code != 409 {
			t.Errorf("%s with a computed token: %d", id, rec.Code)
		}
	}
	token := byID["a"].AckToken
	for _, tc := range []struct {
		name, body string
		code       int
	}{
		{"unknown item", ackBody("nope", token, 0), 404},
		{"stale token", ackBody("a", "old", 0), 409},
		{"no id", `{"ack_token":"x"}`, 422},
		{"no token", `{"id":"a"}`, 422},
		{"not json", `{`, 422},
		{"unknown field", `{"id":"a","ack_token":"x","who":"bob"}`, 422},
		{"negative expiry", ackBody("a", token, -1), 422},
		{"expiry too long", ackBody("a", token, 91*86400), 422},
		{"two values", ackBody("a", token, 0) + ackBody("a", token, 0), 422},
	} {
		if rec := f.post("/_attention/acknowledge", tc.body); rec.Code != tc.code {
			t.Errorf("%s: %d %s", tc.name, rec.Code, rec.Body.String())
		}
	}
	if f.store.count() != 0 {
		t.Fatal("something was stored")
	}

	// Someone who cannot see the item gets what anyone gets for an item that is not there.
	f.tn.as("bob")
	rec := f.post("/_attention/acknowledge", ackBody("a", token, 0))
	if rec.Code != 404 {
		t.Errorf("%d", rec.Code)
	}
	nope := f.post("/_attention/acknowledge", ackBody("nope", token, 0))
	if rec.Body.String() != nope.Body.String() {
		t.Errorf("the answers differ: %s / %s", rec.Body.String(), nope.Body.String())
	}
	f.tn.as("alice")

	// A form cannot post here, and a tagged node or an unnamed caller may not.
	req := httptest.NewRequest("POST", "/_attention/acknowledge", strings.NewReader(ackBody("a", token, 0)))
	req.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	f.h.ServeHTTP(w, req)
	if w.Code != 422 {
		t.Errorf("text/plain: %d", w.Code)
	}
	f.tn.mu.Lock()
	f.tn.tags = []string{"tag:x"}
	f.tn.mu.Unlock()
	if rec := f.post("/_attention/acknowledge", ackBody("a", token, 0)); rec.Code != 403 {
		t.Errorf("tagged: %d", rec.Code)
	}
	f.tn.mu.Lock()
	f.tn.tags = nil
	f.tn.mu.Unlock()
	f.tn.as("")
	if rec := f.post("/_attention/acknowledge", ackBody("a", token, 0)); rec.Code != 403 {
		t.Errorf("no login: %d", rec.Code)
	}
	if f.store.count() != 0 {
		t.Fatal("something was stored")
	}
}

func TestUnacknowledge(t *testing.T) {
	m := oneItem(LevelCritical)
	f := newAckFx(t, m)
	token := f.item("a").AckToken
	if rec := f.post("/_attention/unacknowledge", ackBody("a", token, 0)); rec.Code != 404 {
		t.Errorf("not acknowledged: %d", rec.Code)
	}
	f.ack("a", 0)
	if rec := f.post("/_attention/unacknowledge", ackBody("a", token, 0)); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if it := f.item("a"); it.Acknowledged != nil {
		t.Errorf("%+v", it)
	}
	if len(f.store.log) != 2 || f.store.log[0].Action != ActionUnacknowledge || f.store.log[0].Actor != "alice" {
		t.Errorf("%+v", f.store.log)
	}
	// A failed undo is not recorded.
	if rec := f.post("/_attention/unacknowledge", ackBody("a", token, 0)); rec.Code != 404 {
		t.Errorf("%d", rec.Code)
	}
	if len(f.store.log) != 2 {
		t.Errorf("%+v", f.store.log)
	}
}

func TestAckLowersSystemLines(t *testing.T) {
	line := func(f *ackFx) SystemLine {
		f.t.Helper()
		for _, l := range f.read().Systems {
			if l.Name == "Mod" {
				return l
			}
		}
		f.t.Fatal("no line")
		return SystemLine{}
	}
	two := func() *attMod {
		return &attMod{stubModule: named("mod"), items: []AttentionItem{
			{ID: "crit", Level: LevelCritical, Title: "C"}, {ID: "wait", Level: LevelWaiting, Title: "W"}},
			lines: []SystemLine{{Name: "Mod", State: "critical", Summary: "2 bad"}}}
	}
	f := newAckFx(t, two())
	if l := line(f); l.State != "critical" || l.Summary != "2 bad" {
		t.Errorf("nothing acknowledged: %+v", l)
	}
	f.ack("wait", 0)
	if l := line(f); l.State != "critical" || l.Summary != "2 bad. 1 acknowledged" {
		t.Errorf("the critical one still stands: %+v", l)
	}
	f.ack("crit", 0)
	if l := line(f); l.State != "ok" || l.Summary != "2 bad. 2 acknowledged" {
		t.Errorf("all acknowledged: %+v", l)
	}

	f = newAckFx(t, two())
	f.ack("crit", 0)
	if l := line(f); l.State != "waiting" {
		t.Errorf("the waiting one still stands: %+v", l)
	}

	t.Run("a waiting line stays waiting under a critical item", func(t *testing.T) {
		m := two()
		m.lines[0].State = "waiting"
		f := newAckFx(t, m)
		f.ack("wait", 0)
		if l := line(f); l.State != "waiting" {
			t.Errorf("%+v", l)
		}
	})
	t.Run("never raised, and unknown stays", func(t *testing.T) {
		m := oneItem(LevelCritical)
		m.lines = []SystemLine{{Name: "Mod", State: "ok", Summary: "fine"}, {Name: "Other", State: "unknown", Summary: "?"}}
		f := newAckFx(t, m)
		f.ack("a", 0)
		for _, l := range f.read().Systems {
			if (l.Name == "Mod" && (l.State != "ok" || l.Summary != "fine")) || (l.Name == "Other" && l.State != "unknown") {
				t.Errorf("%+v", l)
			}
		}
	})
	t.Run("an acknowledged notice changes no line", func(t *testing.T) {
		m := &attMod{stubModule: named("mod"), items: []AttentionItem{{ID: "n", Level: LevelInfo, Title: "N"}},
			lines: []SystemLine{{Name: "Mod", State: "waiting", Summary: "s"}}}
		f := newAckFx(t, m)
		f.ack("n", 0)
		if l := line(f); l.State != "waiting" || l.Summary != "s" {
			t.Errorf("%+v", l)
		}
	})
}

func TestAckActivityFollowsWhoCanSeeTheModule(t *testing.T) {
	type actJSON struct {
		Entries []ActivityEntry `json:"entries"`
		Partial bool            `json:"partial"`
		Hidden  bool            `json:"hidden_systems"`
	}
	gated := &gatedMod{attMod: attMod{stubModule: named("secret"), items: []AttentionItem{{ID: "s", Source: "secret", Level: LevelWaiting, Title: "Secret thing"}}}, allow: "alice"}
	open := oneItem(LevelWaiting)
	open.items[0].Title = "Open thing"
	f := newAckFx(t, gated, open)
	f.ack("s", 0)
	f.ack("a", 0)
	read := func(path string) actJSON {
		t.Helper()
		var v actJSON
		getJSON(t, f.h, path, &v)
		return v
	}

	v := read("/_activity.json")
	if len(v.Entries) != 2 || v.Hidden {
		t.Fatalf("alice sees both: %+v", v)
	}
	if e := v.Entries[1]; e.Actor != "alice" || e.Action != ActionAcknowledge || (e.Detail != "Secret thing" && e.Detail != "Open thing") {
		t.Errorf("the detail is the title alone: %+v", e)
	}
	bySource := map[string]string{}
	for _, e := range v.Entries {
		bySource[e.Detail] = e.Source
	}
	if bySource["Open thing"] != "Nodes" || bySource["Secret thing"] != "secret" {
		t.Errorf("the source is the page of the item, when it has one: %+v", bySource)
	}
	if got := read("/_activity.json?source=Nodes"); len(got.Entries) != 1 || got.Entries[0].Detail != "Open thing" {
		t.Errorf("filtered by source: %+v", got)
	}

	f.tn.as("bob")
	v = read("/_activity.json")
	if len(v.Entries) != 1 || v.Entries[0].Detail != "Open thing" || !v.Hidden {
		t.Fatalf("bob sees only what he can see: %+v", v)
	}
	if raw := do(f.h, "GET", "/_activity.json").Body.String(); strings.Contains(raw, "Secret thing") {
		t.Fatal(raw)
	}

	// A source that cannot say who may see it hides its entries, and the page says the list may be incomplete.
	f.tn.as("alice")
	gated.err = errors.New("down")
	v = read("/_activity.json")
	if len(v.Entries) != 1 || !v.Partial {
		t.Fatalf("%+v", v)
	}

	// A malformed entry is not shown, and an entry of a module that is gone neither.
	gated.err = nil
	f.store.Audit("x", ActionAcknowledge, "no separator")
	f.store.Audit("x", ActionAcknowledge, "gone/gone: Old")
	if v = read("/_activity.json"); len(v.Entries) != 2 {
		t.Errorf("%+v", v)
	}
}

func TestAckConcurrentReadsAndWrites(t *testing.T) {
	m := oneItem(LevelWaiting)
	f := newAckFx(t, m)
	token := f.item("a").AckToken
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(3)
		go func() { defer wg.Done(); do(f.h, "GET", "/_attention.json") }()
		go func() { defer wg.Done(); f.post("/_attention/acknowledge", ackBody("a", token, 0)) }()
		go func() { defer wg.Done(); f.post("/_attention/unacknowledge", ackBody("a", token, 0)) }()
	}
	wg.Wait()
}
