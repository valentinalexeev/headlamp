package funnel

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func openAt(t *testing.T, path string, clk *fakeClock) Store {
	t.Helper()
	s, err := OpenSQLite(path, clk.Now)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newStore(t *testing.T) (Store, *fakeClock, string) {
	t.Helper()
	clk := newClock()
	path := filepath.Join(t.TempDir(), "funnel.db")
	return openAt(t, path, clk), clk, path
}

func r(lo, hi int) Range { return Range{lo, hi} }

func get(t *testing.T, s Store, name string) Namespace {
	t.Helper()
	all, err := s.Namespaces()
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range all {
		if n.Name == name {
			return n
		}
	}
	t.Fatalf("namespace %q not found in %v", name, all)
	return Namespace{}
}

func mustActive(t *testing.T, s Store, name string, ports ...Range) {
	t.Helper()
	if err := s.CreateNamespace(name, ports, "", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkActive(name); err != nil {
		t.Fatal(err)
	}
}

func TestNamespaceLifecyclePendingActiveFailed(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.CreateNamespace("alpha", []Range{r(8080, 8080)}, "first", 7); err != nil {
		t.Fatal(err)
	}
	n := get(t, s, "alpha")
	if n.State != "pending" || n.ChangeID != 7 || n.Note != "first" || !reflect.DeepEqual(n.Ports, []Range{r(8080, 8080)}) || n.PendingPorts != nil {
		t.Fatalf("%+v", n)
	}
	if err := s.MarkActive("alpha"); err != nil {
		t.Fatal(err)
	}
	if n = get(t, s, "alpha"); n.State != "active" {
		t.Fatalf("%+v", n)
	}
	if err := s.MarkActive("alpha"); err != nil { // no-op
		t.Fatal(err)
	}
	if err := s.MarkFailed("alpha"); err != nil { // no-op on active without pending ports
		t.Fatal(err)
	}
	if n = get(t, s, "alpha"); n.State != "active" {
		t.Fatalf("%+v", n)
	}

	if err := s.CreateNamespace("beta", []Range{r(9000, 9000)}, "", 8); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkFailed("beta"); err != nil {
		t.Fatal(err)
	}
	if n = get(t, s, "beta"); n.State != "failed" {
		t.Fatalf("%+v", n)
	}
	for _, f := range []func(string) error{s.MarkActive, s.MarkFailed, s.DeleteNamespace} {
		if err := f("nope"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("want ErrNotFound, got %v", err)
		}
	}
	if err := s.DeleteNamespace("alpha"); err != nil {
		t.Fatal(err)
	}
	all, _ := s.Namespaces()
	if len(all) != 1 || all[0].Name != "beta" {
		t.Fatalf("%v", all)
	}
}

func TestCreateOverFailedReplaces(t *testing.T) {
	s, _, _ := newStore(t)
	s.CreateNamespace("alpha", []Range{r(1, 1)}, "old", 1)
	s.MarkFailed("alpha")
	if err := s.CreateNamespace("alpha", []Range{r(2, 3)}, "new", 2); err != nil {
		t.Fatal(err)
	}
	n := get(t, s, "alpha")
	if n.State != "pending" || n.Note != "new" || n.ChangeID != 2 || !reflect.DeepEqual(n.Ports, []Range{r(2, 3)}) {
		t.Fatalf("%+v", n)
	}
}

func TestCreateOverExistingIsErrExists(t *testing.T) {
	s, _, _ := newStore(t)
	s.CreateNamespace("alpha", []Range{r(1, 1)}, "", 1)
	if err := s.CreateNamespace("alpha", []Range{r(1, 1)}, "", 2); !errors.Is(err, ErrExists) {
		t.Fatalf("pending: %v", err)
	}
	s.MarkActive("alpha")
	if err := s.CreateNamespace("alpha", []Range{r(1, 1)}, "", 3); !errors.Is(err, ErrExists) {
		t.Fatalf("active: %v", err)
	}
}

func TestPortChangeKeepsOldPortsUntilConfirmed(t *testing.T) {
	s, _, _ := newStore(t)
	mustActive(t, s, "alpha", r(80, 80))
	if err := s.SetPendingPorts("alpha", []Range{r(80, 80), r(443, 443)}, 9); err != nil {
		t.Fatal(err)
	}
	n := get(t, s, "alpha")
	if n.State != "active" || n.ChangeID != 9 ||
		!reflect.DeepEqual(n.Ports, []Range{r(80, 80)}) || !reflect.DeepEqual(n.PendingPorts, []Range{r(80, 80), r(443, 443)}) {
		t.Fatalf("%+v", n)
	}
	if err := s.MarkActive("alpha"); err != nil {
		t.Fatal(err)
	}
	n = get(t, s, "alpha")
	if !reflect.DeepEqual(n.Ports, []Range{r(80, 80), r(443, 443)}) || n.PendingPorts != nil || n.State != "active" {
		t.Fatalf("%+v", n)
	}
}

func TestFailedPortChangeKeepsActivePorts(t *testing.T) {
	s, _, _ := newStore(t)
	mustActive(t, s, "alpha", r(80, 80))
	s.SetPendingPorts("alpha", []Range{r(443, 443)}, 9)
	if err := s.MarkFailed("alpha"); err != nil {
		t.Fatal(err)
	}
	n := get(t, s, "alpha")
	if n.State != "active" || n.PendingPorts != nil || !reflect.DeepEqual(n.Ports, []Range{r(80, 80)}) {
		t.Fatalf("%+v", n)
	}
}

func TestSetPendingPortsBusyWhenAlreadyPending(t *testing.T) {
	s, _, _ := newStore(t)
	mustActive(t, s, "alpha", r(80, 80))
	s.SetPendingPorts("alpha", []Range{r(443, 443)}, 9)
	if err := s.SetPendingPorts("alpha", []Range{r(444, 444)}, 10); !errors.Is(err, ErrBusy) {
		t.Fatalf("got %v", err)
	}
	s.CreateNamespace("beta", []Range{r(1, 1)}, "", 1) // still pending
	if err := s.SetPendingPorts("beta", []Range{r(2, 2)}, 2); !errors.Is(err, ErrBusy) {
		t.Fatalf("pending ns: %v", err)
	}
	if err := s.SetPendingPorts("nope", []Range{r(2, 2)}, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown: %v", err)
	}
}

func TestPortsAreSortedAndMerged(t *testing.T) {
	s, _, _ := newStore(t)
	in := []Range{r(9000, 9010), r(80, 80), r(81, 82), r(9005, 9020), r(443, 443), r(443, 443)}
	s.CreateNamespace("alpha", in, "", 1)
	want := []Range{r(80, 82), r(443, 443), r(9000, 9020)}
	if got := get(t, s, "alpha").Ports; !reflect.DeepEqual(got, want) {
		t.Fatalf("%v", got)
	}
	s.MarkActive("alpha")
	s.SetPendingPorts("alpha", []Range{r(5, 5), r(1, 4)}, 2)
	if got := get(t, s, "alpha").PendingPorts; !reflect.DeepEqual(got, []Range{r(1, 5)}) {
		t.Fatalf("%v", got)
	}
}

func TestBadNamesAndRangesRejected(t *testing.T) {
	s, _, _ := newStore(t)
	ok := []Range{r(80, 80)}
	many := make([]Range, 33)
	for i := range many {
		many[i] = r(i*10+1, i*10+2) // non-adjacent, so they do not merge
	}
	cases := []struct {
		name  string
		ns    string
		ports []Range
	}{
		{"empty name", "", ok},
		{"64 chars", strings.Repeat("a", 64), ok},
		{"uppercase", "Alpha", ok},
		{"dot", "a.b", ok},
		{"leading dash", "-a", ok},
		{"trailing dash", "a-", ok},
		{"underscore", "a_b", ok},
		{"no ranges", "alpha", nil},
		{"lo>hi", "alpha", []Range{r(10, 9)}},
		{"zero", "alpha", []Range{r(0, 5)}},
		{"negative", "alpha", []Range{r(-1, 5)}},
		{"65536", "alpha", []Range{r(1, 65536)}},
		{"33 ranges", "alpha", many},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := s.CreateNamespace(c.ns, c.ports, "", 1); !errors.Is(err, ErrInvalid) {
				t.Fatalf("got %v", err)
			}
		})
	}
	if err := s.CreateNamespace(strings.Repeat("a", 63), many[:32], "", 1); err != nil {
		t.Fatalf("63 chars and 32 ranges are fine: %v", err)
	}
	if err := s.CreateNamespace("x", []Range{r(1, 65535)}, "", 1); err != nil {
		t.Fatalf("full range: %v", err)
	}
	mustActive(t, s, "beta", r(80, 80))
	if err := s.SetPendingPorts("beta", []Range{r(0, 1)}, 2); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pending ports: %v", err)
	}
	all, _ := s.Namespaces()
	if len(all) != 3 {
		t.Fatalf("rejected input left rows: %v", all)
	}
}

