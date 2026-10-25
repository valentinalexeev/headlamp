package reconcile

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
	"funnel-ingress/internal/tsnode"
)

type nopPersister struct{}

func (nopPersister) Load() ([]byte, bool, error) { return nil, false, nil }
func (nopPersister) Save([]byte) error           { return nil }

type failingPersister struct {
	mu   sync.Mutex
	fail bool
}

func (f *failingPersister) Load() ([]byte, bool, error) { return nil, false, nil }
func (f *failingPersister) Save([]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return fmt.Errorf("redis down")
	}
	return nil
}

type rig struct {
	r     *Reconciler
	node  *tsnode.Fake
	table *routes.Table
	logs  []string
}

var goodTags = []string{"tag:funnel", "tag:ns-pdl"}

func newRig(t *testing.T, p routes.Persister) *rig {
	t.Helper()
	table, err := routes.NewTable(p, 10)
	if err != nil {
		t.Fatal(err)
	}
	ns := auth.NewNamespaces()
	ns.Set(map[string][]config.PortRange{"pdl": {{Lo: 1, Hi: 65535}}, "other": {{Lo: 1, Hi: 65535}}})
	rg := &rig{node: tsnode.NewFake(), table: table}
	rg.r = &Reconciler{
		Node: rg.node, Table: table, Policy: auth.Policy{FunnelTag: "tag:funnel", NSTagPrefix: "tag:ns-", NS: ns},
		MissingChecks: 3, Interval: time.Hour,
		Log: func(f string, a ...any) { rg.logs = append(rg.logs, fmt.Sprintf(f, a...)) },
	}
	return rg
}

func (rg *rig) put(t *testing.T, ns, svc, node string) {
	t.Helper()
	if err := rg.table.Put(routes.Route{Key: routes.Key{NS: ns, Service: svc}, Target: "100.64.0.5", Port: 80, NodeID: node}); err != nil {
		t.Fatal(err)
	}
}

func peer(id, name string, online bool, tags ...string) tsnode.Peer {
	return tsnode.Peer{NodeID: id, Name: name, Online: online, Tags: tags}
}

func (rg *rig) has(ns, svc string) bool {
	_, ok := rg.table.Lookup(routes.Key{NS: ns, Service: svc})
	return ok
}

func TestOfflineNodeKept(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.node.SetPeers([]tsnode.Peer{peer("n1", "box", false, goodTags...)})
	for i := 0; i < 20; i++ {
		if removed := rg.r.Tick(context.Background()); len(removed) != 0 {
			t.Fatalf("tick %d removed %v", i, removed)
		}
	}
	if !rg.has("pdl", "admin") {
		t.Fatal("an offline node's route was removed")
	}
}

func TestAbsentNodeRemovedAfterNChecks(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.put(t, "pdl", "wiki", "n1")
	for i := 1; i <= 2; i++ {
		if removed := rg.r.Tick(context.Background()); len(removed) != 0 {
			t.Fatalf("tick %d removed %v", i, removed)
		}
	}
	removed := rg.r.Tick(context.Background())
	if len(removed) != 2 || rg.has("pdl", "admin") || rg.has("pdl", "wiki") {
		t.Fatalf("tick 3 removed %v", removed)
	}
}

func TestMissCounterResetsWhenNodeReappears(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.r.Tick(context.Background())
	rg.r.Tick(context.Background())
	rg.node.SetPeers([]tsnode.Peer{peer("n1", "box", true, goodTags...)})
	rg.r.Tick(context.Background())
	rg.node.SetPeers(nil)
	rg.r.Tick(context.Background())
	rg.r.Tick(context.Background())
	if !rg.has("pdl", "admin") {
		t.Fatal("misses were not consecutive but the route was removed")
	}
	if removed := rg.r.Tick(context.Background()); len(removed) != 1 {
		t.Fatalf("third consecutive miss removed %v", removed)
	}
}

func TestNoCleanupBeforeSnapshotLoaded(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.node.SetLoaded(false)
	for i := 0; i < 10; i++ {
		rg.r.Tick(context.Background())
	}
	if !rg.has("pdl", "admin") {
		t.Fatal("cleanup ran on an unloaded peer list")
	}
	if !rg.r.LastTick().IsZero() {
		t.Fatal("LastTick set without a complete list")
	}
	// The counters did not move while unloaded: it still takes three loaded ticks.
	rg.node.SetLoaded(true)
	rg.r.Tick(context.Background())
	rg.r.Tick(context.Background())
	if !rg.has("pdl", "admin") {
		t.Fatal("counters advanced while the list was not loaded")
	}
	rg.r.Tick(context.Background())
	if rg.has("pdl", "admin") {
		t.Fatal("route survived three loaded ticks without its owner")
	}
	if rg.r.LastTick().IsZero() {
		t.Fatal("LastTick not set")
	}
}

