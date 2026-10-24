package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/core"
)

const (
	fakePassword = "fake-pw-4f9a7c"
	stateFixture = "../../internal/statestore/testdata/tailscaled.state.example"
)

func b64Key(t *testing.T) string {
	t.Helper()
	k := make([]byte, 32)
	rand.Read(k)
	return base64.StdEncoding.EncodeToString(k)
}

func redisEnv(mr *miniredis.Miniredis, extra map[string]string) core.Env {
	m := map[string]string{
		"REDIS_URL":    "redis://default:" + fakePassword + "@" + mr.Addr(),
		"FLY_APP_NAME": "node-control",
	}
	for k, v := range extra {
		m[k] = v
	}
	return core.MapEnv(m)
}

func fields(mr *miniredis.Miniredis) []string {
	k, _ := mr.HKeys("node-control:tsnet")
	return k
}

// capture routes slog output into a buffer for the test.
func capture(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func TestMainRedisWithoutStateKeyFails(t *testing.T) {
	mr := miniredis.RunT(t)
	_, _, err := buildStateStore(context.Background(), redisEnv(mr, nil))
	if err == nil {
		t.Fatal("started without an encryption key")
	}
	for _, want := range []string{"NODE_CONTROL_STATE_KEY", "openssl rand -base64 32", "STATE_ALLOW_PLAINTEXT"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("message lacks %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), fakePassword) || strings.Contains(err.Error(), mr.Addr()) {
		t.Fatalf("message leaks the URL: %v", err)
	}
	if len(mr.Keys()) != 0 {
		t.Fatal("Redis was written to")
	}
}

func TestMainPlaintextOptOut(t *testing.T) {
	mr := miniredis.RunT(t)
	store, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"STATE_ALLOW_PLAINTEXT": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	if err := store.WriteState("k", []byte("plain")); err != nil {
		t.Fatal(err)
	}
	if mr.HGet("node-control:tsnet", "k") != "plain" {
		t.Fatalf("not stored as plaintext under node-control:tsnet: %v", mr.Keys())
	}
}

func TestMainEncryptedByDefault(t *testing.T) {
	mr := miniredis.RunT(t)
	store, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": b64Key(t)}))
	if err != nil {
		t.Fatal(err)
	}
	defer closeFn()
	store.WriteState("k", []byte("secret"))
	if got := mr.HGet("node-control:tsnet", "k"); strings.Contains(got, "secret") || !strings.HasPrefix(got, "ncenc1:") {
		t.Fatalf("stored %q", got)
	}
}

func TestMainWithoutRedisUnchanged(t *testing.T) {
	buf := capture(t)
	store, closeFn, err := buildStateStore(context.Background(), core.MapEnv(map[string]string{}))
	if err != nil || store != nil {
		t.Fatalf("store=%v err=%v", store, err)
	}
	closeFn()
	if buf.Len() != 0 {
		t.Fatalf("unexpected logs: %s", buf)
	}
	// a key without Redis is ignored with a one-line warning
	key := b64Key(t)
	store, _, err = buildStateStore(context.Background(), core.MapEnv(map[string]string{"NODE_CONTROL_STATE_KEY": key}))
	if err != nil || store != nil {
		t.Fatalf("store=%v err=%v", store, err)
	}
	if !strings.Contains(buf.String(), "ignored") || strings.Contains(buf.String(), key) || strings.Count(buf.String(), "\n") != 1 {
		t.Fatalf("log: %q", buf)
	}
}

func TestMainRedisUnreachableFailsFast(t *testing.T) {
	mr := miniredis.RunT(t)
	env := redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": b64Key(t)})
	mr.Close()
	_, _, err := buildStateStore(context.Background(), env)
	if err == nil {
		t.Fatal("unreachable Redis accepted")
	}
	if strings.Contains(err.Error(), fakePassword) {
		t.Fatalf("leak: %v", err)
	}
}

func TestAutoImportOnlyWhenEmpty(t *testing.T) {
	key := b64Key(t)
	extra := map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": stateFixture}

	t.Run("empty hash imports once", func(t *testing.T) {
		mr := miniredis.RunT(t)
		buf := capture(t)
		_, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, extra))
		if err != nil {
			t.Fatal(err)
		}
		closeFn()
		if !strings.Contains(buf.String(), "imported 4 state keys") {
			t.Fatalf("log: %s", buf)
		}
		if strings.Contains(buf.String(), "fake-profile-id") {
			t.Fatal("log has a value")
		}
		if n := len(fields(mr)); n != 4 {
			t.Fatalf("hash has %d fields", n)
		}
	})

	t.Run("non-empty hash is left alone", func(t *testing.T) {
		mr := miniredis.RunT(t)
		mr.HSet("node-control:tsnet", "other", "x")
		buf := capture(t)
		_, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, extra))
		if err != nil {
			t.Fatal(err)
		}
		defer closeFn()
		if n := len(fields(mr)); n != 1 {
			t.Fatalf("hash changed: %d fields", n)
		}
		if !strings.Contains(buf.String(), "STATE_IMPORT_FROM ignored") || strings.Count(buf.String(), "\n") != 1 {
			t.Fatalf("notice: %q", buf)
		}
	})

	t.Run("second start does not import again", func(t *testing.T) {
		mr := miniredis.RunT(t)
		env := redisEnv(mr, extra)
		_, c1, err := buildStateStore(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		c1()
		mr.HDel("node-control:tsnet", "_profiles") // would be restored by a second import
		_, c2, err := buildStateStore(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		defer c2()
		if mr.HGet("node-control:tsnet", "_profiles") != "" {
			t.Fatal("imported twice")
		}
	})

	t.Run("missing file with empty hash is fine", func(t *testing.T) {
		mr := miniredis.RunT(t)
		env := redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": filepath.Join(t.TempDir(), "nope")})
		_, closeFn, err := buildStateStore(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		defer closeFn()
		if len(mr.Keys()) != 0 {
			t.Fatal("wrote something")
		}
	})

	t.Run("bad file fails startup", func(t *testing.T) {
		mr := miniredis.RunT(t)
		p := filepath.Join(t.TempDir(), "bad")
		os.WriteFile(p, []byte("not json"), 0o600)
		_, _, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": p}))
		if err == nil {
			t.Fatal("bad file accepted")
		}
	})
}

func TestStateImportSubcommand(t *testing.T) {
	mr := miniredis.RunT(t)
	env := redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": b64Key(t)})
	var out bytes.Buffer
	if err := runStateImport(context.Background(), []string{"-from", stateFixture}, env, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "imported 4 state keys") {
		t.Fatalf("out: %q", out.String())
	}
	if err := runStateImport(context.Background(), []string{"-from", stateFixture}, env, &out); err == nil {
		t.Fatal("second import without -force accepted")
	}
	if err := runStateImport(context.Background(), []string{"-from", stateFixture, "-force"}, env, &out); err != nil {
		t.Fatal(err)
	}
	if err := runStateImport(context.Background(), []string{}, env, &out); err == nil {
		t.Fatal("missing -from accepted")
	}
	if err := runStateImport(context.Background(), []string{"-from", stateFixture}, core.MapEnv(nil), &out); err == nil {
		t.Fatal("import without REDIS_URL accepted")
	}
}

func TestInterruptedImport(t *testing.T) {
	key := b64Key(t)
	const hash = "node-control:tsnet"
	crash := func(mr *miniredis.Miniredis) {
		// what a kill -9 mid-import leaves: the marker and a subset of the keys
		mr.HSet(hash, "_node-control-import", `["_machinekey","_current-profile","profile-fake0001","_profiles"]`)
		mr.HSet(hash, "_machinekey", "ncenc1:partial")
	}

	t.Run("redone when STATE_IMPORT_FROM is set", func(t *testing.T) {
		mr := miniredis.RunT(t)
		crash(mr)
		_, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": stateFixture}))
		if err != nil {
			t.Fatal(err)
		}
		defer closeFn()
		if n := len(fields(mr)); n != 4 {
			t.Fatalf("hash has %d fields: %v", n, fields(mr))
		}
		if mr.HGet(hash, "_node-control-import") != "" {
			t.Fatal("marker left")
		}
		if !strings.HasPrefix(mr.HGet(hash, "_machinekey"), "ncenc1:") || mr.HGet(hash, "_machinekey") == "ncenc1:partial" {
			t.Fatal("partial value not replaced")
		}
	})

	t.Run("startup fails without STATE_IMPORT_FROM", func(t *testing.T) {
		mr := miniredis.RunT(t)
		crash(mr)
		store, _, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key}))
		if err == nil || store != nil {
			t.Fatalf("started on partial state: store=%v err=%v", store, err)
		}
		for _, want := range []string{"interrupted", "cleaned up", "state-import"} {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("message lacks %q: %v", want, err)
			}
		}
		if strings.Contains(err.Error(), fakePassword) {
			t.Fatal("leak")
		}
		if len(fields(mr)) != 0 {
			t.Fatalf("partial keys left: %v", fields(mr))
		}
	})

	t.Run("startup fails when the file is gone", func(t *testing.T) {
		mr := miniredis.RunT(t)
		crash(mr)
		_, _, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": filepath.Join(t.TempDir(), "gone")}))
		if err == nil {
			t.Fatal("started on partial state")
		}
	})

	t.Run("state-import cleans up and redoes", func(t *testing.T) {
		mr := miniredis.RunT(t)
		crash(mr)
		var out bytes.Buffer
		if err := runStateImport(context.Background(), []string{"-from", stateFixture}, redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key}), &out); err != nil {
			t.Fatal(err)
		}
		if n := len(fields(mr)); n != 4 || !strings.Contains(out.String(), "interrupted") {
			t.Fatalf("fields=%d out=%q", n, out.String())
		}
	})

	t.Run("state written by tsnet itself is not an interrupted import", func(t *testing.T) {
		mr := miniredis.RunT(t)
		store, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key}))
		if err != nil {
			t.Fatal(err)
		}
		store.WriteState("_machinekey", []byte("k"))
		closeFn()
		_, c2, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": stateFixture}))
		if err != nil {
			t.Fatal(err)
		}
		defer c2()
		if n := len(fields(mr)); n != 1 {
			t.Fatalf("state changed: %v", fields(mr))
		}
	})
}

