package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var multiKey = []byte("0123456789abcdef-multi-test-key")

func twoIngressCfg() IngressConfig {
	return IngressConfig{Ingresses: []Ingress{
		{Name: "ingress-1", UUIDs: uuidsBase},
		{Name: "ingress-2", UUIDs: uuidsDerived},
	}}.WithKey(multiKey)
}

func targetsFor(cfg IngressConfig, agents ...Agent) []SyncTarget {
	var ts []SyncTarget
	for i, ing := range cfg.Ingresses {
		ts = append(ts, SyncTarget{Name: ing.Name, Agent: agents[i], State: NewSyncState(nil), Ingress: ing})
	}
	return ts
}

func uuidSet(cs []Credential) map[string]bool {
	m := map[string]bool{}
	for _, c := range cs {
		m[c.UUID] = true
	}
	return m
}

func baseIngressCfg(n int) IngressConfig {
	var c IngressConfig
	for i := 0; i < n; i++ {
		c.Ingresses = append(c.Ingresses, Ingress{Name: "ingress-" + string(rune('1'+i)), UUIDs: uuidsBase})
	}
	return c
}

func TestSyncAllPushesSameBaseSetToBaseIngresses(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	ts := targetsFor(cfg, a1, a2)
	if !SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want true")
	}
	base, _ := e.store.ActiveCredentials()
	for i, a := range []*fakeAgent{a1, a2} {
		if a.count() != 1 || len(a.pushes[0].creds) != len(base) || len(base) == 0 {
			t.Fatalf("agent %d: %+v", i, a.pushes)
		}
		for j, c := range base {
			if a.pushes[0].creds[j] != c {
				t.Fatalf("agent %d differs at %d", i, j)
			}
		}
	}
}

func TestSyncAllPushesIsolatedCredentialSets(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	e.make("b", 0)
	cfg := twoIngressCfg()
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	if !SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, a1, a2)) {
		t.Fatal("want true")
	}
	base, _ := e.store.ActiveCredentials()
	want2, err := cfg.CredentialsFor(cfg.Ingresses[1], base)
	if err != nil {
		t.Fatal(err)
	}
	got1, got2 := a1.pushes[0].creds, a2.pushes[0].creds
	if len(got2) != len(want2) {
		t.Fatalf("len %d vs %d", len(got2), len(want2))
	}
	for i := range want2 {
		if got2[i] != want2[i] {
			t.Fatalf("derived set differs from CredentialsFor at %d", i)
		}
	}
	s1 := uuidSet(got1)
	for u := range uuidSet(got2) {
		if s1[u] {
			t.Fatal("a UUID is shared between the two ingresses")
		}
	}
	for _, c := range base {
		if uuidSet(got2)[c.UUID] {
			t.Fatal("a base UUID reached the derived ingress")
		}
	}
	for i := range got2 {
		if got2[i].Mode != base[i].Mode || got2[i].NotAfter != base[i].NotAfter {
			t.Fatal("mode or not_after changed")
		}
	}
}

func TestSyncAllOneUnreachableDoesNotAffectOthers(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	a1 := &fakeAgent{fail: &AgentUnavailable{Outcome: OutcomeUnreachable}}
	a2 := &fakeAgent{}
	var now int64 = 100
	ts := targetsFor(cfg, a1, a2)
	ts[0].State, ts[1].State = fixedState(&now), fixedState(&now)
	if SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want false")
	}
	if s := ts[1].State.Snapshot(); s.Pending || s.State != "ok" {
		t.Fatalf("healthy: %+v", s)
	}
	s := ts[0].State.Snapshot()
	if !s.Pending || s.State != "unreachable" || s.Since == nil || *s.Since != 100 {
		t.Fatalf("lagging: %+v", s)
	}
	if a2.count() != 1 {
		t.Fatal("healthy ingress was not pushed")
	}
	atomic.StoreInt64(&now, 500)
	SyncAll(context.Background(), e.store, cfg, ts)
	if s := ts[0].State.Snapshot(); *s.Since != 100 {
		t.Fatalf("since moved: %+v", s)
	}
	if s := ts[1].State.Snapshot(); s.Since != nil {
		t.Fatalf("healthy since: %+v", s)
	}
}

