package tasks

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"
	"tailscale.com/ipn"

	"example.com/node-control/internal/statestore"
)

const storeScope = "node-control:tasks"

var storeKey = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// memKV is a plain in-memory ipn.StateStore whose writes can be made to fail.
type memKV struct {
	mu       sync.Mutex
	m        map[ipn.StateKey][]byte
	failNext int // writes that still fail

	failAll   error         // fails every write (after delay)
	delay     time.Duration // sleep per write
	attempts  int
	failField string        // always fails this field
	entered   chan struct{} // if set: signalled when a write starts, then it waits for release
	release   chan struct{}
}

func newMemKV() *memKV { return &memKV{m: map[ipn.StateKey][]byte{}} }

func (k *memKV) ReadState(id ipn.StateKey) ([]byte, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	b, ok := k.m[id]
	if !ok {
		return nil, ipn.ErrStateNotExist
	}
	return b, nil
}

func (k *memKV) WriteState(id ipn.StateKey, b []byte) error {
	if k.entered != nil {
		k.entered <- struct{}{}
		<-k.release
	}
	if k.delay > 0 {
		time.Sleep(k.delay)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	k.attempts++
	if k.failAll != nil {
		return k.failAll
	}
	if string(id) == k.failField {
		return errors.New("field refused")
	}
	if k.failNext > 0 {
		k.failNext--
		return errors.New("write refused")
	}
	k.m[id] = append([]byte(nil), b...)
	return nil
}

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(`{"tasks":{"ping":{"script":"ping.sh","target":{"node":"n1"},"schedule":{"every":"1h"},"secrets":["token"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func redisKV(t *testing.T) (ipn.StateStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rs, err := statestore.NewRedis("redis://"+mr.Addr(), storeScope)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rs.Close() })
	enc, err := statestore.NewEncrypted(rs, storeKey, storeScope)
	if err != nil {
		t.Fatal(err)
	}
	return enc, mr
}

func openStore(t *testing.T, kv ipn.StateStore) *Store {
	t.Helper()
	s, err := OpenStore(kv, quiet)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestStoreRoundTrip(t *testing.T) {
	kv, _ := redisKV(t)
	s := openStore(t, kv)
	if err := s.PutConfig(testConfig(t), map[string]string{"ping.sh": "echo hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutSecret("token", "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	st := TaskState{Streak: 2, NextRun: time.Unix(1000, 0).UTC()}
	if err := s.SaveState("ping", st); err != nil {
		t.Fatal(err)
	}

	r := openStore(t, kv)
	if r.Config() == nil || r.Config().Tasks["ping"].Script != "ping.sh" {
		t.Fatalf("config = %+v", r.Config())
	}
	if txt, ok := r.Script("ping.sh"); !ok || txt != "echo hi" {
		t.Fatalf("script = %q, %v", txt, ok)
	}
	got, err := r.Secrets([]string{"token"})
	if err != nil || got["token"] != "s3cret-value" {
		t.Fatalf("secrets = %v, %v", got, err)
	}
	if gs := r.State("ping"); gs.Streak != 2 || !gs.NextRun.Equal(st.NextRun) {
		t.Fatalf("state = %+v", gs)
	}
}

func TestStoreHistoryCap(t *testing.T) {
	kv, _ := redisKV(t)
	s := openStore(t, kv)
	// History is reloaded for the tasks of the stored config.
	if err := s.PutConfig(testConfig(t), nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 205; i++ {
		if err := s.AppendRun(Run{ID: fmt.Sprint(i), Task: "ping"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, st := range []*Store{s, openStore(t, kv)} {
		runs := st.Runs("ping")
		if len(runs) != 200 || runs[0].ID != "204" || runs[199].ID != "5" {
			t.Fatalf("len=%d first=%s last=%s", len(runs), runs[0].ID, runs[len(runs)-1].ID)
		}
	}
}

func TestStoreNeverReturnsSecretValuesInNames(t *testing.T) {
	s := openStore(t, newMemKV())
	s.PutSecret("b", "value-b-xyz")
	s.PutSecret("a", "value-a-xyz")
	names := s.SecretNames()
	if strings.Join(names, ",") != "a,b" {
		t.Fatalf("names = %v", names)
	}
	if strings.Contains(strings.Join(names, ""), "xyz") {
		t.Fatal("a value leaked into names")
	}
}

func TestStoreMissingSecretsListed(t *testing.T) {
	s := openStore(t, newMemKV())
	s.PutSecret("have", "hidden-value")
	_, err := s.Secrets([]string{"have", "gone1", "gone2"})
	if err == nil || !strings.Contains(err.Error(), "gone1") || !strings.Contains(err.Error(), "gone2") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "hidden-value") || strings.Contains(err.Error(), "have") {
		t.Fatalf("error names a present secret or value: %v", err)
	}
}

func TestStoreWriteFailureSetsDegradedAndKeepsMemory(t *testing.T) {
	kv := newMemKV()
	s := openStore(t, kv)
	kv.failNext = 1
	if err := s.SaveState("ping", TaskState{Streak: 7}); err == nil {
		t.Fatal("want the write error")
	}
	if !s.Degraded() {
		t.Fatal("not degraded after a failed write")
	}
	if got := s.State("ping").Streak; got != 7 {
		t.Fatalf("memory lost: streak = %d", got)
	}
	// The next successful write retries what failed and clears the flag.
	if err := s.PutSecret("token", "v1"); err != nil {
		t.Fatal(err)
	}
	if s.Degraded() {
		t.Fatal("still degraded after a successful write")
	}
	kv.mu.Lock()
	_, wrote := kv.m["state:ping"]
	kv.mu.Unlock()
	if !wrote {
		t.Fatal("failed write was not retried")
	}
}

func TestStoreOpenWithEmptyRedis(t *testing.T) {
	kv, _ := redisKV(t)
	s, err := OpenStore(kv, quiet)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config() != nil || s.Degraded() || len(s.SecretNames()) != 0 || len(s.Runs("x")) != 0 {
		t.Fatal("expected an empty store")
	}
}

func TestStoreSendsOnlyHGetHSet(t *testing.T) {
	mr := miniredis.RunT(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		mu.Lock()
		seen[strings.ToUpper(cmd)] = true
		mu.Unlock()
		return false
	})
	rs, err := statestore.NewRedis("redis://"+mr.Addr(), storeScope)
	if err != nil {
		t.Fatal(err)
	}
	defer rs.Close()
	kv, _ := statestore.NewEncrypted(rs, storeKey, storeScope)
	s := openStore(t, kv)
	s.PutConfig(testConfig(t), map[string]string{"ping.sh": "x"})
	s.PutSecret("token", "v")
	s.SaveState("ping", TaskState{Streak: 1})
	s.AppendRun(Run{ID: "1", Task: "ping"})
	openStore(t, kv)

	mu.Lock()
	defer mu.Unlock()
	for cmd := range seen {
		switch cmd {
		case "HGET", "HSET", "PING", "HELLO": // connection setup of the shared client (HELLO 2, PING), not store traffic
		default:
			t.Errorf("unexpected command %s", cmd)
		}
	}
	if !seen["HGET"] || !seen["HSET"] {
		t.Fatalf("seen = %v", seen)
	}
}

func TestStoreUpdateStateLosesNoUpdate(t *testing.T) {
	s := openStore(t, newMemKV())
	at := time.Unix(5000, 0).UTC()
	var wg sync.WaitGroup
	for i := 0; i < 200; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); s.UpdateState("ping", func(st *TaskState) { st.Streak++ }) }()
		go func() {
			defer wg.Done()
			s.UpdateState("ping", func(st *TaskState) { st.AlertUntil = at; st.Attempt++ })
		}()
	}
	wg.Wait()
	st := s.State("ping")
	if st.Streak != 200 || st.Attempt != 200 || !st.AlertUntil.Equal(at) {
		t.Fatalf("state = %+v", st)
	}
}

func TestStoreStuckFieldDoesNotBlockOthers(t *testing.T) {
	kv := newMemKV()
	kv.failField = "secret:bad"
	s := openStore(t, kv)
	if err := s.PutSecret("bad", "v"); err == nil {
		t.Fatal("want the write error")
	}
	if err := s.PutSecret("good", "v2"); err != nil {
		t.Fatalf("an unrelated stuck field leaked into this write's error: %v", err)
	}
	if err := s.SaveState("ping", TaskState{Streak: 3}); err != nil {
		t.Fatalf("unrelated stuck field leaked: %v", err)
	}
	if err := s.PutSecret("bad", "v3"); err == nil || !strings.Contains(err.Error(), "secret:bad") ||
		strings.Contains(err.Error(), "good") || strings.Contains(err.Error(), "state:") {
		t.Fatalf("own stuck field error = %v", err)
	}
	kv.mu.Lock()
	_, good := kv.m["secret:good"]
	_, state := kv.m["state:ping"]
	_, idx := kv.m["secrets-index"]
	kv.mu.Unlock()
	if !good || !state || !idx {
		t.Fatalf("other fields blocked: good=%v state=%v index=%v", good, state, idx)
	}
	if !s.Degraded() {
		t.Fatal("not degraded")
	}
	if f := s.DegradedFields(); len(f) != 1 || f[0] != "secret:bad" {
		t.Fatalf("DegradedFields = %v", f)
	}
	kv.mu.Lock()
	kv.failField = ""
	kv.mu.Unlock()
	if err := s.SaveState("ping", TaskState{Streak: 4}); err != nil {
		t.Fatal(err)
	}
	if s.Degraded() || len(s.DegradedFields()) != 0 {
		t.Fatal("still degraded after recovery")
	}
}

func TestStoreRunBounds(t *testing.T) {
	s := openStore(t, newMemKV())
	long := strings.Repeat("é", 600)
	s.AppendRun(Run{ID: "a", Task: "t", Reason: long})
	got := s.Runs("t")[0].Reason
	if len(got) > maxReasonBytes || !strings.HasPrefix(long, got) {
		t.Fatalf("reason len = %d", len(got))
	}

	big := map[string]any{"blob": strings.Repeat("x", 9<<10)}
	s.AppendRun(Run{ID: "b", Task: "t", Result: big})
	if r := s.Runs("t")[0]; r.Result["truncated"] != true || len(r.Result) != 1 {
		t.Fatalf("result = %v", r.Result)
	}
	s.AppendRun(Run{ID: "c", Task: "t", Result: map[string]any{"ok": 1}})
	if r := s.Runs("t")[0]; r.Result["ok"] != 1 {
		t.Fatalf("small result altered: %v", r.Result)
	}
}

func TestStoreRunsFieldCappedOldestFirst(t *testing.T) {
	kv := newMemKV()
	s := openStore(t, kv)
	s.PutConfig(testConfig(t), nil)
	// ~7.5 KiB each: 60 of them exceed 256 KiB.
	for i := 0; i < 60; i++ {
		s.AppendRun(Run{ID: fmt.Sprint(i), Task: "ping", Result: map[string]any{"blob": strings.Repeat("y", 7<<10)}})
	}
	kv.mu.Lock()
	n := len(kv.m["runs:ping"])
	kv.mu.Unlock()
	if n > maxRunsField {
		t.Fatalf("runs field = %d bytes", n)
	}
	runs := s.Runs("ping")
	if len(runs) >= 60 || len(runs) < 10 || runs[0].ID != "59" {
		t.Fatalf("len=%d newest=%s", len(runs), runs[0].ID)
	}
	if last := runs[len(runs)-1].ID; last == "0" {
		t.Fatal("oldest run was kept")
	}
	if r := openStore(t, kv).Runs("ping"); len(r) != len(runs) || r[0].ID != "59" {
		t.Fatalf("reopened len=%d", len(r))
	}
}

func TestStoreAppendRunUpsertsByID(t *testing.T) {
	kv := newMemKV()
	s := openStore(t, kv)
	s.PutConfig(testConfig(t), nil)
	s.AppendRun(Run{ID: "1", Task: "ping", State: RunQueued})
	s.AppendRun(Run{ID: "2", Task: "ping", State: RunQueued})
	s.AppendRun(Run{ID: "1", Task: "ping", State: RunSucceeded, RunnerID: "r1", RunnerAddr: "100.64.0.1"})
	for _, st := range []*Store{s, openStore(t, kv)} {
		runs := st.Runs("ping")
		if len(runs) != 2 || runs[0].ID != "2" || runs[1].ID != "1" || runs[1].State != RunSucceeded ||
			runs[1].RunnerID != "r1" || runs[1].RunnerAddr != "100.64.0.1" {
			t.Fatalf("runs = %+v", runs)
		}
	}
}

func TestStorePutConfigNil(t *testing.T) {
	s := openStore(t, newMemKV())
	if err := s.PutConfig(nil, nil); err == nil || s.Config() != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestStoreReadersDoNotWaitForRedis(t *testing.T) {
	kv := newMemKV()
	kv.entered, kv.release = make(chan struct{}, 8), make(chan struct{})
	s := openStore(t, kv)

	done := make(chan error, 1)
	go func() { done <- s.PutSecret("token", "v") }()
	<-kv.entered // a write is now blocked inside the kv

	reads := make(chan struct{})
	go func() {
		s.Config()
		s.State("ping")
		s.Runs("ping")
		s.SecretNames()
		s.Degraded()
		close(reads)
	}()
	select {
	case <-reads:
	case <-time.After(2 * time.Second):
		t.Fatal("readers blocked behind a Redis write")
	}
	close(kv.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

func queueMany(s *Store, n int) {
	s.mu.Lock()
	for i := 0; i < n; i++ {
		s.queue(fmt.Sprintf("state:q%d", i), []byte("{}"))
	}
	s.mu.Unlock()
}

func TestStoreFlushBudgetBoundsWriterLatency(t *testing.T) {
	kv := newMemKV()
	kv.delay = 40 * time.Millisecond
	kv.failAll = errors.New("ERR rejected") // slow per-field rejections: no early abort
	s := openStore(t, kv)
	s.flushBudget = 120 * time.Millisecond
	queueMany(s, 30)

	start := time.Now()
	err := s.SaveState("ping", TaskState{Streak: 1})
	if took := time.Since(start); took > time.Second {
		t.Fatalf("writer blocked %v for 31 pending fields", took)
	}
	if err == nil {
		t.Fatal("own field was not written: want an error")
	}
	kv.mu.Lock()
	tried := kv.attempts
	kv.failAll = nil
	kv.delay = 0
	kv.mu.Unlock()
	if tried >= 31 {
		t.Fatalf("budget did not stop the flush: %d attempts", tried)
	}
	if err := s.SaveState("ping", TaskState{Streak: 2}); err != nil {
		t.Fatal(err)
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if len(kv.m) != 31 || s.Degraded() {
		t.Fatalf("unattempted fields not written later: %d stored, degraded=%v", len(kv.m), s.Degraded())
	}
}

func TestStoreTransportFailureAbortsFlush(t *testing.T) {
	kv := newMemKV()
	kv.failAll = timeoutErr{}
	s := openStore(t, kv)
	queueMany(s, 20)
	if err := s.SaveState("ping", TaskState{}); err == nil {
		t.Fatal("want an error")
	}
	kv.mu.Lock()
	n := kv.attempts
	kv.failAll = nil
	kv.mu.Unlock()
	if n != 1 {
		t.Fatalf("%d attempts after a transport failure, want 1", n)
	}
	if err := s.SaveState("ping", TaskState{Streak: 1}); err != nil {
		t.Fatal(err)
	}
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if len(kv.m) != 21 {
		t.Fatalf("%d stored, want 21", len(kv.m))
	}
}

func TestStoreFlattenedTransportTextAbortsFlush(t *testing.T) {
	// The shared client turns go-redis errors into plain text.
	if !isTransportErr(errors.New("writing state to redis: dial tcp 10.0.0.1:6379: i/o timeout")) ||
		isTransportErr(errors.New("writing state to redis: OOM command not allowed when used memory > 'maxmemory'")) {
		t.Fatal("transport detection")
	}
}