func TestNoteAndTextBounds(t *testing.T) {
	s, _, _ := newStore(t)
	ok := []Range{r(80, 80)}
	if err := s.CreateNamespace("a", ok, strings.Repeat("n", 200), 1); err != nil {
		t.Fatal(err)
	}
	for _, note := range []string{strings.Repeat("n", 201), "a\nb", "a\x00b", "tab\there"} {
		if err := s.CreateNamespace("b", ok, note, 1); !errors.Is(err, ErrInvalid) {
			t.Fatalf("note %q: %v", note, err)
		}
	}
	long := strings.Repeat("x", 501)
	if err := s.Block("a", "svc", long, "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reason: %v", err)
	}
	if err := s.Block("a", "svc", "r\nr", "alice"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("reason control: %v", err)
	}
	if err := s.Block("a", "svc", "r", strings.Repeat("x", 101)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("actor: %v", err)
	}
	if err := s.Block("a", "svc", strings.Repeat("x", 500), strings.Repeat("x", 100)); err != nil {
		t.Fatal(err)
	}
	if err := s.Audit("alice", "act", long); !errors.Is(err, ErrInvalid) {
		t.Fatalf("detail: %v", err)
	}
	if err := s.Audit(strings.Repeat("x", 101), "act", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("audit actor: %v", err)
	}
	if err := s.Audit("al\nice", "act", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("audit actor control: %v", err)
	}
}

