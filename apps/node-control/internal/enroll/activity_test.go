package enroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestContractEnrolledMarkerAndActivity(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		d, _ := e.create(t, "phone", time.Hour)
		if d.EnrolledAt != 0 || d.Enrolments != 0 || d.LastActive != 0 {
			t.Fatalf("a new device has activity: %+v", d)
		}
		tok, _ := e.b.NewLink(d.ID, time.Hour, 3)

		first, err := e.b.ConsumeToken(tok)
		mustDevice(t, first, err, "first spend")
		if first.EnrolledAt != e.clock.Now().Unix() || first.Enrolments != 1 {
			t.Fatalf("after the first spend: %+v", first)
		}
		e.clock.Advance(90 * time.Second)
		second, err := e.b.ConsumeToken(tok)
		mustDevice(t, second, err, "second spend")
		if second.EnrolledAt != e.clock.Now().Unix() || second.Enrolments != 2 {
			t.Fatalf("after the second spend: %+v", second)
		}
		stored, _ := e.b.GetDevice(d.ID)
		if stored.EnrolledAt != second.EnrolledAt || stored.Enrolments != 2 {
			t.Fatalf("stored = %+v", stored)
		}

		// A refused spend (link used up, unknown token, revoked device) changes nothing.
		other, _ := e.create(t, "other", time.Hour)
		otok, _ := e.b.NewLink(other.ID, time.Hour, 1)
		e.b.Revoke(other.ID)
		if g, err := e.b.ConsumeToken(otok); g != nil || err != nil {
			t.Fatalf("revoked spend: %v, %v", g, err)
		}
		if g, _ := e.b.GetDevice(other.ID); g.Enrolments != 0 || g.EnrolledAt != 0 {
			t.Fatalf("a refused spend marked the device: %+v", g)
		}
		e.b.ConsumeToken(tok) // third use
		if g, err := e.b.ConsumeToken(tok); g != nil || err != nil {
			t.Fatalf("fourth spend of a three-use link: %v, %v", g, err)
		}
		if g, _ := e.b.GetDevice(d.ID); g.Enrolments != 3 {
			t.Fatalf("enrolments = %d, want 3", g.Enrolments)
		}

		// Last active only moves forward and never creates a record.
		if err := e.b.TouchActive(d.ID, 5000); err != nil {
			t.Fatal(err)
		}
		if err := e.b.TouchActive(d.ID, 4000); err != nil {
			t.Fatal(err)
		}
		if g, _ := e.b.GetDevice(d.ID); g.LastActive != 5000 {
			t.Fatalf("LastActive = %d, want 5000 (never backwards)", g.LastActive)
		}
		if err := e.b.TouchActive(999, 1); err != nil {
			t.Fatal(err)
		}
		if devs, _ := e.b.ListDevices(); len(devs) != 2 {
			t.Fatalf("TouchActive on an unknown id created a device: %d devices", len(devs))
		}
	})
}

func TestRedisSpendScriptChecksTheDeviceItself(t *testing.T) {
	s, e := redisEnv(t)
	ctx := context.Background()
	d, _ := e.create(t, "phone", time.Hour)
	tok, _ := s.NewLink(d.ID, time.Hour, 5)
	tk := s.tokenKey(hashToken(tok))
	now := nowSec(s)
	args := func() []any { return []any{fnum(now), "1000000"} }

	// Revoke straight in Redis, bypassing the Go pre-check: the script must still refuse (this is the race the Go
	// check alone could not close).
	e.mr.HSet(s.deviceKey(d.ID), "revoked", "1")
	if n, err := spendScript.Eval(ctx, s.rdb, []string{tk, s.deviceKey(d.ID)}, args()...).Int(); err != nil || n != 0 {
		t.Fatalf("revoked device: %d, %v", n, err)
	}
	if got := e.mr.HGet(tk, "uses"); got != "0" {
		t.Fatalf("uses = %q after a refused spend", got)
	}
	e.mr.HSet(s.deviceKey(d.ID), "revoked", "0")
	e.mr.HSet(s.deviceKey(d.ID), "expires_at", "1")
	if n, _ := spendScript.Eval(ctx, s.rdb, []string{tk, s.deviceKey(d.ID)}, args()...).Int(); n != 0 {
		t.Fatalf("expired device: %d", n)
	}
	e.mr.HSet(s.deviceKey(d.ID), "expires_at", "4000000000")

	// A device that was deleted: no spend, and no stray device record.
	gone := s.deviceKey(4242)
	if n, _ := spendScript.Eval(ctx, s.rdb, []string{tk, gone}, args()...).Int(); n != 0 || e.mr.Exists(gone) {
		t.Fatalf("missing device: %d, exists=%v", n, e.mr.Exists(gone))
	}
	// And the happy path marks the device.
	if n, _ := spendScript.Eval(ctx, s.rdb, []string{tk, s.deviceKey(d.ID)}, args()...).Int(); n != 1 {
		t.Fatalf("valid spend: %d", n)
	}
	if e.mr.HGet(s.deviceKey(d.ID), "enrol_count") != "1" || e.mr.HGet(s.deviceKey(d.ID), "enrolled_at") != "1000000" {
		t.Fatal("the device was not marked by the script")
	}
}

