package enroll

import (
	"encoding/base64"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/statestore"
)

type contractEnv struct {
	b     Backend
	clock *fakeClock
	mr    *miniredis.Miniredis // nil for SQLite
}

type backendFactory struct {
	name string
	open func(t *testing.T) contractEnv
}

func testCipher(t *testing.T) *statestore.Cipher {
	t.Helper()
	c, err := statestore.NewCipher(base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestRedisStore(t *testing.T, clock *fakeClock, c *statestore.Cipher) (*RedisStore, *miniredis.Miniredis) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb, secrets, err := statestore.OpenClient("redis://:s3cretpw@" + mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewRedisStore(rdb, secrets, c, "node-control:enroll", testKey, clock.Now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, mr
}

var backendFactories = []backendFactory{
	{"sqlite", func(t *testing.T) contractEnv {
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		s, err := OpenStore(filepath.Join(t.TempDir(), "c.db"), testKey, clock.Now)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return contractEnv{b: s, clock: clock}
	}},
	{"redis", func(t *testing.T) contractEnv {
		clock := &fakeClock{now: time.Unix(1_000_000, 0)}
		s, mr := newTestRedisStore(t, clock, testCipher(t))
		return contractEnv{b: s, clock: clock, mr: mr}
	}},
}

func eachBackend(t *testing.T, fn func(t *testing.T, e contractEnv)) {
	for _, f := range backendFactories {
		t.Run(f.name, func(t *testing.T) { fn(t, f.open(t)) })
	}
}

func (e contractEnv) create(t *testing.T, name string, ttl time.Duration) (Device, string) {
	t.Helper()
	d, secret, err := e.b.CreateDevice(name, "alice", "sing-box", e.clock.Now().Add(ttl).Unix())
	if err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}
	return d, secret
}

func TestContractCreateGetList(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		if devs, err := e.b.ListDevices(); err != nil || len(devs) != 0 {
			t.Fatalf("empty list = %v, %v", devs, err)
		}
		a, _ := e.create(t, "a", time.Hour)
		b, _ := e.create(t, "b", time.Hour)
		if b.ID <= a.ID {
			t.Fatalf("ids not increasing: %d then %d", a.ID, b.ID)
		}
		if len(a.UUIDs) != 3 || a.Owner != "alice" || a.Client != "sing-box" || a.Revoked {
			t.Fatalf("device = %+v", a)
		}
		got, err := e.b.GetDevice(a.ID)
		if err != nil || got.Name != "a" || got.UUIDs["auto"] != a.UUIDs["auto"] {
			t.Fatalf("GetDevice = %+v, %v", got, err)
		}
		if _, err := e.b.GetDevice(999); err != ErrNotFound {
			t.Fatalf("GetDevice(999) err = %v", err)
		}
		devs, err := e.b.ListDevices()
		if err != nil || len(devs) != 2 || devs[0].ID != a.ID || devs[1].ID != b.ID {
			t.Fatalf("ListDevices = %+v, %v", devs, err)
		}
	})
}

func TestContractSecrets(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		d, secret := e.create(t, "phone", time.Hour)
		again, err := e.b.SubscriptionSecret(d.ID)
		if err != nil || again != secret {
			t.Fatalf("SubscriptionSecret = %q, %v (want %q)", again, err, secret)
		}
		got, err := e.b.VerifySecret(secret)
		mustDevice(t, got, err, "verify")
		for _, bad := range []string{"", "x", "1", "1.", ".abc", "abc.def", "99999999999999.x", secret + "x"} {
			g, err := e.b.VerifySecret(bad)
			mustNil(t, g, err, "verify "+bad)
		}
		if err := e.b.RotateSecret(d.ID); err != nil {
			t.Fatal(err)
		}
		g, err := e.b.VerifySecret(secret)
		mustNil(t, g, err, "old secret after rotate")
		fresh, _ := e.b.SubscriptionSecret(d.ID)
		if fresh == secret {
			t.Fatal("rotate did not change the secret")
		}
		mustDevice(t, mustVerify(e.b, fresh), nil, "new secret")
		if _, err := e.b.SubscriptionSecret(999); err != ErrNotFound {
			t.Fatalf("SubscriptionSecret(999) err = %v", err)
		}
	})
}

func mustVerify(b Backend, s string) *Device { d, _ := b.VerifySecret(s); return d }

func TestContractSecretStaysValidAcrossBackends(t *testing.T) {
	// The same key, id and epoch give the same secret on both stores, so a migration keeps issued links working.
	var secrets []string
	for _, f := range backendFactories {
		e := f.open(t)
		_, s := e.create(t, "x", time.Hour)
		secrets = append(secrets, s)
	}
	if secrets[0] != secrets[1] {
		t.Fatalf("secrets differ: %q vs %q", secrets[0], secrets[1])
	}
}