func TestDeleteNamespaceInUseWhileManualRoutes(t *testing.T) {
	s, _, _ := newStore(t)
	mustActive(t, s, "alpha", r(80, 80))
	s.PutManual(ManualRoute{NS: "alpha", Service: "web", Target: "10.0.0.1", Port: 80})
	if err := s.DeleteNamespace("alpha"); !errors.Is(err, ErrInUse) {
		t.Fatalf("got %v", err)
	}
	s.DeleteManual("alpha", "web")
	if err := s.DeleteNamespace("alpha"); err != nil {
		t.Fatal(err)
	}
}

func TestManualRouteRoundtripReplaceAndValidation(t *testing.T) {
	s, _, _ := newStore(t)
	a := ManualRoute{NS: "alpha", Service: "web", Target: "host.example.test", Port: 8443, Scheme: "https", InsecureSkipVerify: true}
	if err := s.PutManual(a); err != nil {
		t.Fatal(err)
	}
	b := ManualRoute{NS: "alpha", Service: "api", Target: "10.0.0.2", Port: 80}
	if err := s.PutManual(b); err != nil {
		t.Fatal(err)
	}
	got, err := s.ManualRoutes()
	if err != nil {
		t.Fatal(err)
	}
	b.Scheme = "http" // empty scheme means http
	if !reflect.DeepEqual(got, []ManualRoute{b, a}) {
		t.Fatalf("%+v", got) // ordered by namespace, service
	}
	a.Target, a.Scheme, a.InsecureSkipVerify = "other.example.test", "http", false
	s.PutManual(a)
	got, _ = s.ManualRoutes()
	if len(got) != 2 || got[1] != a {
		t.Fatalf("replace failed: %+v", got)
	}
	bad := []ManualRoute{
		{NS: "Alpha", Service: "web", Target: "t", Port: 80},
		{NS: "alpha", Service: "we b", Target: "t", Port: 80},
		{NS: "alpha", Service: "web", Target: "", Port: 80},
		{NS: "alpha", Service: "web", Target: "a b", Port: 80},
		{NS: "alpha", Service: "web", Target: "a\nb", Port: 80},
		{NS: "alpha", Service: "web", Target: strings.Repeat("a", 254), Port: 80},
		{NS: "alpha", Service: "web", Target: "t", Port: 0},
		{NS: "alpha", Service: "web", Target: "t", Port: 65536},
		{NS: "alpha", Service: "web", Target: "t", Port: 80, Scheme: "ftp"},
	}
	for i, m := range bad {
		if err := s.PutManual(m); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d: %v", i, err)
		}
	}
	if err := s.DeleteManual("alpha", "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManual("alpha", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestBlockUnblockAndReplace(t *testing.T) {
	s, clk, _ := newStore(t)
	if err := s.Block("alpha", "web", "abuse", "alice"); err != nil {
		t.Fatal(err)
	}
	clk.Advance(time.Minute)
	if err := s.Block("alpha", "web", "again", "bob"); err != nil {
		t.Fatal(err)
	}
	s.Block("alpha", "api", "", "alice")
	bl, err := s.Blocks()
	if err != nil || len(bl) != 2 {
		t.Fatalf("%v %v", bl, err)
	}
	w := bl[1] // ordered by namespace, service: api then web
	if w.Service != "web" || w.Reason != "again" || w.Actor != "bob" || !w.At.Equal(time.Unix(1_000_060, 0)) {
		t.Fatalf("%+v", w)
	}
	if err := s.Unblock("alpha", "web"); err != nil {
		t.Fatal(err)
	}
	if err := s.Unblock("alpha", "web"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("got %v", err)
	}
	if err := s.Block("Alpha", "web", "", "a"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("got %v", err)
	}
}