func TestRedisReadsDeviceRecordsWrittenBeforeActivityExisted(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	for _, f := range []string{"enrolled_at", "enrol_count", "last_active"} {
		e.mr.HDel(s.deviceKey(d.ID), f)
	}
	got, err := s.GetDevice(d.ID)
	if err != nil || got.EnrolledAt != 0 || got.Enrolments != 0 || got.LastActive != 0 {
		t.Fatalf("old record = %+v, %v", got, err)
	}
	// The next spend still works on such a record.
	tok, _ := s.NewLink(d.ID, time.Hour, 1)
	if g, err := s.ConsumeToken(tok); g == nil || err != nil || g.Enrolments != 1 {
		t.Fatalf("spend on an old record: %+v, %v", g, err)
	}
}

func TestActivityFieldsSurviveEveryMove(t *testing.T) {
	// SQLite -> Redis (import) -> JSON snapshot -> SQLite file: the three values stay what they were.
	src, err := OpenStore(filepath.Join(t.TempDir(), "s.db"), testKey, func() time.Time { return time.Unix(1_000_000, 0) })
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	d, _, _ := src.CreateDevice("phone", "o", "c", 4_000_000_000)
	tok, _ := src.NewLink(d.ID, time.Hour, 2)
	src.ConsumeToken(tok)
	src.ConsumeToken(tok)
	src.TouchActive(d.ID, 1_000_500)

	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	dst, _ := newTestRedisStore(t, clock, testCipher(t))
	if _, _, _, err := Import(src, dst, false); err != nil {
		t.Fatal(err)
	}
	got, _ := dst.GetDevice(d.ID)
	if got.EnrolledAt != 1_000_000 || got.Enrolments != 2 || got.LastActive != 1_000_500 {
		t.Fatalf("after the import: %+v", got)
	}
	snap, err := dst.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "back.db")
	if err := WriteSQLite(snap, p, testKey); err != nil {
		t.Fatal(err)
	}
	back, err := OpenStore(p, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	g, _ := back.GetDevice(d.ID)
	if g.EnrolledAt != 1_000_000 || g.Enrolments != 2 || g.LastActive != 1_000_500 {
		t.Fatalf("after the export to SQLite: %+v", g)
	}
}