func TestSyncAllWrongTokenIsolated(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	var good atomic.Int32
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer good" {
			good.Add(1)
			return
		}
		w.WriteHeader(401)
	}))
	defer ok.Close()
	ts := targetsFor(cfg, NewAgent(ok.URL, "good", nil), NewAgent(ok.URL, "bad", nil))
	if SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want false")
	}
	if s := ts[0].State.Snapshot(); s.Pending || s.State != "ok" {
		t.Fatalf("%+v", s)
	}
	if s := ts[1].State.Snapshot(); !s.Pending || s.State != "unauthorized" {
		t.Fatalf("%+v", s)
	}
	if good.Load() != 1 {
		t.Fatal("good ingress not pushed once")
	}
}

func TestSyncAllNewTargetGetsFullSet(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	e.make("b", 0)
	cfg := twoIngressCfg()
	a1 := &fakeAgent{}
	ts := targetsFor(cfg, a1, &fakeAgent{})
	SyncAll(context.Background(), e.store, cfg, ts[:1])
	a2 := &fakeAgent{}
	ts[1].Agent = a2
	SyncAll(context.Background(), e.store, cfg, ts)
	base, _ := e.store.ActiveCredentials()
	want, err := cfg.CredentialsFor(cfg.Ingresses[1], base)
	if err != nil || len(want) == 0 {
		t.Fatal(err)
	}
	if a2.count() != 1 || len(a2.pushes[0].creds) != len(want) {
		t.Fatalf("new target: %+v", a2.pushes)
	}
	for i, c := range want {
		if a2.pushes[0].creds[i] != c {
			t.Fatalf("credential %d differs: %+v vs %+v", i, a2.pushes[0].creds[i], c)
		}
	}
}

func TestSyncAllEmptyStoreNeverPushedToAnyTarget(t *testing.T) {
	e := newEnv(t)
	cfg := twoIngressCfg()
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	ts := targetsFor(cfg, a1, a2)
	if !SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want true")
	}
	if a1.count() != 0 || a2.count() != 0 {
		t.Fatal("empty store was pushed")
	}
	for _, x := range ts {
		if s := x.State.Snapshot(); s.Pending || s.State != "empty" {
			t.Fatalf("%+v", s)
		}
	}
}

func TestSyncAllConfirmEmptyWhenAllRevoked(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	if err := e.store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	cfg := twoIngressCfg()
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	if !SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, a1, a2)) {
		t.Fatal("want true")
	}
	for _, a := range []*fakeAgent{a1, a2} {
		if a.count() != 1 || len(a.pushes[0].creds) != 0 || !a.pushes[0].confirmEmpty {
			t.Fatalf("%+v", a.pushes)
		}
	}
}

func TestSyncAllUnreadableStoreFailsAllTargets(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	e.store.Close()
	cfg := twoIngressCfg()
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	ts := targetsFor(cfg, a1, a2)
	if SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want false")
	}
	if a1.count()+a2.count() != 0 {
		t.Fatal("pushed after read error")
	}
	for _, x := range ts {
		if s := x.State.Snapshot(); !s.Pending || s.State != "error" {
			t.Fatalf("%+v", s)
		}
	}
}