func TestContractRevokeExtend(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		d, secret := e.create(t, "phone", time.Hour)
		if err := e.b.Revoke(d.ID); err != nil {
			t.Fatal(err)
		}
		got, _ := e.b.GetDevice(d.ID)
		if !got.Revoked {
			t.Fatal("not revoked")
		}
		mustNil(t, mustVerify(e.b, secret), nil, "revoked device secret")

		d2, secret2 := e.create(t, "other", time.Hour)
		e.clock.Advance(2 * time.Hour)
		mustNil(t, mustVerify(e.b, secret2), nil, "expired device secret")
		if err := e.b.Extend(d2.ID, e.clock.Now().Unix()+3600); err != nil {
			t.Fatal(err)
		}
		mustDevice(t, mustVerify(e.b, secret2), nil, "extended device secret")
	})
}

func TestContractUpdatesOnMissingDeviceChangeNothing(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		if err := e.b.Revoke(42); err != nil {
			t.Fatal(err)
		}
		if err := e.b.Extend(42, 5); err != nil {
			t.Fatal(err)
		}
		if err := e.b.RotateSecret(42); err != nil {
			t.Fatal(err)
		}
		if devs, err := e.b.ListDevices(); err != nil || len(devs) != 0 {
			t.Fatalf("stray device after updates on a missing id: %+v, %v", devs, err)
		}
		if _, err := e.b.NewLink(42, time.Hour, 1); err == nil {
			t.Fatal("NewLink on a missing device must fail")
		}
	})
}

func TestContractLinks(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		d, _ := e.create(t, "phone", time.Hour)
		tok, err := e.b.NewLink(d.ID, 10*time.Minute, 2)
		if err != nil || tok == "" {
			t.Fatalf("NewLink = %q, %v", tok, err)
		}
		for i := 0; i < 2; i++ {
			got, err := e.b.ConsumeToken(tok)
			mustDevice(t, got, err, "consume")
			if got.ID != d.ID || got.UUIDs["exit"] != d.UUIDs["exit"] {
				t.Fatalf("consumed device = %+v", got)
			}
		}
		g, err := e.b.ConsumeToken(tok)
		mustNil(t, g, err, "third use")
		g, err = e.b.ConsumeToken("unknown")
		mustNil(t, g, err, "unknown token")

		tok2, _ := e.b.NewLink(d.ID, 10*time.Minute, 5)
		e.clock.Advance(11 * time.Minute)
		g, err = e.b.ConsumeToken(tok2)
		mustNil(t, g, err, "expired link")

		tok3, _ := e.b.NewLink(d.ID, 10*time.Minute, 5)
		if err := e.b.Revoke(d.ID); err != nil {
			t.Fatal(err)
		}
		g, err = e.b.ConsumeToken(tok3)
		mustNil(t, g, err, "link of a revoked device")
	})
}

func TestContractConcurrentConsumeSpendsOnce(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		d, _ := e.create(t, "phone", time.Hour)
		tok, _ := e.b.NewLink(d.ID, time.Hour, 1)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got, err := e.b.ConsumeToken(tok); err == nil && got != nil {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("a one-use link was spent %d times", wins.Load())
		}
	})
}

func TestContractActiveCredentials(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		creds, err := e.b.ActiveCredentials()
		if err != nil || creds == nil || len(creds) != 0 {
			t.Fatalf("empty = %#v, %v (must be a non-nil empty slice)", creds, err)
		}
		a, _ := e.create(t, "a", time.Hour)
		b, _ := e.create(t, "b", time.Hour)
		c, _ := e.create(t, "c", time.Minute)
		e.b.Revoke(b.ID)
		e.clock.Advance(2 * time.Minute) // c expires
		creds, err = e.b.ActiveCredentials()
		if err != nil || len(creds) != 3 {
			t.Fatalf("creds = %+v, %v", creds, err)
		}
		for i, m := range []string{"auto", "direct", "exit"} {
			if creds[i].Mode != m || creds[i].UUID != a.UUIDs[m] || creds[i].NotAfter != a.ExpiresAt {
				t.Fatalf("creds[%d] = %+v", i, creds[i])
			}
		}
		_ = c
	})
}

func TestContractApprovals(t *testing.T) {
	eachBackend(t, func(t *testing.T, e contractEnv) {
		if rows, err := e.b.ApprovalRows(); err != nil || len(rows) != 0 {
			t.Fatalf("empty approvals = %v, %v", rows, err)
		}
		if err := e.b.Approve("one", "fp1"); err != nil {
			t.Fatal(err)
		}
		if err := e.b.ApproveNode("two", "fp2", 7, "mkey:abc"); err != nil {
			t.Fatal(err)
		}
		rows, _ := e.b.ApprovalRows()
		if rows["one"] != (ApprovalRow{Fingerprint: "fp1"}) || rows["two"] != (ApprovalRow{Fingerprint: "fp2", NodeID: 7, MachineKey: "mkey:abc"}) {
			t.Fatalf("rows = %+v", rows)
		}
		if err := e.b.ApproveNode("two", "fp3", 8, "mkey:def"); err != nil {
			t.Fatal(err)
		}
		fps, _ := e.b.Approvals()
		if fps["two"] != "fp3" || fps["one"] != "fp1" {
			t.Fatalf("fps = %+v", fps)
		}
		if err := e.b.Unapprove("one"); err != nil || e.b.Unapprove("missing") != nil {
			t.Fatal("unapprove failed")
		}
		if rows, _ := e.b.ApprovalRows(); len(rows) != 1 {
			t.Fatalf("rows after unapprove = %+v", rows)
		}
	})
}