func TestTagLossRemovesRoutes(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.put(t, "other", "api", "n1")
	rg.put(t, "pdl", "keep", "n2")
	rg.node.SetPeers([]tsnode.Peer{
		peer("n1", "box", true, "tag:funnel", "tag:ns-other"), // lost tag:ns-pdl
		peer("n2", "box2", true, goodTags...),
	})
	removed := rg.r.Tick(context.Background())
	if len(removed) != 1 || removed[0] != (routes.Key{NS: "pdl", Service: "admin"}) {
		t.Fatalf("removed %v", removed)
	}
	if !rg.has("other", "api") || !rg.has("pdl", "keep") {
		t.Fatal("unrelated routes removed")
	}
	// Losing the funnel tag removes everything of that node.
	rg.node.SetPeers([]tsnode.Peer{peer("n1", "box", true, "tag:ns-other"), peer("n2", "box2", true, goodTags...)})
	if removed := rg.r.Tick(context.Background()); len(removed) != 1 || rg.has("other", "api") {
		t.Fatalf("removed %v", removed)
	}
}

func TestRenamedNodeKeepsRoutes(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.node.SetPeers([]tsnode.Peer{peer("n1", "new-name", true, goodTags...)})
	for i := 0; i < 10; i++ {
		rg.r.Tick(context.Background())
	}
	if !rg.has("pdl", "admin") {
		t.Fatal("a rename dropped the routes")
	}
}

func TestSameNameNewIDLosesRoutes(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "n1")
	rg.node.SetPeers([]tsnode.Peer{peer("n2", "box", true, goodTags...)}) // same name, new stable ID
	for i := 0; i < 3; i++ {
		rg.r.Tick(context.Background())
	}
	if rg.has("pdl", "admin") {
		t.Fatal("a re-enrolled node inherited the old node's route")
	}
	if len(rg.table.ByNode("n2")) != 0 {
		t.Fatal("new node owns routes it never registered")
	}
}

func TestRemovalIsLogged(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.put(t, "pdl", "admin", "node-42")
	for i := 0; i < 3; i++ {
		rg.r.Tick(context.Background())
	}
	joined := strings.Join(rg.logs, "\n")
	for _, want := range []string{"node-42", "pdl", "admin"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q: %s", want, joined)
		}
	}
}

func TestStaticRoutesAreNeverTouched(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.table.SetStatic([]routes.Route{{Key: routes.Key{NS: "pdl", Service: "docs"}, Target: "pdl-1", Port: 80}})
	for i := 0; i < 10; i++ {
		rg.r.Tick(context.Background())
	}
	if !rg.has("pdl", "docs") {
		t.Fatal("a static route was removed")
	}
}

func TestFailedSaveIsRetried(t *testing.T) {
	fp := &failingPersister{}
	rg := newRig(t, fp)
	rg.put(t, "pdl", "admin", "n1")
	fp.fail = true
	for i := 0; i < 5; i++ {
		rg.r.Tick(context.Background())
	}
	if !rg.has("pdl", "admin") {
		t.Fatal("route removed although the removal could not be saved")
	}
	fp.fail = false
	rg.r.Tick(context.Background())
	if rg.has("pdl", "admin") {
		t.Fatal("removal was not retried")
	}
}

func TestConcurrentWithPut(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.node.SetPeers([]tsnode.Peer{peer("n1", "box", true, goodTags...)})
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(context.Background())
	wg.Add(2)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			rg.r.Tick(ctx)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = rg.table.Put(routes.Route{Key: routes.Key{NS: "pdl", Service: fmt.Sprintf("s%d", i%7)}, Target: "100.64.0.5", Port: 80, NodeID: fmt.Sprintf("n%d", i%3)})
			_ = rg.table.Delete(routes.Key{NS: "pdl", Service: fmt.Sprintf("s%d", i%7)}, fmt.Sprintf("n%d", i%3))
		}
		cancel()
	}()
	wg.Wait()
}

func TestRunStopsOnCancel(t *testing.T) {
	rg := newRig(t, nopPersister{})
	rg.r.Interval = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rg.r.Run(ctx); close(done) }()
	time.Sleep(50 * time.Millisecond)
	if rg.r.LastTick().IsZero() {
		t.Error("Run never ticked")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not stop")
	}
}