func TestSyncAllRacingCallsEndWithNewestSet(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	cfg := twoIngressCfg()
	started, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	a1.onPut = func() {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-release
		}
	}
	ts := targetsFor(cfg, a1, a2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); SyncAll(context.Background(), e.store, cfg, ts) }()
	<-started
	if err := e.store.Revoke(d.ID); err != nil {
		t.Fatal(err)
	}
	go func() { defer wg.Done(); SyncAll(context.Background(), e.store, cfg, ts) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, a := range []*fakeAgent{a1, a2} {
		if a.count() != 2 || len(a.pushes[0].creds) != 3 || len(a.pushes[1].creds) != 0 {
			t.Fatalf("agent %d pushes %+v", i, a.pushes)
		}
	}
}

func TestSyncAllPushesAreConcurrent(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	// each push waits until BOTH pushes are in flight; sequential pushes never meet and time out
	var arrived sync.WaitGroup
	arrived.Add(2)
	met := make(chan struct{})
	go func() { arrived.Wait(); close(met) }()
	var timedOut atomic.Bool
	mk := func() *fakeAgent {
		return &fakeAgent{onPut: func() {
			arrived.Done()
			select {
			case <-met:
			case <-time.After(5 * time.Second):
				timedOut.Store(true)
			}
		}}
	}
	SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, mk(), mk()))
	if timedOut.Load() {
		t.Fatal("pushes ran one after the other")
	}
}

