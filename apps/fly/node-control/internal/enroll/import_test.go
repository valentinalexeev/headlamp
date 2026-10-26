package enroll

import (
	"bytes"
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func importFixture(t *testing.T, name string, x expectedFile) (*Store, *RedisStore, contractEnv) {
	t.Helper()
	src := openFixture(t, name, x)
	clock := &fakeClock{now: time.Unix(x.Now, 0)}
	dst, mr := newTestRedisStore(t, clock, testCipher(t))
	dst.key = []byte(x.Key) // subscription secrets derive from the same key on both stores
	return src, dst, contractEnv{b: dst, clock: clock, mr: mr}
}

func TestImportPythonFixtureKeepsEverythingWorking(t *testing.T) {
	x := loadExpected(t)
	for _, tc := range []struct {
		file string
		want expectedSet
	}{{"python_enroll.db", x.Modern}, {"python_enroll_legacy.db", x.Legacy}} {
		t.Run(tc.file, func(t *testing.T) {
			src, dst, _ := importFixture(t, tc.file, x)
			devices, _, _, err := Import(src, dst, false)
			if err != nil {
				t.Fatal(err)
			}
			if devices != len(tc.want.Devices) {
				t.Fatalf("imported %d devices, want %d", devices, len(tc.want.Devices))
			}
			// Same ids, UUIDs, rotated secrets, active credentials as the SQLite file served.
			checkSet(t, dst, tc.want)
		})
	}
}

func TestImportCarriesTheLinkAndItsUses(t *testing.T) {
	x := loadExpected(t)
	src, dst, e := importFixture(t, "python_enroll.db", x)
	// Spend one use on the source first: the copy must remember it.
	if d, err := src.ConsumeToken(x.Token); err != nil || d == nil {
		t.Fatalf("source consume: %v", err)
	}
	if _, links, _, err := Import(src, dst, false); err != nil || links < 1 {
		t.Fatalf("Import: links=%d err=%v", links, err)
	}
	for i := 1; i < x.MaxUses; i++ {
		d, err := e.b.ConsumeToken(x.Token)
		mustDevice(t, d, err, "remaining use")
	}
	d, err := e.b.ConsumeToken(x.Token)
	mustNil(t, d, err, "link after its uses are spent")
}

func TestImportKeepsTheIdCounterAndNeverReusesIds(t *testing.T) {
	x := loadExpected(t)
	src, dst, e := importFixture(t, "python_enroll.db", x)
	// Make the source counter run ahead of max(id): create then drop the newest row, as AUTOINCREMENT keeps counting.
	d, _, err := src.CreateDevice("gone", "o", "c", x.Now+100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.Exec("DELETE FROM device_uuids WHERE device_id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := src.db.Exec("DELETE FROM devices WHERE id=?", d.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	n, _, err := e.b.CreateDevice("new", "o", "c", x.Now+100)
	if err != nil {
		t.Fatal(err)
	}
	if n.ID <= d.ID {
		t.Fatalf("new device got id %d, which the source already issued (%d)", n.ID, d.ID)
	}
}

func TestImportRefusesAPopulatedTargetAndIsRepeatableWithForce(t *testing.T) {
	x := loadExpected(t)
	src, dst, _ := importFixture(t, "python_enroll.db", x)
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(src, dst, false); !errors.Is(err, ErrPopulated) {
		t.Fatalf("second import err = %v, want ErrPopulated", err)
	}
	if _, _, _, err := Import(src, dst, true); err != nil {
		t.Fatalf("forced re-import: %v", err)
	}
	checkSet(t, dst, x.Modern)
}

func TestInterruptedRestoreLeavesTheTargetEmptyAndIsRerunnable(t *testing.T) {
	x := loadExpected(t)
	src, dst, e := importFixture(t, "python_enroll.db", x)
	snap, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a crash after the device records but before the counter: write them by hand.
	for _, d := range snap.Devices {
		if err := dst.putDevice(nil2ctx(), Device{ID: d.ID, Name: d.Name, Owner: d.Owner, Client: d.Client, ExpiresAt: d.ExpiresAt, Revoked: d.Revoked, UUIDs: d.UUIDs}, d.Epoch); err != nil {
			t.Fatal(err)
		}
	}
	if e.mr.HGet(dst.metaKey(), "seq") != "" {
		t.Fatal("test setup: counter should be unset")
	}
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatalf("re-run after an interrupted import: %v", err)
	}
	checkSet(t, dst, x.Modern)
}

func TestImportDoesNotModifyTheSource(t *testing.T) {
	x := loadExpected(t)
	src, dst, _ := importFixture(t, "python_enroll.db", x)
	before, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	after, err := src.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatal("the source changed during the import")
	}
}

func TestExportRoundTripThroughRedisSnapshot(t *testing.T) {
	x := loadExpected(t)
	src, dst, _ := importFixture(t, "python_enroll.db", x)
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	snap, err := dst.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := src.Snapshot()
	if len(snap.Devices) != len(want.Devices) || snap.Seq < want.Seq {
		t.Fatalf("snapshot devices=%d seq=%d, want %d / >=%d", len(snap.Devices), snap.Seq, len(want.Devices), want.Seq)
	}
	// And a snapshot restores into a fresh Redis store unchanged.
	clock := &fakeClock{now: time.Unix(x.Now, 0)}
	fresh, _ := newTestRedisStore(t, clock, testCipher(t))
	fresh.key = []byte(x.Key)
	if _, _, _, err := fresh.Restore(snap, false); err != nil {
		t.Fatal(err)
	}
	checkSet(t, fresh, x.Modern)
}

func nil2ctx() context.Context { return context.Background() }

func TestReadOnlyOpenNeverWritesTheSource(t *testing.T) {
	x := loadExpected(t)
	for _, name := range []string{"python_enroll.db", "python_enroll_legacy.db"} {
		t.Run(name, func(t *testing.T) {
			path := copyFixture(t, name)
			before, _ := os.ReadFile(path)
			src, err := OpenStoreReadOnly(path, []byte(x.Key), func() time.Time { return time.Unix(x.Now, 0) })
			if err != nil {
				t.Fatal(err)
			}
			dst, _ := newTestRedisStore(t, &fakeClock{now: time.Unix(x.Now, 0)}, testCipher(t))
			dst.key = []byte(x.Key)
			if _, _, _, err := Import(src, dst, false); err != nil {
				t.Fatal(err)
			}
			src.Close()
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("the source file changed")
			}
			if _, err := os.Stat(path + "-wal"); err == nil {
				t.Fatal("a -wal file appeared next to the source")
			}
		})
	}
}

