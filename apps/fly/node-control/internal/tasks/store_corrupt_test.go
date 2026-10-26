package tasks

import (
	"bytes"
	"encoding/base64"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"tailscale.com/ipn"

	"example.com/node-control/internal/statestore"
)

// A stored value the store cannot use (bad JSON, a config a stricter parser
// refuses, a value sealed with another key) must not lock the module out: the
// field is treated as absent, so a push can repair it.

type corruptEnv struct {
	mr    *miniredis.Miniredis
	enc   ipn.StateStore // the store's own view (right key)
	other ipn.StateStore // same hash, another key
	logs  *bytes.Buffer
	log   *slog.Logger
}

func newCorruptEnv(t *testing.T) *corruptEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	mk := func(key string) ipn.StateStore {
		rs, err := statestore.NewRedis("redis://"+mr.Addr(), storeScope)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { rs.Close() })
		enc, err := statestore.NewEncrypted(rs, key, storeScope)
		if err != nil {
			t.Fatal(err)
		}
		return enc
	}
	otherKey := base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff"))
	logs := &bytes.Buffer{}
	return &corruptEnv{mr: mr, enc: mk(storeKey), other: mk(otherKey), logs: logs,
		log: slog.New(slog.NewTextHandler(logs, nil))}
}

// seed stores a working config, script, secret, state and history.
func (c *corruptEnv) seed(t *testing.T) {
	t.Helper()
	s := openStore(t, c.enc)
	if err := s.PutSecret("token", "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConfig(testConfig(t), map[string]string{"ping.sh": "echo hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveState("ping", TaskState{Streak: 3}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendRun(Run{ID: "r1", Task: "ping", State: RunSucceeded}); err != nil {
		t.Fatal(err)
	}
}

func (c *corruptEnv) write(t *testing.T, kv ipn.StateStore, field, val string) {
	t.Helper()
	if err := kv.WriteState(ipn.StateKey(field), []byte(val)); err != nil {
		t.Fatal(err)
	}
}

// open must succeed, warn naming the field only, and accept a later push.
func (c *corruptEnv) openAndRepair(t *testing.T, field, secretVal string) *Store {
	t.Helper()
	s, err := OpenStore(c.enc, c.log)
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	out := c.logs.String()
	if !strings.Contains(out, "field="+field) {
		t.Fatalf("no warning naming %s:\n%s", field, out)
	}
	for _, bad := range []string{"s3cret-value", secretVal, "bad-json-marker"} {
		if bad != "" && strings.Contains(out, bad) {
			t.Fatalf("log carries a value %q:\n%s", bad, out)
		}
	}
	if err := s.PutSecret("token", "s3cret-value"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConfig(testConfig(t), map[string]string{"ping.sh": "echo hi"}); err != nil {
		t.Fatalf("PutConfig after a bad field: %v", err)
	}
	return s
}

func TestStoreOpensWithCorruptConfig(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	c.write(t, c.enc, "config", `{"tasks": bad-json-marker`)
	s, err := OpenStore(c.enc, c.log)
	if err != nil {
		t.Fatal(err)
	}
	if s.Config() != nil {
		t.Fatal("a bad config must read as no config")
	}
	if v, err := s.Secrets([]string{"token"}); err != nil || v["token"] != "s3cret-value" {
		t.Fatalf("secrets that decode are kept: %v %v", v, err)
	}
	c.logs.Reset()
	c.write(t, c.enc, "config", `{"tasks":{"ping":{"script":"ping.sh","target":{"node":"n1"},"schedule":{"every":"1s"}}}}`) // refused by ParseConfig
	s = c.openAndRepair(t, "config", "")
	if s.Config() == nil {
		t.Fatal("config not repaired")
	}
	if r := openStore(t, c.enc); r.Config() == nil {
		t.Fatal("repaired config not stored")
	}
}

func TestStoreOpensWithCorruptState(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	c.write(t, c.enc, "state:ping", "bad-json-marker")
	s := c.openAndRepair(t, "state:ping", "")
	if s.State("ping") != (TaskState{}) {
		t.Fatalf("state = %+v, want absent", s.State("ping"))
	}
	if len(s.Runs("ping")) != 1 {
		t.Fatal("other fields of the task must still load")
	}
}

func TestStoreOpensWithCorruptRuns(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	c.write(t, c.enc, "runs:ping", "[bad-json-marker")
	s := c.openAndRepair(t, "runs:ping", "")
	if len(s.Runs("ping")) != 0 {
		t.Fatal("runs must read as absent")
	}
	if s.State("ping").Streak != 3 {
		t.Fatal("state of the task must still load")
	}
	if err := s.AppendRun(Run{ID: "r2", Task: "ping", State: RunSucceeded}); err != nil {
		t.Fatal(err)
	}
}

func TestStoreOpensWithUndecryptableSecret(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	s0 := openStore(t, c.enc)
	if err := s0.PutSecret("other", "kept-value"); err != nil {
		t.Fatal(err)
	}
	c.write(t, c.other, "secret:token", "sealed-with-another-key") // a rotated key
	s := c.openAndRepair(t, "secret:token", "sealed-with-another-key")
	if v, err := s.Secrets([]string{"other"}); err != nil || v["other"] != "kept-value" {
		t.Fatalf("decodable secret lost: %v %v", v, err)
	}
	// openAndRepair pushed token again; a fresh open lists it.
	if v, err := openStore(t, c.enc).Secrets([]string{"token"}); err != nil || v["token"] != "s3cret-value" {
		t.Fatalf("re-pushed secret: %v %v", v, err)
	}
}

func TestStoreUndecryptableSecretIsNotListed(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	c.write(t, c.other, "secret:token", "sealed-with-another-key")
	s, err := OpenStore(c.enc, c.log)
	if err != nil {
		t.Fatal(err)
	}
	if names := s.SecretNames(); len(names) != 0 {
		t.Fatalf("names = %v, want the unreadable secret unlisted", names)
	}
}

func TestStoreOpenStillFailsOnTransportError(t *testing.T) {
	c := newCorruptEnv(t)
	c.seed(t)
	c.mr.Close()
	if _, err := OpenStore(c.enc, c.log); err == nil {
		t.Fatal("OpenStore must fail while Redis is unreachable")
	}
}

func TestModuleOpensOverCorruptConfigAndPushRepairs(t *testing.T) {
	h := newModHarnessEnv(t, nil, func(env map[string]string) {
		kv, closeKV, err := openRedisKV(env["REDIS_URL"], modStateKey)
		if err != nil {
			t.Fatal(err)
		}
		defer closeKV()
		if err := kv.WriteState(fieldConfig, []byte("{bad-json-marker")); err != nil {
			t.Fatal(err)
		}
	})
	store := h.waitOpen()
	if store.Config() != nil {
		t.Fatal("bad stored config must read as none")
	}
	h.pushFull()
	if store.Config() == nil {
		t.Fatal("push did not repair the config")
	}
}