func TestSummaryWorstStateAndEarliestSince(t *testing.T) {
	mk := func(clock int64, fail ...string) SyncTarget {
		st := NewSyncState(func() time.Time { return time.Unix(clock, 0) })
		for _, f := range fail {
			if f == "ok" || f == "empty" {
				st.OK(f)
			} else {
				st.Failed(f)
			}
		}
		return SyncTarget{State: st}
	}
	t.Run("all fine", func(t *testing.T) {
		got := Summary([]SyncTarget{mk(1, "empty"), mk(2, "ok")})
		if got.Pending || got.State != "ok" || got.Since != nil {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("worst wins, earliest since", func(t *testing.T) {
		got := Summary([]SyncTarget{mk(50, "unreachable"), mk(30, "unauthorized"), mk(10, "ok"), mk(5, "conflict")})
		if !got.Pending || got.State != "unauthorized" || got.Since == nil || *got.Since != 5 {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("order", func(t *testing.T) {
		order := []string{"error", "unauthorized", "rejected", "conflict", "unreachable"}
		for i := range order {
			var ts []SyncTarget
			for j := len(order) - 1; j >= i; j-- {
				ts = append(ts, mk(1, order[j]))
			}
			if got := Summary(ts); got.State != order[i] {
				t.Fatalf("want %s got %s", order[i], got.State)
			}
		}
	})
	t.Run("unknown beats ok", func(t *testing.T) {
		got := Summary([]SyncTarget{mk(1, "ok"), {State: NewSyncState(nil)}})
		if got.State != "unknown" || got.Pending {
			t.Fatalf("%+v", got)
		}
	})
}

type statsAgent struct {
	fakeAgent
	answer map[string]any
}

func (a *statsAgent) Stats(ctx context.Context) map[string]any { return a.answer }

func opaqueFor(t *testing.T, cfg IngressConfig, ing Ingress, d Device) string {
	t.Helper()
	u, err := cfg.UUIDFor(ing, "auto", d.UUIDs["auto"])
	if err != nil {
		t.Fatal(err)
	}
	return opaqueID(Device{UUIDs: map[string]string{"auto": u}})
}

func TestStatsMergedPerDeviceAcrossDerivedIds(t *testing.T) {
	e := newEnv(t)
	d1, _ := e.make("a", 0)
	d2, _ := e.make("b", 0)
	cfg := twoIngressCfg()
	a1 := &statsAgent{answer: map[string]any{
		opaqueFor(t, cfg, cfg.Ingresses[0], d1): map[string]any{"up": float64(10), "down": float64(100)},
	}}
	a2 := &statsAgent{answer: map[string]any{
		opaqueFor(t, cfg, cfg.Ingresses[1], d1): map[string]any{"up": int64(1), "down": float64(2)},
		opaqueFor(t, cfg, cfg.Ingresses[1], d2): map[string]any{"up": float64(7), "down": float64(8)},
	}}
	ds, _ := e.store.ListDevices()
	got, partial := MergedStats(context.Background(), targetsFor(cfg, a1, a2), cfg, ds)
	if partial {
		t.Fatal("not partial")
	}
	if got[d1.ID] != (Traffic{11, 102}) || got[d2.ID] != (Traffic{7, 8}) || len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	if opaqueFor(t, cfg, cfg.Ingresses[0], d1) == opaqueFor(t, cfg, cfg.Ingresses[1], d1) {
		t.Fatal("test needs different ids")
	}
}

func TestMergedStatsPartialWhenAgentDown(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	cfg := baseIngressCfg(2)
	id := opaqueFor(t, cfg, cfg.Ingresses[0], d)
	a1 := &statsAgent{answer: map[string]any{id: map[string]any{"up": float64(1), "down": float64(2)}}}
	a2 := &statsAgent{answer: map[string]any{}}
	ts := targetsFor(cfg, a1, a2)
	ds, _ := e.store.ListDevices()
	t.Run("pending and empty is partial", func(t *testing.T) {
		ts[1].State.Failed("unreachable")
		got, partial := MergedStats(context.Background(), ts, cfg, ds)
		if !partial || got[d.ID] != (Traffic{1, 2}) {
			t.Fatalf("%v %+v", partial, got)
		}
	})
	t.Run("healthy and idle is not partial", func(t *testing.T) {
		ts[1].State.OK("ok")
		if _, partial := MergedStats(context.Background(), ts, cfg, ds); partial {
			t.Fatal("partial")
		}
	})
}

func TestMergedStatsIgnoresMalformedEntries(t *testing.T) {
	e := newEnv(t)
	var devs []Device
	for _, n := range []string{"a", "b", "c", "d", "e", "f"} {
		d, _ := e.make(n, 0)
		devs = append(devs, d)
	}
	cfg := baseIngressCfg(1)
	ans := map[string]any{
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[0]): "text",
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[1]): map[string]any{"up": "1", "down": float64(2)},
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[2]): map[string]any{"up": float64(1)},
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[3]): []any{1, 2},
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[4]): nil,
		opaqueFor(t, cfg, cfg.Ingresses[0], devs[5]): map[string]any{"up": float64(3), "down": float64(4)},
		"unknown-id": map[string]any{"up": float64(9), "down": float64(9)},
	}
	ds, _ := e.store.ListDevices()
	got, _ := MergedStats(context.Background(), targetsFor(cfg, &statsAgent{answer: ans}), cfg, ds)
	if len(got) != 1 || got[devs[5].ID] != (Traffic{3, 4}) {
		t.Fatalf("%+v", got)
	}
}

func TestAgentFuncResolvesEachCall(t *testing.T) {
	var hits [2]atomic.Int32
	mk := func(i int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits[i].Add(1)
			if r.URL.Path == "/v1/stats" {
				w.Write([]byte(`{"x":{"up":1,"down":2}}`))
			}
		}))
	}
	s0, s1 := mk(0), mk(1)
	defer s0.Close()
	defer s1.Close()
	cur := s0.URL
	var failResolve atomic.Bool
	ag := NewAgentFunc(func(context.Context) (string, error) {
		if failResolve.Load() {
			return "", errors.New("secret-resolver-detail")
		}
		return cur, nil
	}, "tok", nil)
	if err := ag.PutCredentials(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	cur = s1.URL
	if err := ag.PutCredentials(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	if len(ag.Stats(context.Background())) != 1 || hits[0].Load() != 1 || hits[1].Load() != 2 {
		t.Fatalf("hits %d %d", hits[0].Load(), hits[1].Load())
	}
	failResolve.Store(true)
	err := ag.PutCredentials(context.Background(), nil, true)
	var au *AgentUnavailable
	if !errors.As(err, &au) || au.Outcome != OutcomeUnreachable || strings.Contains(err.Error(), "secret") {
		t.Fatalf("%v", err)
	}
	if len(ag.Stats(context.Background())) != 0 {
		t.Fatal("stats on resolve error")
	}
}

func TestSyncLogsNameNoSecrets(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := twoIngressCfg()
	buf := captureLogs(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	ts := targetsFor(cfg, NewAgent(srv.URL, "TOKEN-ONE-xyz", nil), NewAgent(srv.URL, "TOKEN-TWO-xyz", nil))
	SyncAll(context.Background(), e.store, cfg, ts)
	base, _ := e.store.ActiveCredentials()
	der, _ := cfg.CredentialsFor(cfg.Ingresses[1], base)
	out := buf.String()
	if !strings.Contains(out, "ingress-1") || !strings.Contains(out, "ingress-2") {
		t.Fatalf("names missing: %s", out)
	}
	for _, c := range append(base, der...) {
		if strings.Contains(out, c.UUID) {
			t.Fatal("uuid in logs")
		}
	}
	if strings.Contains(out, "TOKEN-") {
		t.Fatal("token in logs")
	}
	// one-target wrapper keeps its wording without an ingress attribute
	buf.Reset()
	Sync(context.Background(), e.store, NewAgent(srv.URL, "x", nil), NewSyncState(nil))
	if strings.Contains(buf.String(), "ingress=") {
		t.Fatalf("wrapper log changed: %s", buf.String())
	}
}

func TestReconcileLoopAllUsesCurrentTargets(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	defer setReconcileForTest(10*time.Millisecond, 10*time.Millisecond)()
	cfg := baseIngressCfg(2)
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	all := targetsFor(cfg, a1, a2)
	var n atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		ReconcileLoopAll(ctx, e.store, cfg, func() []SyncTarget {
			if n.Add(1) == 1 {
				return all[:1]
			}
			return all
		})
		close(done)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for a2.count() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if a1.count() < 2 || a2.count() == 0 {
		t.Fatalf("%d %d", a1.count(), a2.count())
	}
}

func TestSyncAllCredentialPanicIsolated(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	credentialsHook = func(ing Ingress) {
		if ing.Name == "ingress-1" {
			panic("boom-" + ing.Name)
		}
	}
	defer func() { credentialsHook = nil }()
	buf := captureLogs(t)
	a1, a2 := &fakeAgent{}, &fakeAgent{}
	ts := targetsFor(cfg, a1, a2)
	if SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want false")
	}
	if s := ts[0].State.Snapshot(); !s.Pending || s.State != "error" || a1.count() != 0 {
		t.Fatalf("%+v", s)
	}
	if s := ts[1].State.Snapshot(); s.Pending || s.State != "ok" || a2.count() != 1 {
		t.Fatalf("%+v", s)
	}
	if strings.Contains(buf.String(), "boom-") {
		t.Fatal("panic value in logs")
	}
	if !syncMu.TryLock() {
		t.Fatal("lock still held")
	}
	syncMu.Unlock()
}

type panicAgent struct{ fakeAgent }

func (a *panicAgent) PutCredentials(context.Context, []Credential, bool) error { panic("x") }
func (a *panicAgent) Stats(context.Context) map[string]any                     { panic("x") }

func TestSyncAllPanickingAgentIsolated(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	a2 := &fakeAgent{}
	ts := targetsFor(cfg, &panicAgent{}, a2)
	if SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want false")
	}
	if s := ts[0].State.Snapshot(); !s.Pending || s.State != "error" {
		t.Fatalf("%+v", s)
	}
	if s := ts[1].State.Snapshot(); s.Pending || a2.count() != 1 {
		t.Fatalf("%+v", s)
	}
}

