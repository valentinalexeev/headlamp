package routes

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
)

type memPersister struct {
	mu    sync.Mutex
	blob  []byte
	has   bool
	saves int
	fail  error
}

func (m *memPersister) Load() ([]byte, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.blob, m.has, nil
}

func (m *memPersister) Save(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		return m.fail
	}
	m.blob, m.has = append([]byte(nil), b...), true
	m.saves++
	return nil
}

func (m *memPersister) count() int { m.mu.Lock(); defer m.mu.Unlock(); return m.saves }

func newTable(t *testing.T, max int) (*Table, *memPersister) {
	t.Helper()
	p := &memPersister{}
	tb, err := NewTable(p, max)
	if err != nil {
		t.Fatal(err)
	}
	return tb, p
}

func dyn(ns, svc, node string, port int) Route {
	return Route{Key: Key{ns, svc}, Target: "100.64.0.9", Port: port, NodeID: node}
}

func stat(ns, svc string) Route {
	return Route{Key: Key{ns, svc}, Target: "pdl-1", Port: 8080}
}

func TestStaticWinsOverDynamic(t *testing.T) {
	tb, _ := newTable(t, 5)
	tb.SetStatic([]Route{stat("pdl", "docs")})
	if err := tb.Put(dyn("pdl", "docs", "n1", 9000)); !errors.Is(err, ErrStaticConflict) {
		t.Fatalf("want ErrStaticConflict, got %v", err)
	}
	r, ok := tb.Lookup(Key{"pdl", "docs"})
	if !ok || r.Kind != Static || r.Target != "pdl-1" {
		t.Fatalf("lookup: %+v %v", r, ok)
	}
}

