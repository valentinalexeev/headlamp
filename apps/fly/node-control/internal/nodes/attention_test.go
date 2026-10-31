package nodes

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
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
