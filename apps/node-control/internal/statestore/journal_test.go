package statestore

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"tailscale.com/ipn"
)

// failNth fails the Nth WriteState call.
type failNth struct {
	ipn.StateStore
	n, calls int
}

func (f *failNth) WriteState(id ipn.StateKey, b []byte) error {
	f.calls++
	if f.calls == f.n {
		return errors.New("simulated write failure")
	}
	return f.StateStore.WriteState(id, b)
}

func TestImportRollsBackOnFailure(t *testing.T) {
	t.Run("write fails on the 3rd key", func(t *testing.T) {
		r, mr := newTestRedis(t)
		dst := &failNth{StateStore: r, n: 3}
		if _, err := Import(context.Background(), fixture, dst, false, r); err == nil {
			t.Fatal("failure not reported")
		}
		if mr.Exists(testKey) {
			f, _ := mr.HKeys(testKey)
			t.Fatalf("hash not empty after rollback: %v", f)
		}
	})
	t.Run("read-back mismatch", func(t *testing.T) {
		r, mr := newTestRedis(t)
		dst := &corruptingStore{StateStore: r}
		if _, err := Import(context.Background(), fixture, dst, false, r); err == nil {
			t.Fatal("corruption not detected")
		}
		if mr.Exists(testKey) {
			t.Fatal("hash not empty after rollback")
		}
	})
}

func TestImportMarkerLifecycle(t *testing.T) {
	r, mr := newTestRedis(t)
	enc, _ := NewEncrypted(r, newB64Key(t), testKey)
	var during string
	dst := &hookStore{StateStore: enc, onWrite: func(n int) {
		if n == 2 {
			during = mr.HGet(testKey, MarkerField)
		}
	}}
	if n, err := Import(context.Background(), fixture, dst, false, r); err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	for _, k := range []string{"_machinekey", "_current-profile", "profile-fake0001", "_profiles"} {
		if !strings.Contains(during, k) {
			t.Fatalf("marker during import lacks %q: %q", k, during)
		}
	}
	if strings.Contains(during, "ncenc1") || strings.Contains(during, "fake-") && strings.Contains(during, "bytes") {
		t.Fatalf("marker carries values: %q", during)
	}
	if mr.HGet(testKey, MarkerField) != "" {
		t.Fatal("marker left after success")
	}
	if n, _ := r.Len(); n != 4 {
		t.Fatalf("len=%d", n)
	}
}

type hookStore struct {
	ipn.StateStore
	onWrite func(n int)
	n       int
}

func (h *hookStore) WriteState(id ipn.StateKey, b []byte) error {
	h.n++
	h.onWrite(h.n)
	return h.StateStore.WriteState(id, b)
}

