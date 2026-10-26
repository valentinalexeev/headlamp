package enroll

// Opt-in tests against a REAL Redis (the shared instance the app will use), not the in-memory fake. They are skipped
// unless ENROLL_REDIS_LIVE_URL is set, so `go test ./...` never touches a network.
//
//	ENROLL_REDIS_LIVE_URL=redis://localhost:6379 go test ./internal/enroll -run 'Live|Contract' -count=1 -v
//
// (for a Redis only reachable from a private network, run `fly redis proxy <name>` and use localhost.)
//
// Every run uses its own unique key prefix "<app>:enroll-livetest-<random>" (the app part is ENROLL_REDIS_LIVE_APP,
// default node-control, so the shared-Redis "<app>:" prefix rule holds), seals values with a random key, and removes
// every key under that prefix afterwards with a scoped SCAN and DEL. Nothing outside the prefix is read or written.
// The tests exist to show that the real server accepts the commands the store sends, notably EVAL (never EVALSHA),
// HINCRBY, HGETALL and PEXPIRE inside the scripts.

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"example.com/node-control/internal/statestore"
)

const liveEnv = "ENROLL_REDIS_LIVE_URL"

// liveFactory returns the contract factory for the real Redis, or false when the URL is not set.
func liveFactory() (backendFactory, bool) {
	if os.Getenv(liveEnv) == "" {
		return backendFactory{}, false
	}
	return backendFactory{"redis-live", func(t *testing.T) contractEnv {
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		s := newLiveStore(t, clock)
		return contractEnv{b: s, clock: clock}
	}}, true
}

func newLiveStore(t *testing.T, clock *fakeClock) *RedisStore {
	t.Helper()
	url := os.Getenv(liveEnv)
	if url == "" {
		t.Skip(liveEnv + " is not set")
	}
	rdb, secrets, err := statestore.OpenClient(url)
	if err != nil {
		t.Fatalf("connecting to the live Redis: %v", err)
	}
	app := os.Getenv("ENROLL_REDIS_LIVE_APP")
	if app == "" {
		app = "node-control"
	}
	prefix := app + ":enroll-livetest-" + randHex(4)
	key := base64.StdEncoding.EncodeToString(randBytes(32))
	c, err := statestore.NewCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewRedisStore(rdb, secrets, c, prefix, testKey, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		var cursor uint64
		for {
			keys, next, err := rdb.Scan(ctx, cursor, prefix+":*", 100).Result()
			if err != nil {
				t.Errorf("cleanup scan of %s:* failed; delete those keys by hand: %v", prefix, err)
				break
			}
			if len(keys) > 0 {
				if err := rdb.Del(ctx, keys...).Err(); err != nil {
					t.Errorf("cleanup delete under %s:* failed: %v", prefix, err)
					break
				}
			}
			if cursor = next; cursor == 0 {
				break
			}
		}
		s.Close()
	})
	return s
}

func TestLiveRedisTTLOnlyOnLinks(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s := newLiveStore(t, clock)
	e := contractEnv{b: s, clock: clock}
	d, _ := e.create(t, "phone", time.Hour)
	tok, err := s.NewLink(d.ID, 10*time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ttl, err := s.rdb.PTTL(ctx, s.tokenKey(hashToken(tok))).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 10*time.Minute || ttl > 11*time.Minute {
		t.Fatalf("link TTL = %v, want just under the lifetime plus the margin", ttl)
	}
	for _, k := range []string{s.deviceKey(d.ID), s.metaKey()} {
		// -1 (no expiry) comes back as a negative duration constant.
		if ttl, err := s.rdb.PTTL(ctx, k).Result(); err != nil || ttl != -1 {
			t.Fatalf("%s TTL = %v, %v; devices and the counter must never expire", k, ttl, err)
		}
	}
}

func TestLiveRedisExpiredLinkIsNotRevivedBySpend(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s := newLiveStore(t, clock)
	e := contractEnv{b: s, clock: clock}
	d, _ := e.create(t, "phone", time.Hour)
	tok, _ := s.NewLink(d.ID, time.Minute, 5)
	key := s.tokenKey(hashToken(tok))
	ctx := context.Background()

	// Simulate the TTL having fired by removing the key, as Redis would.
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		t.Fatal(err)
	}
	got, err := s.ConsumeToken(tok)
	mustNil(t, got, err, "consume after the key is gone")
	if err := spendScript.Eval(ctx, s.rdb, []string{key, s.deviceKey(d.ID)}, "1", "1").Err(); err != nil {
		t.Fatalf("the real server rejected the spend script: %v", err)
	}
	if n, err := s.rdb.Exists(ctx, key).Result(); err != nil || n != 0 {
		t.Fatalf("the spend script recreated an expired link (exists=%d, err=%v)", n, err)
	}
}

func TestLiveRedisSnapshotRoundTrip(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s := newLiveStore(t, clock)
	e := contractEnv{b: s, clock: clock}
	a, _ := e.create(t, "a", time.Hour)
	if err := s.Approve("ingress-one", "fp"); err != nil {
		t.Fatal(err)
	}
	snap, err := s.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 || snap.Devices[0].ID != a.ID || len(snap.Approvals) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	ok, err := s.HasData()
	if err != nil || !ok {
		t.Fatalf("HasData = %v, %v", ok, err)
	}
}

// TestLiveImportOfADatabaseCopy imports a copy of a real enrolment database into a throwaway prefix on the real Redis,
// compares the result with the source and removes its keys. Set ENROLL_LIVE_IMPORT_DB to the path of the copy (with its
// -wal and -shm files next to it, if any). The database is opened read-only. A dummy secret key is used on both sides,
// so subscription secrets are compared for equality (same ids and epochs give the same secret) without the real key.
// Nothing about the devices is printed: counts only.
func TestLiveImportOfADatabaseCopy(t *testing.T) {
	path := os.Getenv("ENROLL_LIVE_IMPORT_DB")
	if path == "" {
		t.Skip("ENROLL_LIVE_IMPORT_DB is not set")
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src, err := OpenStoreReadOnly(path, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	dst := newLiveStore(t, &fakeClock{now: time.Now()})

	devices, links, approvals, err := Import(src, dst, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("imported %d devices, %d links, %d approvals", devices, links, approvals)

	want, err := src.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	got, err := dst.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("the device lists differ after the import (%d vs %d devices)", len(want), len(got))
	}
	for _, d := range want {
		a, err1 := src.SubscriptionSecret(d.ID)
		b, err2 := dst.SubscriptionSecret(d.ID)
		if err1 != nil || err2 != nil || a != b {
			t.Fatalf("subscription secret of device %d differs (errors %v, %v)", d.ID, err1, err2)
		}
	}
	wc, err1 := src.ActiveCredentials()
	gc, err2 := dst.ActiveCredentials()
	if err1 != nil || err2 != nil || !reflect.DeepEqual(wc, gc) {
		t.Fatalf("active credentials differ (%d vs %d; errors %v, %v)", len(wc), len(gc), err1, err2)
	}
	wa, _ := src.ApprovalRows()
	ga, _ := dst.ApprovalRows()
	// A copy of a database from before approvals existed has no such table: the source reads as nothing, not an error.
	if (len(wa) > 0 || len(ga) > 0) && !reflect.DeepEqual(wa, ga) {
		t.Fatalf("approvals differ (%d vs %d)", len(wa), len(ga))
	}
	if _, _, _, err := Import(src, dst, false); !errors.Is(err, ErrPopulated) {
		t.Fatalf("a second import must be refused, got %v", err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("the source database file changed")
	}
}