func TestSetStaticDropsCollidingDynamic(t *testing.T) {
	tb, _ := newTable(t, 5)
	if err := tb.Put(dyn("pdl", "docs", "n1", 9000)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Put(dyn("pdl", "other", "n1", 9001)); err != nil {
		t.Fatal(err)
	}
	dropped := tb.SetStatic([]Route{stat("pdl", "docs")})
	if len(dropped) != 1 || dropped[0] != (Key{"pdl", "docs"}) {
		t.Fatalf("dropped = %v", dropped)
	}
	if r, _ := tb.Lookup(Key{"pdl", "docs"}); r.Kind != Static {
		t.Fatal("static route does not win")
	}
	if _, ok := tb.Lookup(Key{"pdl", "other"}); !ok {
		t.Fatal("unrelated dynamic route lost")
	}
}

func TestSetStaticSkipsInvalidAndDefaultsScheme(t *testing.T) {
	tb, _ := newTable(t, 5)
	tb.SetStatic([]Route{stat("pdl", "ok"), {Key: Key{"PDL", "bad"}, Target: "x", Port: 1}, {Key: Key{"pdl", "noport"}, Target: "x"}})
	if got := len(tb.All()); got != 1 {
		t.Fatalf("All() = %d routes", got)
	}
	if r, _ := tb.Lookup(Key{"pdl", "ok"}); r.Scheme != "http" {
		t.Fatalf("scheme = %q", r.Scheme)
	}
}

func TestSetBlockedRemovesDynamicAndRefusesPut(t *testing.T) {
	tb, _ := newTable(t, 5)
	if err := tb.Put(dyn("pdl", "admin", "n1", 8080)); err != nil {
		t.Fatal(err)
	}
	removed := tb.SetBlocked([]Key{{"pdl", "admin"}})
	if len(removed) != 1 {
		t.Fatalf("removed = %v", removed)
	}
	if _, ok := tb.Lookup(Key{"pdl", "admin"}); ok {
		t.Fatal("blocked route still served")
	}
	if err := tb.Put(dyn("pdl", "admin", "n1", 8080)); !errors.Is(err, ErrBlocked) {
		t.Fatalf("want ErrBlocked, got %v", err)
	}
	if got := tb.Blocked(); len(got) != 1 || got[0] != (Key{"pdl", "admin"}) {
		t.Fatalf("Blocked() = %v", got)
	}
}

func TestUnblockAllowsPutAgain(t *testing.T) {
	tb, _ := newTable(t, 5)
	tb.SetBlocked([]Key{{"pdl", "admin"}})
	tb.SetBlocked(nil)
	if err := tb.Put(dyn("pdl", "admin", "n1", 8080)); err != nil {
		t.Fatalf("put after unblock: %v", err)
	}
}

func TestSetStaticAndBlockedAreNotPersisted(t *testing.T) {
	tb, p := newTable(t, 5)
	tb.SetStatic([]Route{stat("pdl", "docs")})
	tb.SetBlocked([]Key{{"pdl", "admin"}})
	if err := tb.Put(dyn("pdl", "wiki", "n1", 80)); err != nil {
		t.Fatal(err)
	}
	tb2, err := NewTable(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(tb2.Blocked()) != 0 {
		t.Fatal("blocked set was persisted")
	}
	all := tb2.All()
	if len(all) != 1 || all[0].Kind != Dynamic {
		t.Fatalf("restored table = %+v", all)
	}
}

func TestPutIdempotentSameNode(t *testing.T) {
	tb, p := newTable(t, 5)
	r := dyn("pdl", "admin", "n1", 8080)
	for i := 0; i < 3; i++ {
		if err := tb.Put(r); err != nil {
			t.Fatal(err)
		}
	}
	if p.count() != 1 {
		t.Fatalf("saves = %d, want 1", p.count())
	}
	r.Port = 9090
	if err := tb.Put(r); err != nil {
		t.Fatal(err)
	}
	if got, _ := tb.Lookup(r.Key); got.Port != 9090 || p.count() != 2 {
		t.Fatalf("update: port %d, saves %d", got.Port, p.count())
	}
}

func TestIdempotentPutDoesNotSave(t *testing.T) {
	tb, p := newTable(t, 5)
	r := dyn("pdl", "admin", "n1", 8080)
	if err := tb.Put(r); err != nil {
		t.Fatal(err)
	}
	before := p.count()
	// The stored route has Scheme normalised to "http"; an explicit "http" is the same route.
	r.Scheme = "http"
	if err := tb.Put(r); err != nil {
		t.Fatal(err)
	}
	if p.count() != before {
		t.Fatal("an unchanged re-registration wrote to the state store")
	}
}

func TestUnchangedSetStaticSetBlockedDoNotSave(t *testing.T) {
	tb, p := newTable(t, 5)
	for i := 0; i < 3; i++ {
		tb.SetStatic([]Route{stat("pdl", "docs")})
		tb.SetBlocked([]Key{{"pdl", "x"}})
	}
	if p.count() != 0 {
		t.Fatalf("saves = %d", p.count())
	}
}

func TestSetStaticAndSetBlockedNeverSave(t *testing.T) {
	tb, p := newTable(t, 5)
	if err := tb.Put(dyn("pdl", "a", "n1", 80)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Put(dyn("pdl", "b", "n1", 81)); err != nil {
		t.Fatal(err)
	}
	before := p.count()
	tb.SetStatic([]Route{stat("pdl", "a")})
	tb.SetBlocked([]Key{{"pdl", "b"}})
	if p.count() != before {
		t.Fatal("config-driven changes wrote to the state store")
	}
}

func TestPutDifferentNodeConflict(t *testing.T) {
	tb, _ := newTable(t, 5)
	if err := tb.Put(dyn("pdl", "admin", "n1", 8080)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Put(dyn("pdl", "admin", "n2", 8080)); !errors.Is(err, ErrOwnedByOther) {
		t.Fatalf("want ErrOwnedByOther, got %v", err)
	}
}

func TestDeleteOnlyOwner(t *testing.T) {
	tb, _ := newTable(t, 5)
	k := Key{"pdl", "admin"}
	if err := tb.Put(dyn("pdl", "admin", "n1", 8080)); err != nil {
		t.Fatal(err)
	}
	if err := tb.Delete(k, "n2"); !errors.Is(err, ErrOwnedByOther) {
		t.Fatalf("want ErrOwnedByOther, got %v", err)
	}
	if _, ok := tb.Lookup(k); !ok {
		t.Fatal("route removed by a non-owner")
	}
	if err := tb.Delete(k, "n1"); err != nil {
		t.Fatal(err)
	}
	if err := tb.Delete(k, "n2"); err != nil {
		t.Fatalf("deleting an absent key: %v", err)
	}
}

func TestLimitPerNode(t *testing.T) {
	tb, _ := newTable(t, 2)
	for i, svc := range []string{"a", "b"} {
		if err := tb.Put(dyn("pdl", svc, "n1", 80+i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.Put(dyn("pdl", "c", "n1", 90)); !errors.Is(err, ErrLimit) {
		t.Fatalf("want ErrLimit, got %v", err)
	}
	if err := tb.Put(dyn("pdl", "a", "n1", 99)); err != nil {
		t.Fatalf("updating an existing route at the limit: %v", err)
	}
	if err := tb.Put(dyn("pdl", "c", "n2", 90)); err != nil {
		t.Fatalf("other node: %v", err)
	}
}

func TestBadNames(t *testing.T) {
	tb, _ := newTable(t, 5)
	long := strings.Repeat("a", 64)
	for _, svc := range []string{"Admin", "a.b", "", long, "-a", "a-", "a_b", "a b"} {
		if err := tb.Put(dyn("pdl", svc, "n1", 80)); !errors.Is(err, ErrBadName) {
			t.Errorf("service %q: got %v", svc, err)
		}
	}
	if err := tb.Put(dyn("PDL", "a", "n1", 80)); !errors.Is(err, ErrBadName) {
		t.Errorf("uppercase namespace accepted: %v", err)
	}
	if err := tb.Put(dyn("pdl", strings.Repeat("a", 63), "n1", 80)); err != nil {
		t.Errorf("63-char label rejected: %v", err)
	}
	for _, port := range []int{0, -1, 65536} {
		if err := tb.Put(dyn("pdl", "p", "n1", port)); !errors.Is(err, ErrBadName) {
			t.Errorf("port %d: %v", port, err)
		}
	}
	r := dyn("pdl", "s", "n1", 80)
	r.Scheme = "ftp"
	if err := tb.Put(r); !errors.Is(err, ErrBadName) {
		t.Errorf("scheme ftp: %v", err)
	}
	if err := tb.Put(dyn("pdl", "s", "", 80)); !errors.Is(err, ErrBadName) {
		t.Errorf("empty owner: %v", err)
	}
}

func TestPersistRoundTrip(t *testing.T) {
	tb, p := newTable(t, 5)
	r := dyn("pdl", "admin", "n1", 8080)
	r.Scheme = "https"
	r.InsecureSkipVerify = true
	if err := tb.Put(r); err != nil {
		t.Fatal(err)
	}
	if err := tb.Put(dyn("pdl", "wiki", "n2", 81)); err != nil {
		t.Fatal(err)
	}
	tb2, err := NewTable(p, 5)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := tb2.Lookup(Key{"pdl", "admin"})
	r.Kind = Dynamic
	if !ok || got != r {
		t.Fatalf("restored %+v, want %+v", got, r)
	}
	if ids := tb2.NodeIDs(); len(ids) != 2 || ids[0] != "n1" || ids[1] != "n2" {
		t.Fatalf("NodeIDs = %v", ids)
	}
}

func TestCorruptBlobFailsAndIsNotOverwritten(t *testing.T) {
	for name, blob := range map[string]string{
		"garbage":       "not json",
		"wrong version": `{"version":99,"routes":[]}`,
		"bad entry":     `{"version":1,"routes":[{"ns":"PDL","service":"a","target":"x","port":1,"node_id":"n"}]}`,
		"no owner":      `{"version":1,"routes":[{"ns":"pdl","service":"a","target":"x","port":1}]}`,
		"duplicate":     `{"version":1,"routes":[{"ns":"pdl","service":"a","target":"x","port":1,"node_id":"n"},{"ns":"pdl","service":"a","target":"y","port":2,"node_id":"n"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := &memPersister{blob: []byte(blob), has: true}
			if _, err := NewTable(p, 5); err == nil {
				t.Fatal("corrupt blob accepted")
			}
			if p.count() != 0 || string(p.blob) != blob {
				t.Fatal("corrupt blob was overwritten")
			}
		})
	}
}

func TestFailedSaveRefusesMutationAndKeepsState(t *testing.T) {
	tb, p := newTable(t, 5)
	if err := tb.Put(dyn("pdl", "keep", "n1", 80)); err != nil {
		t.Fatal(err)
	}
	p.fail = errors.New("redis down")
	if err := tb.Put(dyn("pdl", "new", "n1", 81)); !errors.Is(err, ErrPersist) {
		t.Fatalf("put: %v", err)
	}
	if _, ok := tb.Lookup(Key{"pdl", "new"}); ok {
		t.Fatal("route accepted although it could not be saved")
	}
	if err := tb.Delete(Key{"pdl", "keep"}, "n1"); !errors.Is(err, ErrPersist) {
		t.Fatalf("delete: %v", err)
	}
	if _, ok := tb.Lookup(Key{"pdl", "keep"}); !ok {
		t.Fatal("route removed although the removal could not be saved")
	}
	if _, err := tb.RemoveNode("n1"); !errors.Is(err, ErrPersist) {
		t.Fatalf("remove node: %v", err)
	}
	if len(tb.ByNode("n1")) != 1 {
		t.Fatal("routes removed although the removal could not be saved")
	}
	p.fail = nil
	if err := tb.Put(dyn("pdl", "new", "n1", 81)); err != nil {
		t.Fatalf("put after recovery: %v", err)
	}
}

func TestRemoveNodeSavesOnce(t *testing.T) {
	tb, p := newTable(t, 5)
	for _, s := range []string{"a", "b", "c"} {
		if err := tb.Put(dyn("pdl", s, "n1", 80)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tb.Put(dyn("pdl", "z", "n2", 80)); err != nil {
		t.Fatal(err)
	}
	before := p.count()
	keys, err := tb.RemoveNode("n1")
	if err != nil || len(keys) != 3 {
		t.Fatalf("removed %v, %v", keys, err)
	}
	if p.count() != before+1 {
		t.Fatalf("saves = %d", p.count()-before)
	}
	if keys, err := tb.RemoveNode("n1"); err != nil || len(keys) != 0 || p.count() != before+1 {
		t.Fatal("second removal should be a silent no-op")
	}
	if len(tb.ByNode("n2")) != 1 {
		t.Fatal("other node's routes touched")
	}
}

func TestAllListsStaticThenDynamicSorted(t *testing.T) {
	tb, _ := newTable(t, 5)
	tb.SetStatic([]Route{stat("pdl", "zz"), stat("abc", "yy")})
	_ = tb.Put(dyn("pdl", "bb", "n1", 80))
	_ = tb.Put(dyn("aaa", "cc", "n1", 80))
	var got []string
	for _, r := range tb.All() {
		got = append(got, fmt.Sprintf("%s/%s/%s", r.Kind, r.NS, r.Service))
	}
	want := "static/abc/yy static/pdl/zz dynamic/aaa/cc dynamic/pdl/bb"
	if strings.Join(got, " ") != want {
		t.Fatalf("All() = %v", got)
	}
}

func TestConcurrentPutDelete(t *testing.T) {
	tb, _ := newTable(t, 1000)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			node := fmt.Sprintf("n%d", g)
			for i := 0; i < 50; i++ {
				svc := fmt.Sprintf("s%d", i%5)
				_ = tb.Put(dyn("pdl", svc, node, 80))
				tb.Lookup(Key{"pdl", svc})
				tb.ByNode(node)
				tb.All()
				tb.NodeIDs()
				_ = tb.Delete(Key{"pdl", svc}, node)
				if i%10 == 0 {
					tb.SetStatic([]Route{stat("pdl", "s1")})
					tb.SetBlocked([]Key{{"pdl", "s2"}})
					_, _ = tb.RemoveNode(node)
				}
			}
		}(g)
	}
	wg.Wait()
}