func TestMergedStatsPanickingAgentRecovered(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	cfg := baseIngressCfg(2)
	id := opaqueFor(t, cfg, cfg.Ingresses[0], d)
	good := &statsAgent{answer: map[string]any{id: map[string]any{"up": float64(1), "down": float64(2)}}}
	ts := targetsFor(cfg, &panicAgent{}, good)
	ds, _ := e.store.ListDevices()
	got, partial := MergedStats(context.Background(), ts, cfg, ds)
	if !partial || got[d.ID] != (Traffic{1, 2}) {
		t.Fatalf("%v %+v", partial, got)
	}
}

func TestSyncAllCancelledContextReturnsPromptly(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := baseIngressCfg(2)
	hang := make(chan struct{})
	defer close(hang)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-hang:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	ts := targetsFor(cfg, NewAgent(srv.URL, "t", nil), NewAgent(srv.URL, "t", nil))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan bool)
	go func() { done <- SyncAll(ctx, e.store, cfg, ts) }()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("want false")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SyncAll hangs on a cancelled context")
	}
	for _, x := range ts {
		if s := x.State.Snapshot(); !s.Pending || (s.State != "unreachable" && s.State != "error") {
			t.Fatalf("%+v", s)
		}
	}
}

func TestMergedStatsBoundsHostileValues(t *testing.T) {
	const max = 9007199254740992.0
	e := newEnv(t)
	d1, _ := e.make("a", 0)
	d2, _ := e.make("b", 0)
	cfg := baseIngressCfg(2)
	id1, id2 := opaqueFor(t, cfg, cfg.Ingresses[0], d1), opaqueFor(t, cfg, cfg.Ingresses[0], d2)
	ans := func() map[string]any {
		return map[string]any{
			id1: map[string]any{"up": max, "down": max},
			id2: map[string]any{"up": 1e308, "down": max + 2}, // both ignored: the entry is dropped
		}
	}
	ts := targetsFor(cfg, &statsAgent{answer: ans()}, &statsAgent{answer: ans()})
	ds, _ := e.store.ListDevices()
	got, _ := MergedStats(context.Background(), ts, cfg, ds)
	if got[d1.ID] != (Traffic{max, max}) {
		t.Fatalf("not clamped: %+v", got[d1.ID])
	}
	if _, ok := got[d2.ID]; ok {
		t.Fatalf("hostile entry accepted: %+v", got[d2.ID])
	}
	if _, err := json.Marshal(got); err != nil {
		t.Fatal(err)
	}
	t.Run("exactly the maximum is accepted", func(t *testing.T) {
		one, _ := MergedStats(context.Background(), ts[:1], cfg, ds)
		if one[d1.ID] != (Traffic{max, max}) {
			t.Fatalf("%+v", one[d1.ID])
		}
	})
	t.Run("one value over the maximum drops the entry", func(t *testing.T) {
		a := &statsAgent{answer: map[string]any{id1: map[string]any{"up": float64(5), "down": max * 2}}}
		r, _ := MergedStats(context.Background(), targetsFor(cfg, a, &statsAgent{}), cfg, ds)
		if len(r) != 0 {
			t.Fatalf("%+v", r)
		}
	})
}

func TestMergedStatsRejectsNaNInfNegative(t *testing.T) {
	e := newEnv(t)
	d, _ := e.make("a", 0)
	cfg := baseIngressCfg(1)
	id := opaqueFor(t, cfg, cfg.Ingresses[0], d)
	ds, _ := e.store.ListDevices()
	for name, v := range map[string]float64{"nan": math.NaN(), "inf": math.Inf(1), "ninf": math.Inf(-1), "negative": -1} {
		t.Run(name, func(t *testing.T) {
			a := &statsAgent{answer: map[string]any{id: map[string]any{"up": v, "down": float64(1)}}}
			got, _ := MergedStats(context.Background(), targetsFor(cfg, a), cfg, ds)
			if len(got) != 0 {
				t.Fatalf("%+v", got)
			}
		})
	}
}