func TestMarkerNeverEncryptedOrVisible(t *testing.T) {
	r, mr := newTestRedis(t)
	enc, _ := NewEncrypted(r, newB64Key(t), testKey)
	if err := r.Begin([]string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if got := mr.HGet(testKey, MarkerField); got != `["a","b"]` {
		t.Fatalf("marker stored as %q", got)
	}
	if _, err := enc.ReadState(MarkerField); !errors.Is(err, ipn.ErrStateNotExist) {
		t.Fatalf("tsnet read sees the marker: %v", err)
	}
	if err := enc.WriteState(MarkerField, []byte("x")); err == nil {
		t.Fatal("tsnet can write the marker")
	}
}

func TestRecover(t *testing.T) {
	t.Run("interrupted import is undone", func(t *testing.T) {
		r, mr := newTestRedis(t)
		r.Begin([]string{"a", "b", "c"})
		r.WriteState("a", []byte("1"))
		r.WriteState("b", []byte("2"))
		cleaned, err := Recover(r)
		if err != nil || !cleaned {
			t.Fatalf("cleaned=%v err=%v", cleaned, err)
		}
		if mr.Exists(testKey) {
			t.Fatal("hash not empty")
		}
	})
	t.Run("state written by tsnet itself is complete", func(t *testing.T) {
		r, mr := newTestRedis(t)
		enc, _ := NewEncrypted(r, newB64Key(t), testKey)
		enc.WriteState("_machinekey", []byte("k"))
		enc.WriteState("_profiles", []byte("p"))
		cleaned, err := Recover(r)
		if err != nil || cleaned {
			t.Fatalf("cleaned=%v err=%v", cleaned, err)
		}
		if f, _ := mr.HKeys(testKey); len(f) != 2 {
			t.Fatalf("state touched: %v", f)
		}
	})
	t.Run("import refuses while a marker is pending", func(t *testing.T) {
		r, _ := newTestRedis(t)
		r.Begin([]string{"a"})
		if _, err := Import(context.Background(), fixture, r, false, r); !errors.Is(err, ErrInterrupted) {
			t.Fatalf("err=%v", err)
		}
	})
	t.Run("import refuses existing real keys without force", func(t *testing.T) {
		r, _ := newTestRedis(t)
		r.WriteState("unrelated", []byte("x"))
		if _, err := Import(context.Background(), fixture, r, false, r); err == nil {
			t.Fatal("imported into a non-empty hash")
		}
	})
}

var fileKeys = []string{"_machinekey", "_current-profile", "profile-fake0001", "_profiles"}

func sortedFields(t *testing.T, mr interface {
	HKeys(string) ([]string, error)
}) []string {
	t.Helper()
	f, _ := mr.HKeys(testKey)
	sort.Strings(f)
	return f
}

func TestForcedImportReplacesTheWholeIdentity(t *testing.T) {
	seed := func(r *Redis) {
		for _, k := range []string{"_machinekey", "old-b", "old-c"} {
			r.WriteState(ipn.StateKey(k), []byte("old"))
		}
	}
	want := append([]string(nil), fileKeys...)
	sort.Strings(want)

	t.Run("success leaves exactly the file keys", func(t *testing.T) {
		r, mr := newTestRedis(t)
		seed(r)
		if n, err := Import(context.Background(), fixture, r, true, r); err != nil || n != 4 {
			t.Fatalf("n=%d err=%v", n, err)
		}
		if got := sortedFields(t, mr); strings.Join(got, ",") != strings.Join(want, ",") {
			t.Fatalf("hash: %v", got)
		}
	})
	t.Run("failure on the 2nd write leaves no real keys and no marker", func(t *testing.T) {
		r, mr := newTestRedis(t)
		seed(r)
		_, err := Import(context.Background(), fixture, &failNth{StateStore: r, n: 2}, true, r)
		if !errors.Is(err, ErrIdentityRemoved) {
			t.Fatalf("err=%v", err)
		}
		if mr.Exists(testKey) {
			t.Fatalf("hash not empty: %v", sortedFields(t, mr))
		}
	})
	t.Run("marker lists the union and old keys are deleted before the first write", func(t *testing.T) {
		r, mr := newTestRedis(t)
		seed(r)
		var marker string
		var atFirstWrite []string
		dst := &hookStore{StateStore: r, onWrite: func(n int) {
			if n == 1 {
				marker = mr.HGet(testKey, MarkerField)
				atFirstWrite = sortedFields(t, mr)
			}
		}}
		if _, err := Import(context.Background(), fixture, dst, true, r); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(marker, "old-b") || !strings.Contains(marker, "old-c") {
			t.Fatalf("marker lacks the old keys: %q", marker)
		}
		for _, f := range atFirstWrite {
			if f == "old-b" || f == "old-c" {
				t.Fatalf("old key %q still there at the first write: %v", f, atFirstWrite)
			}
		}
	})
}

// noRollback simulates a kill -9: the failing run never gets to roll back.
type noRollback struct{ *Redis }

func (noRollback) Abort([]string) error { return nil }

func TestCrashedForcedImportRecoversToNoRealKeys(t *testing.T) {
	for _, at := range []int{1, 2, 4} {
		t.Run("crash on write "+string(rune('0'+at)), func(t *testing.T) {
			r, mr := newTestRedis(t)
			for _, k := range []string{"_machinekey", "old-b", "old-c"} {
				r.WriteState(ipn.StateKey(k), []byte("old"))
			}
			if _, err := Import(context.Background(), fixture, &failNth{StateStore: r, n: at}, true, noRollback{r}); err == nil {
				t.Fatal("expected the simulated crash")
			}
			if mr.HGet(testKey, MarkerField) == "" {
				t.Fatal("no marker left by the crash")
			}
			cleaned, err := Recover(r)
			if err != nil || !cleaned {
				t.Fatalf("cleaned=%v err=%v", cleaned, err)
			}
			if mr.Exists(testKey) {
				t.Fatalf("leftover keys: %v", sortedFields(t, mr))
			}
		})
	}
}

// crashBeforeDelete dies right after the marker is stored.
type crashBeforeDelete struct{ noRollback }

func (crashBeforeDelete) Delete([]string) error { return errors.New("simulated crash") }

func TestCrashedForcedImportAfterBeginRecoversToNoRealKeys(t *testing.T) {
	r, mr := newTestRedis(t)
	for _, k := range []string{"_machinekey", "old-b", "old-c"} {
		r.WriteState(ipn.StateKey(k), []byte("old"))
	}
	if _, err := Import(context.Background(), fixture, r, true, crashBeforeDelete{noRollback{r}}); err == nil {
		t.Fatal("expected the simulated crash")
	}
	if cleaned, err := Recover(r); err != nil || !cleaned {
		t.Fatalf("cleaned=%v err=%v", cleaned, err)
	}
	if mr.Exists(testKey) {
		t.Fatalf("leftover keys: %v", sortedFields(t, mr))
	}
}
