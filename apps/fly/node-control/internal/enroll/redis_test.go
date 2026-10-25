package enroll

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/statestore"
)

func redisEnv(t *testing.T) (*RedisStore, contractEnv) {
	t.Helper()
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s, mr := newTestRedisStore(t, clock, testCipher(t))
	return s, contractEnv{b: s, clock: clock, mr: mr}
}

func TestRedisTTLOnlyOnLinks(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	tok, err := s.NewLink(d.ID, 10*time.Minute, 1)
	if err != nil {
		t.Fatal(err)
	}
	if ttl := e.mr.TTL(s.tokenKey(hashToken(tok))); ttl != 11*time.Minute {
		t.Fatalf("link TTL = %v, want the lifetime plus the margin", ttl)
	}
	for _, k := range []string{s.deviceKey(d.ID), s.metaKey()} {
		if ttl := e.mr.TTL(k); ttl != 0 {
			t.Fatalf("%s has a TTL (%v); devices must never expire", k, ttl)
		}
	}
}

func TestRedisExpiredLinkIsNotRevivedBySpend(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	tok, _ := s.NewLink(d.ID, time.Minute, 5)
	key := s.tokenKey(hashToken(tok))

	e.mr.FastForward(2*time.Minute + time.Second) // Redis drops the key
	if e.mr.Exists(key) {
		t.Fatal("link key still exists after its TTL")
	}
	got, err := s.ConsumeToken(tok)
	mustNil(t, got, err, "consume after the key expired")
	// A blind HINCRBY would have created a fresh hash with no TTL; the guarded script must not.
	if err := spendScript.Eval(context.Background(), s.rdb, []string{key}, "1").Err(); err != nil {
		t.Fatal(err)
	}
	if e.mr.Exists(key) {
		t.Fatal("the spend script recreated an expired link")
	}
}

func TestRedisExpiredByStoredExpiryEvenIfKeyLives(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	tok, _ := s.NewLink(d.ID, time.Minute, 5)
	e.clock.Advance(2 * time.Minute) // the store's clock moves; Redis's does not, so the key is still there
	if !e.mr.Exists(s.tokenKey(hashToken(tok))) {
		t.Fatal("test setup: key should still exist")
	}
	got, err := s.ConsumeToken(tok)
	mustNil(t, got, err, "consume past the stored expiry")
}

func TestRedisValuesAreSealed(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "secret-phone-name", time.Hour)
	if err := s.Approve("ingress-one", "fp-value"); err != nil {
		t.Fatal(err)
	}
	meta := e.mr.HGet(s.deviceKey(d.ID), "meta")
	for _, plain := range []string{"secret-phone-name", "alice", "sing-box", d.UUIDs["auto"]} {
		if strings.Contains(meta, plain) {
			t.Fatalf("device record holds %q in clear", plain)
		}
	}
	if ap := e.mr.HGet(s.approvalsKey(), "ingress-one"); strings.Contains(ap, "fp-value") {
		t.Fatal("approval record holds the fingerprint in clear")
	}
}

func TestRedisWrongKeyOrMovedValueIsRefused(t *testing.T) {
	s, e := redisEnv(t)
	d, _ := e.create(t, "phone", time.Hour)
	other, err := statestore.NewCipher(base64.StdEncoding.EncodeToString([]byte("ffffffffffffffffffffffffffffffff")))
	if err != nil {
		t.Fatal(err)
	}
	s2 := *s
	s2.cipher = other
	if _, err := s2.GetDevice(d.ID); err == nil {
		t.Fatal("a device sealed with another key was read")
	}
	// A record copied to another device id must not open (the id is part of the authenticated data).
	d2, _ := e.create(t, "other", time.Hour)
	e.mr.HSet(s.deviceKey(d2.ID), "meta", e.mr.HGet(s.deviceKey(d.ID), "meta"))
	if _, err := s.GetDevice(d2.ID); err == nil {
		t.Fatal("a device record moved to another id was accepted")
	}
}

func TestRedisPlaintextModeOnlyWhenNoCipher(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1_000_000, 0)}
	s, mr := newTestRedisStore(t, clock, nil)
	d, _, err := s.CreateDevice("phone", "alice", "sing-box", clock.Now().Unix()+3600)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mr.HGet(s.deviceKey(d.ID), "meta"), "alice") {
		t.Fatal("without a cipher the record should be stored as JSON")
	}
	// With a cipher configured later, an unsealed record is refused rather than trusted.
	s.cipher = testCipher(t)
	if _, err := s.GetDevice(d.ID); err == nil {
		t.Fatal("an unsealed record was accepted by a sealing store")
	}
}

func TestRedisErrorsNeverCarryTheURLOrPassword(t *testing.T) {
	s, e := redisEnv(t)
	e.mr.Close()
	for name, err := range map[string]error{
		"create":  func() error { _, _, err := s.CreateDevice("a", "b", "c", 1); return err }(),
		"list":    func() error { _, err := s.ListDevices(); return err }(),
		"creds":   func() error { _, err := s.ActiveCredentials(); return err }(),
		"link":    func() error { _, err := s.NewLink(1, time.Hour, 1); return err }(),
		"consume": func() error { _, err := s.ConsumeToken("x"); return err }(),
		"approve": s.Approve("n", "f"),
	} {
		if err == nil {
			t.Fatalf("%s: no error with Redis down", name)
		}
		if strings.Contains(err.Error(), "s3cretpw") {
			t.Fatalf("%s: error carries the password: %v", name, err)
		}
	}
}

func TestRedisReadFailureIsNeverAnEmptySet(t *testing.T) {
	s, e := redisEnv(t)
	e.create(t, "phone", time.Hour)
	e.mr.Close()
	if creds, err := s.ActiveCredentials(); err == nil {
		t.Fatalf("ActiveCredentials with Redis down returned %v and no error", creds)
	}
	if devs, err := s.ListDevices(); err == nil {
		t.Fatalf("ListDevices with Redis down returned %v and no error", devs)
	}
}

func TestRedisPrefixValidation(t *testing.T) {
	for _, bad := range []string{"", "nocolon", ":x", "a:", "a*:b", "a:b[c"} {
		if err := validatePrefix(bad); err == nil {
			t.Fatalf("prefix %q accepted", bad)
		}
	}
	if err := validatePrefix("node-control:enroll"); err != nil {
		t.Fatal(err)
	}
}

func TestRedisDeviceIDsAreNeverReused(t *testing.T) {
	s, e := redisEnv(t)
	a, _ := e.create(t, "a", time.Hour)
	e.mr.Del(s.deviceKey(a.ID)) // even if a record were lost, its id is not handed out again
	b, _ := e.create(t, "b", time.Hour)
	if b.ID <= a.ID {
		t.Fatalf("id %d reused after %d", b.ID, a.ID)
	}
}
