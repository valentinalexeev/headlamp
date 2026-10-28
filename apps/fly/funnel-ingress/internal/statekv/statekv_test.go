package statekv

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/alicebob/miniredis/v2/server"

	"funnel-ingress/internal/config"
)

const redisKey = "funnel-ingress:tsnet"

func stateKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func redisCfg(mr *miniredis.Miniredis) config.Config {
	return config.Config{RedisURL: "redis://:topsecret@" + mr.Addr(), RedisKey: redisKey, StateKey: stateKey()}
}

func TestOnlyAllowedRedisCommandsAreSent(t *testing.T) {
	mr := miniredis.RunT(t)
	var mu sync.Mutex
	seen := map[string]bool{}
	mr.Server().SetPreHook(func(_ *server.Peer, cmd string, _ ...string) bool {
		mu.Lock()
		seen[strings.ToUpper(cmd)] = true
		mu.Unlock()
		return false
	})
	st, err := Open(context.Background(), redisCfg(mr))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	p := NewPersister(st.KV, RoutesField)
	if err := p.Save([]byte(`[]`)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Load(); err != nil {
		t.Fatal(err)
	}
	if err := st.TSNet.WriteState("_machinekey", []byte("k")); err != nil {
		t.Fatal(err)
	}
	if err := NewBlob(st.KV, ConfigField).Save(nil); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"HELLO": true, "PING": true, "HGET": true, "HSET": true, "HDEL": true, "HLEN": true, "HEXISTS": true, "HKEYS": true}
	mu.Lock()
	defer mu.Unlock()
	var got []string
	for c := range seen {
		got = append(got, c)
		if !allowed[c] {
			t.Errorf("command %s is not in the allowed set", c)
		}
	}
	sort.Strings(got)
	t.Logf("commands seen: %v", got)
}

func TestOpenRedisDownFailsFastNoNewIdentity(t *testing.T) {
	mr := miniredis.RunT(t)
	cfg := redisCfg(mr)
	mr.Close()
	st, err := Open(context.Background(), cfg)
	if err == nil {
		st.Close()
		t.Fatal("opened with Redis down")
	}
	if st.TSNet != nil || st.KV != nil {
		t.Fatal("stores returned alongside an error")
	}
}

func TestRedisModeSharesOneHashForTSNetAndKV(t *testing.T) {
	mr := miniredis.RunT(t)
	st, err := Open(context.Background(), redisCfg(mr))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.TSNet.WriteState("_machinekey", []byte("machine")); err != nil {
		t.Fatal(err)
	}
	if err := NewPersister(st.KV, RoutesField).Save([]byte(`[1]`)); err != nil {
		t.Fatal(err)
	}
	if err := NewBlob(st.KV, ConfigField).Save([]byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	fields, err := mr.HKeys(redisKey)
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(fields)
	if strings.Join(fields, ",") != "_machinekey,funnel/config,funnel/routes" {
		t.Fatalf("fields = %v", fields)
	}
	if v := mr.HGet(redisKey, RoutesField); strings.Contains(v, "[1]") {
		t.Fatal("routes are stored in plaintext")
	}
	got, ok, err := NewPersister(st.KV, RoutesField).Load()
	if err != nil || !ok || string(got) != "[1]" {
		t.Fatalf("round trip: %q %v %v", got, ok, err)
	}
	if len(mr.Keys()) != 1 {
		t.Fatalf("more than one Redis key: %v", mr.Keys())
	}
}

func TestFileModeKVRoundtripAndNoRedis(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{StateDir: dir}
	st, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st.TSNet != nil {
		t.Fatal("file mode must leave the tsnet store to tsnet")
	}
	p := NewPersister(st.KV, RoutesField)
	if _, ok, err := p.Load(); err != nil || ok {
		t.Fatalf("empty store: ok=%v err=%v", ok, err)
	}
	if err := p.Save([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	st.Close()
	st2, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, ok, err := NewPersister(st2.KV, RoutesField).Load()
	if err != nil || !ok || string(got) != "hello" {
		t.Fatalf("after reopen: %q %v %v", got, ok, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, "funnel-kv.json")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("kv file: %v %v", fi, err)
	}
}

func TestFileModeCorruptFileIsNotOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "funnel-kv.json")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(context.Background(), config.Config{StateDir: dir}); err == nil {
		t.Fatal("opened a corrupt file")
	}
	if b, _ := os.ReadFile(path); string(b) != "garbage" {
		t.Fatal("corrupt file was modified")
	}
}

func TestPlaintextRequiresExplicitOptIn(t *testing.T) {
	// config.Load refuses REDIS_URL without a key unless STATE_ALLOW_PLAINTEXT=1; with the opt-in the
	// values reach Redis unencrypted.
	env := func(k string) string {
		return map[string]string{"FUNNEL_CONFIG_FILE": "/c.json", "TS_AUTHKEY": "k", "REDIS_URL": "redis://h:1"}[k]
	}
	if _, err := config.Load(env); err == nil {
		t.Fatal("redis without a key accepted")
	}
	mr := miniredis.RunT(t)
	cfg := config.Config{RedisURL: "redis://" + mr.Addr(), RedisKey: redisKey, AllowPlaintext: true}
	st, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := NewBlob(st.KV, ConfigField).Save([]byte("visible")); err != nil {
		t.Fatal(err)
	}
	if mr.HGet(redisKey, ConfigField) != "visible" {
		t.Fatal("opt-in store should hold plaintext")
	}
}

func TestKeyAndURLNeverInErrors(t *testing.T) {
	cfg := config.Config{RedisURL: "redis://:topsecret@127.0.0.1:1", RedisKey: redisKey, StateKey: stateKey()}
	_, err := Open(context.Background(), cfg)
	if err == nil {
		t.Fatal("unreachable redis accepted")
	}
	for _, s := range []string{"topsecret", cfg.RedisURL, cfg.StateKey} {
		if strings.Contains(err.Error(), s) {
			t.Errorf("error leaks %q: %v", s, err)
		}
	}
	cfg.StateKey = "not-a-key"
	mr := miniredis.RunT(t)
	cfg.RedisURL = "redis://:topsecret@" + mr.Addr()
	_, err = Open(context.Background(), cfg)
	if err == nil || strings.Contains(err.Error(), "not-a-key") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestImportFromFileOnFirstStartOnly(t *testing.T) {
	mr := miniredis.RunT(t)
	src := filepath.Join(t.TempDir(), "tailscaled.state")
	if err := os.WriteFile(src, []byte(`{"_machinekey":"`+base64.StdEncoding.EncodeToString([]byte("m"))+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := redisCfg(mr)
	cfg.ImportFrom = src
	st, err := Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := st.TSNet.ReadState("_machinekey")
	if err != nil || string(got) != "m" {
		t.Fatalf("import: %q %v", got, err)
	}
	st.Close()
	// Change the source: the second start must not touch what Redis already holds.
	if err := os.WriteFile(src, []byte(`{"_machinekey":"`+base64.StdEncoding.EncodeToString([]byte("other"))+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err = Open(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if got, _ := st.TSNet.ReadState("_machinekey"); string(got) != "m" {
		t.Fatalf("second start overwrote state: %q", got)
	}
}