func TestOldSQLiteDatabaseGainsTheActivityColumns(t *testing.T) {
	x := loadExpected(t)
	path := copyFixture(t, "python_enroll.db") // no activity columns
	s, err := OpenStore(path, []byte(x.Key), func() time.Time { return time.Unix(x.Now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	checkSet(t, s, x.Modern) // everything else unchanged
	if err := s.TouchActive(1, x.Now+5); err != nil {
		t.Fatal(err)
	}
	if g, _ := s.GetDevice(1); g.LastActive != x.Now+5 {
		t.Fatalf("LastActive = %d", g.LastActive)
	}
	s.Close()
	// A read-only copy of the old file (no migration) reads the columns as 0 instead of failing.
	ro, err := OpenStoreReadOnly(copyFixture(t, "python_enroll.db"), []byte(x.Key), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	devs, err := ro.ListDevices()
	if err != nil || len(devs) != len(x.Modern.Devices) || devs[0].LastActive != 0 {
		t.Fatalf("read-only list of an old database: %+v, %v", devs, err)
	}
	if _, err := ro.Snapshot(); err != nil {
		t.Fatalf("snapshot of an old database: %v", err)
	}
}

// --- the tracker

type failingTouch struct{ Backend }

func (failingTouch) TouchActive(int64, int64) error { return errors.New("boom") }

func trackerFx(t *testing.T) (*ActivityTracker, Backend, *fakeClock, Device) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s, err := OpenStore(filepath.Join(t.TempDir(), "t.db"), testKey, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	d, _, _ := s.CreateDevice("phone", "o", "c", 4_000_000_000)
	return NewActivityTracker(s), s, clock, d
}

func tr(up, down float64) map[int64]Traffic { return map[int64]Traffic{1: {Up: up, Down: down}} }

func TestTrackerSeedsThenRecordsGrowthOnly(t *testing.T) {
	tk, s, clock, d := trackerFx(t)
	devs := []Device{d}

	if got := tk.Observe(devs, tr(10, 20)); len(got) != 0 { // the first look only seeds
		t.Fatalf("first look touched: %v", got)
	}
	clock.Advance(5 * time.Minute)
	if got := tk.Observe(devs, tr(10, 20)); len(got) != 0 { // no growth
		t.Fatalf("no growth touched: %v", got)
	}
	clock.Advance(5 * time.Minute)
	got := tk.Observe(devs, tr(10, 25))
	if got[d.ID] != clock.Now().Unix() {
		t.Fatalf("growth: %v, want %d", got, clock.Now().Unix())
	}
	if g, _ := s.GetDevice(d.ID); g.LastActive != clock.Now().Unix() {
		t.Fatalf("stored LastActive = %d", g.LastActive)
	}
}

func TestTrackerTreatsACounterResetWithTrafficAsActivity(t *testing.T) {
	tk, _, clock, d := trackerFx(t)
	devs := []Device{d}
	tk.Observe(devs, tr(500, 500))
	clock.Advance(5 * time.Minute)
	// xray restarted: the total fell, but it is above zero, so the device was active since the restart.
	if got := tk.Observe(devs, tr(3, 4)); len(got) != 1 {
		t.Fatalf("reset with traffic: %v", got)
	}
	clock.Advance(5 * time.Minute)
	// A reset to zero says nothing.
	if got := tk.Observe(devs, tr(0, 0)); len(got) != 0 {
		t.Fatalf("reset to zero: %v", got)
	}
}

func TestTrackerRateLimitsWritesAndKeepsBaselineWhenCountersVanish(t *testing.T) {
	tk, _, clock, d := trackerFx(t)
	devs := []Device{d}
	tk.Observe(devs, tr(1, 1))
	clock.Advance(10 * time.Second)
	if got := tk.Observe(devs, tr(2, 2)); len(got) != 1 {
		t.Fatalf("first growth: %v", got)
	}
	clock.Advance(10 * time.Second) // inside the minimum gap
	if got := tk.Observe(devs, tr(3, 3)); len(got) != 0 {
		t.Fatalf("a second write inside the gap: %v", got)
	}
	clock.Advance(2 * time.Minute)
	tk.Observe(devs, nil) // the agent knows nothing about the device now: the baseline stays
	if got := tk.Observe(devs, tr(4, 4)); len(got) != 1 {
		t.Fatalf("growth after the counters came back: %v", got)
	}
}

func TestTrackerForgetsDeletedDevicesAndSurvivesStoreErrors(t *testing.T) {
	tk, s, clock, d := trackerFx(t)
	tk.Observe([]Device{d}, tr(1, 1))
	tk.Observe(nil, nil) // the device is gone from the list
	if len(tk.prev) != 0 || len(tk.wrote) != 0 {
		t.Fatalf("state kept for a deleted device: %v %v", tk.prev, tk.wrote)
	}

	bad := NewActivityTracker(failingTouch{s})
	bad.Observe([]Device{d}, tr(1, 1))
	clock.Advance(5 * time.Minute)
	if got := bad.Observe([]Device{d}, tr(9, 9)); len(got) != 0 {
		t.Fatalf("a failed write was reported as recorded: %v", got)
	}
}

func TestListShowsEnrolledAndActivityWithoutLeaking(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	link := decode(t, f.post(idPath(id, "links"), map[string]any{"ttl": 3600, "max_uses": 1}))["url"].(string)
	token := link[len(link)-22:]
	if d, err := f.store.ConsumeToken(token); d == nil || err != nil {
		t.Fatalf("spend: %v, %v", d, err)
	}
	dev, _ := f.store.GetDevice(id)
	sum := sha256.Sum256([]byte(dev.UUIDs["auto"]))
	opaque := hex.EncodeToString(sum[:])[:8]

	read := func() map[string]any {
		return decode(t, f.do("GET", "/enroll/devices", "", ""))["devices"].([]any)[0].(map[string]any)
	}
	f.agent.stats = map[string]any{opaque: map[string]any{"up": 5, "down": 7}}
	row := read() // first look seeds
	if row["enrol_count"] != float64(1) || row["enrolled_at"] != float64(adminNow) || row["last_active"] != float64(0) {
		t.Fatalf("row = %v", row)
	}
	f.agent.stats = map[string]any{opaque: map[string]any{"up": 50, "down": 70}}
	row = read() // growth: shown at once, stored
	if row["last_active"] != float64(adminNow) {
		t.Fatalf("last_active = %v after growth", row["last_active"])
	}
	if g, _ := f.store.GetDevice(id); g.LastActive != adminNow {
		t.Fatalf("not stored: %d", g.LastActive)
	}
	// An ingress that does not answer must not read as a counter reset.
	f.agent.stats = nil
	if row = read(); row["last_active"] != float64(adminNow) {
		t.Fatalf("last_active changed on partial stats: %v", row["last_active"])
	}
}
