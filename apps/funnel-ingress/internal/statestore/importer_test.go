package statestore

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
)

const fixture = "testdata/tailscaled.state.example"

func TestImportCopiesAllKeysAndVerifies(t *testing.T) {
	t.Run("copies every key", func(t *testing.T) {
		dst := new(mem.Store)
		n, err := Import(context.Background(), fixture, dst, false, nil)
		if err != nil || n != 4 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		got, err := dst.ReadState("_current-profile")
		if err != nil || string(got) != "fake-profile-id" {
			t.Fatalf("got %q, %v", got, err)
		}
	})
	t.Run("detects a destination that stores something else", func(t *testing.T) {
		dst := &corruptingStore{StateStore: new(mem.Store)}
		if _, err := Import(context.Background(), fixture, dst, false, nil); err == nil {
			t.Fatal("corrupted write not detected")
		}
	})
	t.Run("through encryption", func(t *testing.T) {
		raw := new(mem.Store)
		enc, _ := NewEncrypted(raw, newB64Key(t), testKey)
		if n, err := Import(context.Background(), fixture, enc, false, nil); err != nil || n != 4 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		stored, _ := raw.ReadState("_current-profile")
		if !bytes.HasPrefix(stored, []byte("ncenc1:")) {
			t.Fatalf("not encrypted at rest: %q", stored)
		}
	})
}

// corruptingStore drops the last byte of every value it stores.
type corruptingStore struct{ ipn.StateStore }

func (c *corruptingStore) WriteState(id ipn.StateKey, b []byte) error {
	if len(b) > 0 {
		b = b[:len(b)-1]
	}
	return c.StateStore.WriteState(id, b)
}

func TestImportRefusesExistingWithoutForce(t *testing.T) {
	dst := new(mem.Store)
	dst.WriteState("_machinekey", []byte("keep-me"))
	n, err := Import(context.Background(), fixture, dst, false, nil)
	if err == nil || n != 0 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !strings.Contains(err.Error(), "-force") {
		t.Fatalf("error does not mention the override: %v", err)
	}
	if got, _ := dst.ReadState("_machinekey"); string(got) != "keep-me" {
		t.Fatal("existing value touched")
	}
	if _, err := dst.ReadState("_profiles"); err == nil {
		t.Fatal("partial import happened")
	}
}

func TestImportForceOverwrites(t *testing.T) {
	dst := new(mem.Store)
	dst.WriteState("_machinekey", []byte("old"))
	n, err := Import(context.Background(), fixture, dst, true, nil)
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if got, _ := dst.ReadState("_machinekey"); string(got) != "fake-machine-key-bytes-00000000" {
		t.Fatalf("not overwritten: %q", got)
	}
}

func TestImportDoesNotTouchSource(t *testing.T) {
	src := filepath.Join(t.TempDir(), "tailscaled.state")
	orig, _ := os.ReadFile(fixture)
	if err := os.WriteFile(src, orig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Import(context.Background(), src, new(mem.Store), false, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(src)
	if err != nil || !bytes.Equal(after, orig) {
		t.Fatalf("source changed: %v", err)
	}
	fi, _ := os.Stat(src)
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode changed: %v", fi.Mode())
	}
}

func TestImportBadJSON(t *testing.T) {
	for name, content := range map[string]string{
		"not json":       "not json",
		"array":          `["a"]`,
		"not base64":     `{"k": "***not-base64***"}`,
		"non string val": `{"k": 5}`,
	} {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "s")
			os.WriteFile(p, []byte(content), 0o600)
			dst := new(mem.Store)
			if _, err := Import(context.Background(), p, dst, false, nil); err == nil {
				t.Fatal("accepted")
			}
			if _, err := dst.ReadState("k"); err == nil {
				t.Fatal("wrote something")
			}
		})
	}
	t.Run("missing file", func(t *testing.T) {
		_, err := Import(context.Background(), filepath.Join(t.TempDir(), "nope"), new(mem.Store), false, nil)
		if !os.IsNotExist(unwrapAll(err)) {
			t.Fatalf("want a not-exist error, got %v", err)
		}
	})
}

func unwrapAll(err error) error {
	for {
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return err
		}
		err = u.Unwrap()
	}
}

func TestImportLogsNoValues(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)

	if _, err := Import(context.Background(), fixture, new(mem.Store), false, nil); err != nil {
		t.Fatal(err)
	}
	// the fake values, plain and as they appear in the file
	for _, v := range []string{"fake-machine-key-bytes", "fake-profile-id", "ZmFrZS1tYWNoaW5l", "ZmFrZS1wcm9maWxl"} {
		if strings.Contains(buf.String(), v) {
			t.Fatalf("log contains a value: %q", buf.String())
		}
	}
	if !strings.Contains(buf.String(), "_machinekey") {
		t.Fatalf("key names should be logged: %q", buf.String())
	}
}

func TestKeyForDefaultsAndValidation(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
		bad  bool
	}{
		{"fly app", map[string]string{"FLY_APP_NAME": "funnel-ingress", "FUNNEL_HOSTNAME": "x"}, "funnel-ingress:tsnet", false},
		{"hostname fallback", map[string]string{"FUNNEL_HOSTNAME": "my-host"}, "my-host:tsnet", false},
		{"default hostname", map[string]string{}, "funnel-ingress:tsnet", false},
		{"explicit", map[string]string{"REDIS_KEY": "other:state"}, "other:state", false},
		{"explicit wins", map[string]string{"REDIS_KEY": "other:state", "FLY_APP_NAME": "a"}, "other:state", false},
		{"no separator", map[string]string{"REDIS_KEY": "tsnet"}, "", true},
		{"empty prefix", map[string]string{"REDIS_KEY": ":tsnet"}, "", true},
		{"empty suffix", map[string]string{"REDIS_KEY": "app:"}, "", true},
		{"star", map[string]string{"REDIS_KEY": "app:*"}, "", true},
		{"question", map[string]string{"REDIS_KEY": "app:t?net"}, "", true},
		{"bracket", map[string]string{"REDIS_KEY": "app:[a]"}, "", true},
		{"glob in app name", map[string]string{"FLY_APP_NAME": "a*b"}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := KeyFor(coreEnv(c.env))
			if c.bad {
				if err == nil {
					t.Fatalf("accepted %q", got)
				}
				return
			}
			if err != nil || got != c.want {
				t.Fatalf("got %q, %v; want %q", got, err, c.want)
			}
		})
	}
}

func TestNoForbiddenRedisCommands(t *testing.T) {
	// Shared Redis rules: DB 0 only, no SELECT, FLUSHALL/FLUSHDB or KEYS.
	files, _ := filepath.Glob("../../cmd/funnel-ingress/*.go")
	more, _ := filepath.Glob("*.go")
	files = append(files, more...)
	forbidden := []string{".Select(", ".FlushAll(", ".FlushDB(", ".FlushAllAsync(", ".FlushDBAsync(", ".Keys(", ".Scan(", `"SELECT"`, `"FLUSHALL"`, `"FLUSHDB"`, `"KEYS"`}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range forbidden {
			if bytes.Contains(b, []byte(w)) {
				t.Errorf("%s uses %s", f, w)
			}
		}
	}
	if len(files) < 2 {
		t.Fatal("no source files scanned")
	}
}