func TestBadStateKeyFailsBeforeConnecting(t *testing.T) {
	mr := miniredis.RunT(t)
	env := redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": "short"})
	before := mr.TotalConnectionCount()
	if _, _, err := buildStateStore(context.Background(), env); err == nil {
		t.Fatal("bad key accepted")
	}
	if mr.TotalConnectionCount() != before {
		t.Fatal("connected to Redis before validating the key")
	}
}

func TestCloseStoreIsIdempotent(t *testing.T) {
	mr := miniredis.RunT(t)
	_, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"STATE_ALLOW_PLAINTEXT": "1"}))
	if err != nil {
		t.Fatal(err)
	}
	closeFn()
	closeFn()
}

func TestInterruptedForcedImport(t *testing.T) {
	key := b64Key(t)
	const hash = "node-control:tsnet"
	union := `["_current-profile","_machinekey","_profiles","old-b","old-c","profile-fake0001"]`
	stages := map[string]func(mr *miniredis.Miniredis){
		"after begin": func(mr *miniredis.Miniredis) {
			mr.HSet(hash, "_node-control-import", union)
			mr.HSet(hash, "_machinekey", "ncenc1:old")
			mr.HSet(hash, "old-b", "ncenc1:old")
			mr.HSet(hash, "old-c", "ncenc1:old")
		},
		"after the delete": func(mr *miniredis.Miniredis) {
			mr.HSet(hash, "_node-control-import", union)
			mr.HSet(hash, "_machinekey", "ncenc1:old")
		},
		"mid writes": func(mr *miniredis.Miniredis) {
			mr.HSet(hash, "_node-control-import", union)
			mr.HSet(hash, "_machinekey", "ncenc1:new")
			mr.HSet(hash, "_profiles", "ncenc1:new")
		},
	}
	for name, crash := range stages {
		t.Run(name+": redone leaves exactly the file keys", func(t *testing.T) {
			mr := miniredis.RunT(t)
			crash(mr)
			_, closeFn, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key, "STATE_IMPORT_FROM": stateFixture}))
			if err != nil {
				t.Fatal(err)
			}
			defer closeFn()
			got := fields(mr)
			if len(got) != 4 {
				t.Fatalf("hash: %v", got)
			}
			for _, f := range got {
				if f == "old-b" || f == "old-c" || f == "_node-control-import" {
					t.Fatalf("leftover %q", f)
				}
			}
		})
		t.Run(name+": without STATE_IMPORT_FROM fails and leaves no real keys", func(t *testing.T) {
			mr := miniredis.RunT(t)
			crash(mr)
			store, _, err := buildStateStore(context.Background(), redisEnv(mr, map[string]string{"NODE_CONTROL_STATE_KEY": key}))
			if err == nil || store != nil {
				t.Fatalf("started: store=%v err=%v", store, err)
			}
			if len(fields(mr)) != 0 {
				t.Fatalf("real keys left: %v", fields(mr))
			}
		})
	}
}