// A copy that fails verification must not stay behind looking populated: the next start would skip ENROLL_IMPORT_FROM and
// serve (and push to the agents) data that never verified.
func TestFailedVerificationResetsTheCounterSoTheNextStartRetries(t *testing.T) {
	s, e := redisEnv(t)
	// Two entries with the same id: the restore writes both to one key, so the read-back cannot equal the source.
	bad := Snapshot{Seq: 1, Devices: []SnapDevice{
		{ID: 1, Name: "a", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u1"}},
		{ID: 1, Name: "b", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u2"}},
	}}
	if _, _, _, err := ImportSnapshot(bad, s, false); err == nil || !strings.Contains(err.Error(), "verifying the import") {
		t.Fatalf("a mismatching copy was accepted: %v", err)
	}
	if has, err := s.HasData(); err != nil || has {
		t.Fatalf("after a failed verification the store reads as populated (%v, %v): the next start would skip the import", has, err)
	}
	if e.mr.HGet(s.metaKey(), "seq") != "" {
		t.Fatal("the counter is still there")
	}
	// The next attempt, with good data, works.
	good := Snapshot{Seq: 1, Devices: []SnapDevice{{ID: 1, Name: "a", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u1"}}}}
	if _, _, _, err := ImportSnapshot(good, s, false); err != nil {
		t.Fatalf("retry after a failed verification: %v", err)
	}
	if has, _ := s.HasData(); !has {
		t.Fatal("the retry did not populate the store")
	}
}

func TestFailedVerificationWithForceKeepsAnExistingStorePopulated(t *testing.T) {
	s, _ := redisEnv(t)
	good := Snapshot{Seq: 1, Devices: []SnapDevice{{ID: 1, Name: "a", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u1"}}}}
	if _, _, _, err := ImportSnapshot(good, s, false); err != nil {
		t.Fatal(err)
	}
	bad := Snapshot{Seq: 2, Devices: []SnapDevice{
		{ID: 2, Name: "x", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u3"}},
		{ID: 2, Name: "y", Owner: "o", Client: "c", ExpiresAt: 4_000_000_000, UUIDs: map[string]string{"auto": "u4"}},
	}}
	if _, _, _, err := ImportSnapshot(bad, s, true); err == nil {
		t.Fatal("a mismatching forced copy was accepted")
	}
	if has, _ := s.HasData(); !has {
		t.Fatal("a forced import that failed must not make a populated store look empty")
	}
}