func TestStateKeepsOnlyLatestAndSizeCap(t *testing.T) {
	s, clk, _ := newStore(t)
	if _, _, ok, err := s.LatestState(); ok || err != nil {
		t.Fatalf("empty: %v %v", ok, err)
	}
	if err := s.SaveState("n1", []byte(`{"a":1}`), clk.Now()); err != nil {
		t.Fatal(err)
	}
	later := clk.Now().Add(time.Hour)
	if err := s.SaveState("n2", []byte(`{"b":2}`), later); err != nil {
		t.Fatal(err)
	}
	body, at, ok, err := s.LatestState()
	if err != nil || !ok || string(body) != `{"b":2}` || !at.Equal(later) {
		t.Fatalf("%q %v %v %v", body, at, ok, err)
	}
	if err := s.SaveState("n1", make([]byte, 1<<20+1), clk.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversize: %v", err)
	}
	if err := s.SaveState("n1", make([]byte, 1<<20), clk.Now()); err != nil {
		t.Fatalf("1 MiB is fine: %v", err)
	}
	if err := s.SaveState("bad\nid", []byte("x"), clk.Now()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("node id: %v", err)
	}
}

func TestAuditTrimsToNewest1000AndOrder(t *testing.T) {
	s, clk, _ := newStore(t)
	for i := 0; i < 1005; i++ {
		clk.Advance(time.Second)
		if err := s.Audit("alice", "act", fmt.Sprintf("d%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	all, err := s.AuditLog(5000) // clamped to 1000
	if err != nil || len(all) != 1000 {
		t.Fatalf("%d %v", len(all), err)
	}
	if all[0].Detail != "d1004" || all[999].Detail != "d5" || all[0].Actor != "alice" || all[0].Action != "act" {
		t.Fatalf("%+v ... %+v", all[0], all[999])
	}
	if !all[0].At.After(all[1].At) {
		t.Fatal("not newest first")
	}
	three, _ := s.AuditLog(3)
	if len(three) != 3 || three[0].Detail != "d1004" {
		t.Fatalf("%+v", three)
	}
	if one, _ := s.AuditLog(0); len(one) != 1 {
		t.Fatalf("limit 0 clamps to 1, got %d", len(one))
	}
}

func TestAuditNewlinesFlattened(t *testing.T) {
	s, _, _ := newStore(t)
	if err := s.Audit("alice", "act", "line1\nline2\r\nline3"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.AuditLog(1)
	if got[0].Detail != "line1 line2  line3" {
		t.Fatalf("%q", got[0].Detail)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	clk := newClock()
	path := filepath.Join(t.TempDir(), "funnel.db")
	s, err := OpenSQLite(path, clk.Now)
	if err != nil {
		t.Fatal(err)
	}
	mustActive(t, s, "alpha", r(80, 80))
	s.SetPendingPorts("alpha", []Range{r(443, 443)}, 4)
	s.PutManual(ManualRoute{NS: "alpha", Service: "web", Target: "t", Port: 80})
	s.Block("alpha", "web", "why", "alice")
	s.SaveState("n1", []byte("{}"), clk.Now())
	s.Audit("alice", "act", "d")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2 := openAt(t, path, clk)
	n := get(t, s2, "alpha")
	if n.State != "active" || n.ChangeID != 4 || !reflect.DeepEqual(n.PendingPorts, []Range{r(443, 443)}) {
		t.Fatalf("%+v", n)
	}
	if m, _ := s2.ManualRoutes(); len(m) != 1 {
		t.Fatal("manual lost")
	}
	if b, _ := s2.Blocks(); len(b) != 1 {
		t.Fatal("block lost")
	}
	if _, _, ok, _ := s2.LatestState(); !ok {
		t.Fatal("state lost")
	}
	if a, _ := s2.AuditLog(10); len(a) != 1 {
		t.Fatal("audit lost")
	}
}

func TestConcurrentWritesRace(t *testing.T) {
	s, _, _ := newStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("ns%d", i)
			if err := s.CreateNamespace(name, []Range{r(80, 80)}, "", int64(i)); err != nil {
				t.Errorf("create: %v", err)
			}
			s.MarkActive(name)
			s.Audit("alice", "act", name)
			s.SaveState("n1", []byte("{}"), time.Now())
			s.Namespaces()
		}(i)
	}
	wg.Wait()
	if all, _ := s.Namespaces(); len(all) != 20 {
		t.Fatalf("got %d", len(all))
	}
	if a, _ := s.AuditLog(100); len(a) != 20 {
		t.Fatalf("audit %d", len(a))
	}
}

func TestCorruptDBErrorsAndFileUntouched(t *testing.T) {
	path := filepath.Join(t.TempDir(), "funnel.db")
	garbage := []byte(strings.Repeat("this is not a database\n", 200))
	if err := os.WriteFile(path, garbage, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSQLite(path, nil)
	if err == nil {
		s.Close()
		t.Fatal("expected an error")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil || !bytes.Equal(got, garbage) {
		t.Fatalf("file changed: %v", rerr)
	}
}

func TestErrorsWrapSentinels(t *testing.T) {
	s, _, _ := newStore(t)
	mustActive(t, s, "alpha", r(80, 80))
	s.PutManual(ManualRoute{NS: "alpha", Service: "web", Target: "t", Port: 80})
	checks := map[string]struct {
		err  error
		want error
	}{
		"notfound": {s.MarkActive("zzz"), ErrNotFound},
		"exists":   {s.CreateNamespace("alpha", []Range{r(1, 1)}, "", 1), ErrExists},
		"busy": {func() error {
			s.SetPendingPorts("alpha", []Range{r(2, 2)}, 1)
			return s.SetPendingPorts("alpha", []Range{r(3, 3)}, 2)
		}(), ErrBusy},
		"inuse":   {s.DeleteNamespace("alpha"), ErrInUse},
		"invalid": {s.CreateNamespace("", nil, "", 1), ErrInvalid},
	}
	for name, c := range checks {
		if !errors.Is(c.err, c.want) {
			t.Errorf("%s: %v is not %v", name, c.err, c.want)
		}
	}
	// Sentinels are distinct.
	all := []error{ErrNotFound, ErrExists, ErrBusy, ErrInUse, ErrInvalid}
	for i, a := range all {
		for j, b := range all {
			if i != j && errors.Is(a, b) {
				t.Errorf("%v is %v", a, b)
			}
		}
	}
}
