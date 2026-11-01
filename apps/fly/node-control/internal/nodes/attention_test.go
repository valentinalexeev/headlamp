package nodes

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/headscale"
)

var _ core.AttentionSource = (*Module)(nil)

func attentionConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(`{"nodes":{
		"exit-a":{"provider":"yandex","yandex":{"instance_id":"X"}},
		"exit-b":{"provider":"oci"},
		"exit-c":{"provider":"yandex","yandex":{"instance_id":"Y"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func hsNode(name string, online bool) headscale.Node {
	seen := time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC)
	return headscale.Node{Name: name, GivenName: name, Online: online, LastSeen: &seen}
}

// statuses maps a node id to the provider status word; a missing id is an error.
func attentionModule(t *testing.T, src headscale.NodeSource, statuses map[string]string, calls *atomic.Int32) *Module {
	t.Helper()
	p := &fakeProvider{stat: func(_ context.Context, n Target) (Status, error) {
		calls.Add(1)
		s, ok := statuses[n.ID]
		if !ok {
			return nil, errors.New("provider down")
		}
		return Status{"status": s}, nil
	}}
	m := New(attentionConfig(t), src, map[string]Provider{"yandex": p, "oci": p}).(*Module)
	return m
}

func TestNodesAttention(t *testing.T) {
	ctx := context.Background()
	healthy := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", true), hsNode("exit-c", false)}}

	t.Run("a running node that is offline on the tailnet is critical", func(t *testing.T) {
		var calls atomic.Int32
		src := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", false), hsNode("exit-c", false)}}
		m := attentionModule(t, src, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		it := items[0]
		if it.Level != core.LevelCritical || it.Source != "nodes" || it.ActionPath != "/nodes?sel=exit-b" ||
			!strings.Contains(it.Title, "exit-b") || it.Since.IsZero() {
			t.Errorf("%+v", it)
		}
		if len(lines) != 1 || lines[0].Name != "Nodes" || lines[0].Path != "/nodes" || lines[0].State != "critical" ||
			!strings.Contains(lines[0].Summary, "1 of 3 impaired") || !strings.Contains(lines[0].Summary, "1 stopped") {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("a stopped node and a healthy one raise nothing", func(t *testing.T) {
		var calls atomic.Int32
		m := attentionModule(t, healthy, map[string]string{"exit-a": "RUNNING", "exit-b": "STARTED", "exit-c": "STOPPED"}, &calls)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
		if lines[0].State != "ok" || !strings.Contains(lines[0].Summary, "1 stopped") {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("an unmanaged node with no expectation raises nothing", func(t *testing.T) {
		var calls atomic.Int32
		offline := hsNode("relay-1", false)
		offline.Tags = []string{"tag:exit"}
		src := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", true), hsNode("exit-c", false), offline}}
		m := attentionModule(t, src, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
		items, _, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
	})

	t.Run("a re-registered node is online when any registration is", func(t *testing.T) {
		var calls atomic.Int32
		src := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", false), hsNode("exit-b-1", true), hsNode("exit-c", false)}}
		m := attentionModule(t, src, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
		if items, _, _ := m.Attention(ctx, "alice@"); len(items) != 0 {
			t.Fatalf("%+v", items)
		}
	})

	t.Run("an unavailable headscale gives one item and one unknown line", func(t *testing.T) {
		var calls atomic.Int32
		m := attentionModule(t, fakeSrc{err: errors.New("headscale down")}, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "RUNNING"}, &calls)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 1 {
			t.Fatalf("%+v %v", items, err)
		}
		if items[0].Title != "Tailnet status unavailable" || items[0].ActionPath != "/nodes" || items[0].Level != core.LevelWaiting {
			t.Errorf("%+v", items[0])
		}
		if len(lines) != 1 || lines[0].State != "unknown" {
			t.Errorf("lines: %+v", lines)
		}
	})

	t.Run("no headscale configured is not a failure", func(t *testing.T) {
		var calls atomic.Int32
		m := attentionModule(t, nil, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 || len(lines) != 1 || lines[0].State == "unknown" {
			t.Fatalf("%+v %+v %v", items, lines, err)
		}
	})

	t.Run("a provider that cannot answer makes the line unknown without an item", func(t *testing.T) {
		var calls atomic.Int32
		m := attentionModule(t, healthy, map[string]string{"exit-a": "RUNNING", "exit-c": "STOPPED"}, &calls)
		items, lines, err := m.Attention(ctx, "alice@")
		if err != nil || len(items) != 0 {
			t.Fatalf("%+v %v", items, err)
		}
		if lines[0].State != "unknown" || !strings.Contains(lines[0].Summary, "1 could not be checked") {
			t.Errorf("lines: %+v", lines)
		}
	})
}

func TestNodesAttentionCachesProviderCalls(t *testing.T) {
	var calls atomic.Int32
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	src := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", true), hsNode("exit-c", false)}}
	m := attentionModule(t, src, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
	m.now = func() time.Time { return now }
	for i := 0; i < 3; i++ {
		if _, _, err := m.Attention(context.Background(), "alice@"); err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("polling three times called the providers %d times, want 3 (once per node)", got)
	}
	now = now.Add(21 * time.Second)
	m.Attention(context.Background(), "alice@")
	if got := calls.Load(); got != 6 {
		t.Fatalf("after the cache expired: %d calls, want 6", got)
	}
}

// What /status has just read is what Attention needs, so the provider is not asked again for it.
func TestNodesAttentionReusesWhatStatusRead(t *testing.T) {
	var calls atomic.Int32
	src := fakeSrc{ns: []headscale.Node{hsNode("exit-a", true), hsNode("exit-b", true), hsNode("exit-c", false)}}
	m := attentionModule(t, src, map[string]string{"exit-a": "RUNNING", "exit-b": "RUNNING", "exit-c": "STOPPED"}, &calls)
	rec := httptest.NewRecorder()
	m.status(rec, httptest.NewRequest("GET", "/status", nil))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"RUNNING"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, _, err := m.Attention(context.Background(), "alice@"); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("/status then Attention called the providers %d times, want 3 (once per node)", got)
	}
	// /status itself stays fresh: the page shows a start or stop as soon as the provider does.
	m.status(httptest.NewRecorder(), httptest.NewRequest("GET", "/status", nil))
	if got := calls.Load(); got != 6 {
		t.Fatalf("a second /status called the providers %d times in all, want 6", got)
	}
}

// slowStatusModule has one node whose provider holds every call until release is closed or its context ends.
func slowStatusModule(t *testing.T) (m *Module, calls *atomic.Int32, entered, release chan struct{}) {
	t.Helper()
	calls = &atomic.Int32{}
	entered, release = make(chan struct{}, 16), make(chan struct{})
	p := &fakeProvider{stat: func(ctx context.Context, _ Target) (Status, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
			return Status{"status": "RUNNING"}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	cfg, err := ParseConfig([]byte(`{"nodes":{"exit-a":{"provider":"yandex","yandex":{"instance_id":"X"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return New(cfg, nil, map[string]Provider{"yandex": p}).(*Module), calls, entered, release
}

// A poll that goes away mid-read says nothing about the node, so the next poll must not inherit an error.
func TestNodesAttentionCancelledReadIsNotCached(t *testing.T) {
	m, _, entered, release := slowStatusModule(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Attention(ctx, "alice@")
		close(done)
	}()
	<-entered
	cancel()
	<-done
	close(release)

	_, lines, err := m.Attention(context.Background(), "alice@")
	if err != nil || len(lines) != 1 || lines[0].State != "ok" {
		t.Fatalf("the node reads as unchecked after a cancelled read: %+v %v", lines, err)
	}
}

func TestNodesConcurrentReadsShareOneProviderCall(t *testing.T) {
	m, calls, entered, release := slowStatusModule(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		m.Attention(context.Background(), "alice@")
	}()
	go func() {
		defer wg.Done()
		m.status(httptest.NewRecorder(), httptest.NewRequest("GET", "/status", nil))
	}()
	<-entered
	time.Sleep(50 * time.Millisecond) // let the other arrive
	close(release)
	wg.Wait()
	if n := calls.Load(); n != 1 {
		t.Fatalf("concurrent reads called the provider %d times, want 1", n)
	}
}

// powerModule has one node, online on the tailnet, whose provider answers with stat and fails actions with actErr.
func powerModule(t *testing.T, stat func(ctx context.Context) (Status, error), actErr error) *Module {
	t.Helper()
	p := &fakeProvider{
		stat: func(ctx context.Context, _ Target) (Status, error) { return stat(ctx) },
		act:  func(string, Target) (string, string, error) { return "RUNNING", "STOPPED", actErr },
	}
	cfg, err := ParseConfig([]byte(`{"nodes":{"exit-a":{"provider":"yandex","yandex":{"instance_id":"X"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	m := New(cfg, fakeSrc{ns: []headscale.Node{hsNode("exit-a", true)}}, map[string]Provider{"yandex": p}).(*Module)
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return now } // the clock stands still: everything here is within the cache's TTL
	return m
}

func postStop(m *Module) {
	req := httptest.NewRequest("POST", "/nodes/exit-a/stop", nil)
	req.SetPathValue("id", "exit-a")
	m.act("stop")(httptest.NewRecorder(), req)
}

// A node stopped from the console goes offline at once; a remembered "running" must not call that a failure. A failed
// action may have taken effect all the same, so it drops the remembered status too.
func TestNodesAttentionForgetsStatusAfterPowerAction(t *testing.T) {
	for name, actErr := range map[string]error{"succeeded": nil, "failed": errors.New("provider down")} {
		t.Run(name, func(t *testing.T) {
			var word atomic.Value
			word.Store("RUNNING")
			m := powerModule(t, func(context.Context) (Status, error) { return Status{"status": word.Load().(string)}, nil }, actErr)
			if items, _, err := m.Attention(context.Background(), "alice@"); err != nil || len(items) != 0 {
				t.Fatalf("%+v %v", items, err)
			}
			word.Store("STOPPED")
			postStop(m)
			m.src = fakeSrc{ns: []headscale.Node{hsNode("exit-a", false)}}
			if items, _, err := m.Attention(context.Background(), "alice@"); err != nil || len(items) != 0 {
				t.Fatalf("a stopped node raised %+v %v", items, err)
			}
		})
	}
}

// A read that began before the action may end after it: nobody may be given its answer from then on.
func TestNodesAttentionIgnoresReadOlderThanPowerAction(t *testing.T) {
	var calls atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	m := powerModule(t, func(context.Context) (Status, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-release
			return Status{"status": "RUNNING"}, nil
		}
		return Status{"status": "STOPPED"}, nil
	}, nil)
	old := make(chan struct{})
	go func() {
		m.status(httptest.NewRecorder(), httptest.NewRequest("GET", "/status", nil))
		close(old)
	}()
	<-entered
	postStop(m)
	m.src = fakeSrc{ns: []headscale.Node{hsNode("exit-a", false)}}

	done := make(chan []core.AttentionItem, 1)
	go func() {
		items, _, _ := m.Attention(context.Background(), "alice@")
		done <- items
	}()
	select {
	case items := <-done:
		if len(items) != 0 {
			t.Errorf("after the action: %+v", items)
		}
	case <-time.After(2 * time.Second):
		t.Error("Attention joined the read older than the action")
	}
	close(release)
	<-old
	if items, _, err := m.Attention(context.Background(), "alice@"); err != nil || len(items) != 0 {
		t.Fatalf("the older read went back into the cache: %+v %v", items, err)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d provider calls, want 2", n)
	}
}

var (
	_ core.ActivitySource = (*Module)(nil)
	_ core.SearchSource   = (*Module)(nil)
)

func TestNodesActivityReadsOnlyNodeActions(t *testing.T) {
	lg := &recLog{entries: []core.AuditEntry{
		{At: time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC), Actor: "alice@", Action: "node.stop", Detail: "exit-c: ok"},
		{At: time.Date(2026, 10, 31, 11, 0, 0, 0, time.UTC), Actor: "alice@", Action: "device.revoke", Detail: "phone"},
		{At: time.Date(2026, 10, 31, 10, 0, 0, 0, time.UTC), Actor: "bob@", Action: "node.start", Detail: "exit-a: failed (timeout)"},
	}}
	m := New(attentionConfig(t), nil, nil, WithAudit(func() core.AuditLog { return lg })).(*Module)
	got, err := m.Activity(context.Background(), "alice@", 10)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].Source != "Nodes" || got[0].Action != "node.stop" || got[0].Detail != "exit-c: ok" || got[0].Actor != "alice@" || got[1].Action != "node.start" {
		t.Errorf("%+v", got)
	}
	if one, _ := m.Activity(context.Background(), "alice@", 1); len(one) != 1 {
		t.Errorf("limit: %+v", one)
	}
	if !m.HasActivity() {
		t.Error("a module with an audit log has activity")
	}
}

func TestNodesActivityWithoutALogIsEmpty(t *testing.T) {
	for name, opts := range map[string][]Option{
		"no function":        nil,
		"function gives nil": {WithAudit(func() core.AuditLog { return nil })},
	} {
		m := New(attentionConfig(t), nil, nil, opts...).(*Module)
		if got, err := m.Activity(context.Background(), "alice@", 10); got != nil || err != nil {
			t.Errorf("%s: %v %v", name, got, err)
		}
		if m.HasActivity() {
			t.Errorf("%s: nodes alone keep no activity", name)
		}
	}
}

func TestNodesActivityReportsAnUnreadableLog(t *testing.T) {
	m := New(attentionConfig(t), nil, nil, WithAudit(func() core.AuditLog { return &brokenLog{} })).(*Module)
	if _, err := m.Activity(context.Background(), "alice@", 10); err == nil {
		t.Error("an unreadable log is an error, not an empty list")
	}
}

type brokenLog struct{ recLog }

func (*brokenLog) AuditLog(int) ([]core.AuditEntry, error) { return nil, errors.New("down") }

func TestNodesSearchListsNodeIDs(t *testing.T) {
	m := New(attentionConfig(t), nil, nil).(*Module)
	items, err := m.Search(context.Background(), "alice@")
	if err != nil || len(items) != 3 {
		t.Fatalf("%+v %v", items, err)
	}
	for _, it := range items {
		if it.Kind != "Node" || it.Path != "/nodes?sel="+it.Label {
			t.Errorf("%+v", it)
		}
	}
}

func TestNodesStoppedNames(t *testing.T) {
	var calls atomic.Int32
	m := attentionModule(t, nil, map[string]string{"exit-a": "STOPPED", "exit-b": "RUNNING", "exit-c": "not_configured"}, &calls)
	got := m.StoppedNames(context.Background())
	if len(got) != 1 || !got["exit-a"] {
		t.Fatalf("only what the provider says is stopped: %v", got)
	}
	m = attentionModule(t, nil, map[string]string{"exit-b": "RUNNING"}, &calls) // exit-a and exit-c do not answer
	if got := m.StoppedNames(context.Background()); len(got) != 0 {
		t.Fatalf("no answer is not stopped: %v", got)
	}
}
