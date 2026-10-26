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
	k.mu.Lock()
	defer k.mu.Unlock()
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
		case "HGET", "HSET", "PING", "HELLO": // connection setup of the shared client, not store traffic
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
