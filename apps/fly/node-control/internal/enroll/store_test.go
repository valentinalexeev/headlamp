package enroll

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var testKey = []byte("test-key-0123456789abcdef")

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type env struct {
	t     *testing.T
	dir   string
	path  string
	clock *fakeClock
	store *Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	e := &env{t: t, dir: dir, path: filepath.Join(dir, "enroll.db"), clock: &fakeClock{now: time.Unix(1_000_000, 0)}}
	e.store = e.open(e.path, testKey)
	return e
}

func (e *env) open(path string, key []byte) *Store {
	e.t.Helper()
	s, err := OpenStore(path, key, e.clock.Now)
	if err != nil {
		e.t.Fatalf("OpenStore: %v", err)
	}
	e.t.Cleanup(func() { s.Close() })
	return s
}

func (e *env) make(name string, expiresAt int64) (Device, string) {
	e.t.Helper()
	if expiresAt == 0 {
		expiresAt = e.clock.Now().Unix() + 3600
	}
	d, secret, err := e.store.CreateDevice(name, "alice", "sing-box", expiresAt)
	if err != nil {
		e.t.Fatalf("CreateDevice: %v", err)
	}
	return d, secret
}

func mustNil(t *testing.T, d *Device, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error %v", what, err)
	}
	if d != nil {
		t.Fatalf("%s: expected nil device, got %+v", what, *d)
	}
}

func mustDevice(t *testing.T, d *Device, err error, what string) *Device {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: unexpected error %v", what, err)
	}
	if d == nil {
		t.Fatalf("%s: expected a device, got nil", what)
	}
	return d
}

func TestCreateDevice(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	if len(dev.UUIDs) != 3 {
		t.Fatalf("uuids = %v", dev.UUIDs)
	}
	seen := map[string]bool{}
	for _, m := range Modes {
		u, ok := dev.UUIDs[m]
		if !ok {
			t.Fatalf("missing mode %s", m)
		}
		seen[u] = true
	}
	if len(seen) != 3 {
		t.Fatalf("uuids not distinct: %v", dev.UUIDs)
	}
	if dev.Revoked || dev.Owner != "alice" || dev.Client != "sing-box" {
		t.Fatalf("device = %+v", dev)
	}
	if len(secret) < 22 {
		t.Fatalf("secret too short: %q", secret)
	}
	again, err := e.store.SubscriptionSecret(dev.ID)
	if err != nil || again != secret {
		t.Fatalf("SubscriptionSecret = %q, %v", again, err)
	}
	if !strings.HasPrefix(secret, "1.") {
		t.Fatalf("secret %q lacks id prefix", secret)
	}
	got, err := e.store.VerifySecret(secret)
	if g := mustDevice(t, got, err, "verify"); !reflect.DeepEqual(*g, dev) {
		t.Fatalf("verify = %+v, want %+v", *g, dev)
	}
}

func TestModesOrder(t *testing.T) {
	if !reflect.DeepEqual(Modes, []string{"auto", "direct", "exit"}) {
		t.Fatalf("Modes = %v", Modes)
	}
}

func TestSecondConsumeOfSingleUseTokenIsNil(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, err := e.store.NewLink(dev.ID, 24*time.Hour, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.store.ConsumeToken(tok)
	want := dev // a spend also marks the device as enrolled, now and once
	want.EnrolledAt, want.Enrolments = e.clock.Now().Unix(), 1
	if g := mustDevice(t, got, err, "first"); !reflect.DeepEqual(*g, want) {
		t.Fatalf("got %+v want %+v", *g, want)
	}
	got, err = e.store.ConsumeToken(tok)
	mustNil(t, got, err, "second")
}

func TestMultiUseToken(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, 24*time.Hour, 2)
	for i := 0; i < 2; i++ {
		d, err := e.store.ConsumeToken(tok)
		mustDevice(t, d, err, "use")
	}
	d, err := e.store.ConsumeToken(tok)
	mustNil(t, d, err, "third")
}

func TestTokenPastTTLIsNil(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, 60*time.Second, 1)
	e.clock.Advance(61 * time.Second)
	d, err := e.store.ConsumeToken(tok)
	mustNil(t, d, err, "expired token")
}

func TestUnknownTokenIsNil(t *testing.T) {
	e := newEnv(t)
	d, err := e.store.ConsumeToken("nope")
	mustNil(t, d, err, "unknown")
}

func TestTokenOfRevokedOrExpiredDeviceIsNil(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("a", 0)
	tok, _ := e.store.NewLink(dev.ID, 24*time.Hour, 1)
	if err := e.store.Revoke(dev.ID); err != nil {
		t.Fatal(err)
	}
	d, err := e.store.ConsumeToken(tok)
	mustNil(t, d, err, "revoked device")

	dev2, _ := e.make("b", e.clock.Now().Unix()+10)
	tok2, _ := e.store.NewLink(dev2.ID, 1000*time.Second, 1)
	e.clock.Advance(11 * time.Second)
	d, err = e.store.ConsumeToken(tok2)
	mustNil(t, d, err, "expired device")
}

func TestConcurrentConsumeExactlyOneWins(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, 24*time.Hour, 1)
	if n := consumeConcurrently(t, e.store, tok, 8); n != 1 {
		t.Fatalf("winners = %d", n)
	}
}

func TestConcurrentConsumeNeverExceedsMaxUses(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, 24*time.Hour, 3)
	if n := consumeConcurrently(t, e.store, tok, 40); n != 3 {
		t.Fatalf("winners = %d, want 3", n)
	}
}

func consumeConcurrently(t *testing.T, s *Store, tok string, workers int) int {
	t.Helper()
	var wins atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			d, err := s.ConsumeToken(tok)
			if err != nil {
				t.Errorf("consume: %v", err)
				return
			}
			if d != nil {
				wins.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	return int(wins.Load())
}

func TestSecretAfterRevokeIsNil(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	e.store.Revoke(dev.ID)
	d, err := e.store.VerifySecret(secret)
	mustNil(t, d, err, "revoked")
	list, err := e.store.ListDevices()
	if err != nil || !list[0].Revoked {
		t.Fatalf("list = %+v, %v", list, err)
	}
}

func TestSecretAfterExpiryIsNilAndExtendRestores(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	e.clock.Advance(7200 * time.Second)
	d, err := e.store.VerifySecret(secret)
	mustNil(t, d, err, "expired")
	if err := e.store.Extend(dev.ID, e.clock.Now().Unix()+100); err != nil {
		t.Fatal(err)
	}
	d, err = e.store.VerifySecret(secret)
	if g := mustDevice(t, d, err, "extended"); g.ID != dev.ID {
		t.Fatalf("id = %d", g.ID)
	}
}

func TestUnknownSecretIsNil(t *testing.T) {
	e := newEnv(t)
	d, err := e.store.VerifySecret("nope")
	mustNil(t, d, err, "unknown")
}

func TestDBFileHasNoPlaintext(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, 24*time.Hour, 1)
	var data []byte
	for _, p := range []string{e.path, e.path + "-wal"} {
		if b, err := os.ReadFile(p); err == nil {
			data = append(data, b...)
		}
	}
	if bytes.Contains(data, []byte(secret)) || bytes.Contains(data, []byte(tok)) {
		t.Fatal("plaintext secret or token found in the database files")
	}
}

func TestTokenIs128BitAndStoredHashed(t *testing.T) {
	e := newEnv(t)
	dev, _ := e.make("phone", 0)
	tok, _ := e.store.NewLink(dev.ID, time.Hour, 1)
	if len(tok) != 22 {
		t.Fatalf("token %q is not 16 bytes of base64url", tok)
	}
	var h string
	if err := e.store.db.QueryRow("SELECT token_hash FROM tokens").Scan(&h); err != nil {
		t.Fatal(err)
	}
	if len(h) != 64 || h == tok {
		t.Fatalf("token_hash = %q", h)
	}
}

func TestActiveCredentials(t *testing.T) {
	e := newEnv(t)
	live, _ := e.make("live", 0)
	revoked, _ := e.make("rev", 0)
	e.store.Revoke(revoked.ID)
	e.make("exp", e.clock.Now().Unix()+5)
	e.clock.Advance(10 * time.Second)
	creds, err := e.store.ActiveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if len(creds) != 3 {
		t.Fatalf("creds = %+v", creds)
	}
	modes := map[string]bool{}
	for _, c := range creds {
		modes[c.Mode] = true
		if c.UUID != live.UUIDs[c.Mode] || c.NotAfter != live.ExpiresAt {
			t.Fatalf("cred %+v does not match %+v", c, live)
		}
	}
	if len(modes) != 3 {
		t.Fatalf("modes = %v", modes)
	}
	// Ordered by device id then mode.
	if creds[0].Mode != "auto" || creds[1].Mode != "direct" || creds[2].Mode != "exit" {
		t.Fatalf("order = %+v", creds)
	}
}

func TestCredentialJSONShape(t *testing.T) {
	b, err := json.Marshal(Credential{UUID: "u", Mode: "auto", NotAfter: 5})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	json.Unmarshal(b, &m)
	if len(m) != 3 || m["uuid"] != "u" || m["mode"] != "auto" || m["not_after"] != float64(5) {
		t.Fatalf("json = %s", b)
	}
}

func TestEmptyActiveCredentialsMarshalToEmptyArray(t *testing.T) {
	e := newEnv(t)
	creds, err := e.store.ActiveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(creds)
	if string(b) != "[]" {
		t.Fatalf("json = %s", b)
	}
}

func TestDeviceHasNoSecretFields(t *testing.T) {
	typ := reflect.TypeOf(Device{})
	var got []string
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	// EnrolledAt, Enrolments and LastActive are activity times and a count, not secrets.
	want := []string{"ID", "Name", "Owner", "Client", "ExpiresAt", "Revoked", "UUIDs", "EnrolledAt", "Enrolments", "LastActive"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("fields = %v", got)
	}
}

func TestShortOrEmptyKeyRejected(t *testing.T) {
	dir := t.TempDir()
	for _, key := range [][]byte{nil, {}, []byte("short")} {
		if s, err := OpenStore(filepath.Join(dir, "k.db"), key, time.Now); err == nil {
			s.Close()
			t.Fatalf("key %q accepted", key)
		}
	}
}

func TestSubscriptionSecretUnknownDeviceIsNotFound(t *testing.T) {
	e := newEnv(t)
	if _, err := e.store.SubscriptionSecret(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := e.store.GetDevice(999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetDevice err = %v", err)
	}
}

func TestSecretDependsOnKeyAndIsDeterministic(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	again := e.open(e.path, testKey)
	if got, _ := again.SubscriptionSecret(dev.ID); got != secret {
		t.Fatalf("got %q want %q", got, secret)
	}
	other := e.open(e.path, []byte("another-key-0123456789"))
	if got, _ := other.SubscriptionSecret(dev.ID); got == secret {
		t.Fatal("different key gave the same secret")
	}
	d, err := other.VerifySecret(secret)
	mustNil(t, d, err, "other key")
}

func TestMalformedAndForgedSecretsAreNil(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	mac := strings.SplitN(secret, ".", 2)[1]
	flipped := mac[:len(mac)-1] + "A"
	if strings.HasSuffix(mac, "A") {
		flipped = mac[:len(mac)-1] + "B"
	}
	id := dev.ID
	bad := []string{
		"", ".", itoa(id), itoa(id) + ".", "." + mac, "x." + mac, itoa(id+1) + "." + mac,
		itoa(id) + "." + flipped, itoa(id) + "." + mac + ".extra", "é.é",
		strings.Repeat("9", 25) + ".x", strings.Repeat("9", 5000) + ".x",
		"+" + itoa(id) + "." + mac, " " + itoa(id) + "." + mac,
	}
	for _, s := range bad {
		name := s
		if len(name) > 30 {
			name = name[:30]
		}
		t.Run(name, func(t *testing.T) {
			d, err := e.store.VerifySecret(s)
			mustNil(t, d, err, "bad secret")
		})
	}
}

func itoa(n int64) string {
	b, _ := json.Marshal(n)
	return string(b)
}

func TestRotateSecret(t *testing.T) {
	e := newEnv(t)
	dev, old := e.make("phone", 0)
	if err := e.store.RotateSecret(dev.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := e.store.SubscriptionSecret(dev.ID)
	if fresh == old {
		t.Fatal("secret did not change")
	}
	d, err := e.store.VerifySecret(old)
	mustNil(t, d, err, "old secret")
	d, err = e.store.VerifySecret(fresh)
	if g := mustDevice(t, d, err, "new secret"); g.ID != dev.ID {
		t.Fatalf("id = %d", g.ID)
	}
}

func TestOldDBWithoutEpochIsMigrated(t *testing.T) {
	e := newEnv(t)
	path := filepath.Join(e.dir, "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec("CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL," +
		" owner TEXT NOT NULL, client TEXT NOT NULL, expires_at INTEGER NOT NULL," +
		" revoked INTEGER NOT NULL DEFAULT 0, secret_hash TEXT NOT NULL UNIQUE);" +
		"INSERT INTO devices (name, owner, client, expires_at, secret_hash)" +
		" VALUES ('a','b','sing-box', 9999999999, 'h');")
	if err != nil {
		t.Fatal(err)
	}
	raw.Close()
	s := e.open(path, testKey)
	list, err := s.ListDevices()
	if err != nil || len(list) != 1 {
		t.Fatalf("list = %v, %v", list, err)
	}
	sec, _ := s.SubscriptionSecret(list[0].ID)
	d, err := s.VerifySecret(sec)
	mustDevice(t, d, err, "migrated verify")
	nd, nsec, err := s.CreateDevice("n", "o", "sing-box", 9999999999)
	if err != nil {
		t.Fatal(err)
	}
	d, err = s.VerifySecret(nsec)
	if g := mustDevice(t, d, err, "new on legacy"); g.ID != nd.ID {
		t.Fatalf("id = %d", g.ID)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	e := newEnv(t)
	dev, secret := e.make("phone", 0)
	again := e.open(e.path, testKey)
	d, err := again.VerifySecret(secret)
	if g := mustDevice(t, d, err, "reopen"); !reflect.DeepEqual(*g, dev) {
		t.Fatalf("got %+v want %+v", *g, dev)
	}
}

// ---- compatibility with databases created by the Python store ----

type expectedDevice struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Owner     string            `json:"owner"`
	Client    string            `json:"client"`
	ExpiresAt int64             `json:"expires_at"`
	Revoked   bool              `json:"revoked"`
	UUIDs     map[string]string `json:"uuids"`
	Secret    string            `json:"secret"`
}

type expectedSet struct {
	Devices     []expectedDevice `json:"devices"`
	Credentials []Credential     `json:"credentials"`
}

type expectedFile struct {
	Key     string      `json:"key"`
	Now     int64       `json:"now"`
	Modern  expectedSet `json:"modern"`
	Legacy  expectedSet `json:"legacy"`
	Token   string      `json:"token_for_device_a"`
	MaxUses int         `json:"token_max_uses"`
}

func loadExpected(t *testing.T) expectedFile {
	t.Helper()
	b, err := os.ReadFile("testdata/python_expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var x expectedFile
	if err := json.Unmarshal(b, &x); err != nil {
		t.Fatal(err)
	}
	return x
}

// copyFixture copies a fixture into a temp dir so the committed file is never modified.
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func openFixture(t *testing.T, name string, x expectedFile) *Store {
	t.Helper()
	s, err := OpenStore(copyFixture(t, name), []byte(x.Key), func() time.Time { return time.Unix(x.Now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func checkSet(t *testing.T, s Backend, want expectedSet) {
	t.Helper()
	list, err := s.ListDevices()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(want.Devices) {
		t.Fatalf("devices = %d, want %d", len(list), len(want.Devices))
	}
	for i, w := range want.Devices {
		g := list[i]
		if g.ID != w.ID || g.Name != w.Name || g.Owner != w.Owner || g.Client != w.Client ||
			g.ExpiresAt != w.ExpiresAt || g.Revoked != w.Revoked || !reflect.DeepEqual(g.UUIDs, w.UUIDs) {
			t.Fatalf("device %d = %+v, want %+v", i, g, w)
		}
		sec, err := s.SubscriptionSecret(w.ID)
		if err != nil || sec != w.Secret {
			t.Fatalf("secret for %d = %q (%v), want %q", w.ID, sec, err, w.Secret)
		}
		d, err := s.VerifySecret(w.Secret)
		if w.Revoked || w.ExpiresAt <= 1_000_000 {
			mustNil(t, d, err, "inactive device secret")
		} else if g := mustDevice(t, d, err, "python secret"); g.ID != w.ID {
			t.Fatalf("verified id %d, want %d", g.ID, w.ID)
		}
	}
	creds, err := s.ActiveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(creds, want.Credentials) {
		t.Fatalf("credentials = %+v, want %+v", creds, want.Credentials)
	}
}

func TestOpensPythonCreatedDB(t *testing.T) {
	x := loadExpected(t)
	t.Run("lists and verifies", func(t *testing.T) {
		checkSet(t, openFixture(t, "python_enroll.db", x), x.Modern)
	})
	t.Run("fixture has a rotated secret", func(t *testing.T) {
		if x.Modern.Devices[0].Secret == "" || x.Modern.Devices[0].Name != "device-a" {
			t.Fatalf("unexpected fixture: %+v", x.Modern.Devices[0])
		}
		var epoch int
		s := openFixture(t, "python_enroll.db", x)
		if err := s.db.QueryRow("SELECT secret_epoch FROM devices WHERE id=1").Scan(&epoch); err != nil || epoch != 1 {
			t.Fatalf("epoch = %d, %v", epoch, err)
		}
	})
	t.Run("consumes a python-issued token", func(t *testing.T) {
		s := openFixture(t, "python_enroll.db", x)
		for i := 0; i < x.MaxUses; i++ {
			d, err := s.ConsumeToken(x.Token)
			mustDevice(t, d, err, "python token")
		}
		d, err := s.ConsumeToken(x.Token)
		mustNil(t, d, err, "exhausted python token")
	})
	t.Run("create revoke extend rotate", func(t *testing.T) {
		s := openFixture(t, "python_enroll.db", x)
		nd, nsec, err := s.CreateDevice("device-z", "owner-z", "clash", x.Now+500)
		if err != nil {
			t.Fatal(err)
		}
		if nd.ID != 4 {
			t.Fatalf("new id = %d", nd.ID)
		}
		d, err := s.VerifySecret(nsec)
		mustDevice(t, d, err, "new device")
		if err := s.Revoke(nd.ID); err != nil {
			t.Fatal(err)
		}
		d, err = s.VerifySecret(nsec)
		mustNil(t, d, err, "revoked")
		// device-c is expired in the fixture; extending brings it back.
		c := x.Modern.Devices[2]
		if err := s.Extend(c.ID, x.Now+9999); err != nil {
			t.Fatal(err)
		}
		d, err = s.VerifySecret(c.Secret)
		mustDevice(t, d, err, "extended")
		if err := s.RotateSecret(c.ID); err != nil {
			t.Fatal(err)
		}
		d, err = s.VerifySecret(c.Secret)
		mustNil(t, d, err, "rotated old secret")
	})
}

func TestOpensLegacyPythonDB(t *testing.T) {
	x := loadExpected(t)
	checkSet(t, openFixture(t, "python_enroll_legacy.db", x), x.Legacy)
}

func TestLegacySecretHashColumnFilled(t *testing.T) {
	x := loadExpected(t)
	s := openFixture(t, "python_enroll_legacy.db", x)
	d1, _, err := s.CreateDevice("device-n1", "owner-n", "sing-box", x.Now+100)
	if err != nil {
		t.Fatalf("legacy insert 1: %v", err)
	}
	d2, _, err := s.CreateDevice("device-n2", "owner-n", "sing-box", x.Now+100)
	if err != nil {
		t.Fatalf("legacy insert 2 (placeholders must be unique): %v", err)
	}
	for _, id := range []int64{d1.ID, d2.ID} {
		var h string
		if err := s.db.QueryRow("SELECT secret_hash FROM devices WHERE id=?", id).Scan(&h); err != nil {
			t.Fatal(err)
		}
		if len(h) != 32 || strings.Trim(h, "0123456789abcdef") != "" {
			t.Fatalf("placeholder = %q, want 32 hex chars", h)
		}
	}
	var epoch int
	if err := s.db.QueryRow("SELECT secret_epoch FROM devices WHERE id=1").Scan(&epoch); err != nil || epoch != 0 {
		t.Fatalf("migrated epoch = %d, %v", epoch, err)
	}
}

func TestFixturesAreNotModified(t *testing.T) {
	before, _ := os.ReadFile("testdata/python_enroll.db")
	x := loadExpected(t)
	s := openFixture(t, "python_enroll.db", x)
	s.CreateDevice("device-q", "owner-q", "clash", x.Now+10)
	after, _ := os.ReadFile("testdata/python_enroll.db")
	if !bytes.Equal(before, after) {
		t.Fatal("fixture changed")
	}
}

func TestApprovalsStore(t *testing.T) {
	t.Run("empty then approve, replace, remove", func(t *testing.T) {
		s := newStore(t, 1_800_000_000)
		got, err := s.Approvals()
		if err != nil || len(got) != 0 {
			t.Fatalf("%v %v", got, err)
		}
		if err := s.Approve("ingress-3", "fp1"); err != nil {
			t.Fatal(err)
		}
		if err := s.Approve("ingress-3", "fp2"); err != nil { // re-approval replaces
			t.Fatal(err)
		}
		got, _ = s.Approvals()
		if len(got) != 1 || got["ingress-3"] != "fp2" {
			t.Fatalf("%v", got)
		}
		if err := s.Unapprove("ingress-3"); err != nil {
			t.Fatal(err)
		}
		if err := s.Unapprove("ingress-3"); err != nil { // idempotent
			t.Fatal(err)
		}
		if got, _ = s.Approvals(); len(got) != 0 {
			t.Fatalf("%v", got)
		}
	})
}

func TestApprovalsPersistInTheDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.db")
	s, err := OpenStore(path, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Approve("ingress-3", "fp1")
	s.Close()
	s2, err := OpenStore(path, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, _ := s2.Approvals(); got["ingress-3"] != "fp1" {
		t.Fatalf("%v", got)
	}
}

func TestOldDatabaseOpensWithoutApprovalsTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	// the schema as the Python implementation created it: no approvals table
	for _, q := range []string{
		`CREATE TABLE devices (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, owner TEXT NOT NULL, client TEXT NOT NULL, expires_at INTEGER NOT NULL, revoked INTEGER NOT NULL DEFAULT 0, secret_epoch INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE device_uuids (device_id INTEGER NOT NULL REFERENCES devices(id), mode TEXT NOT NULL, uuid TEXT NOT NULL, PRIMARY KEY (device_id, mode))`,
		`CREATE TABLE tokens (id INTEGER PRIMARY KEY AUTOINCREMENT, device_id INTEGER NOT NULL REFERENCES devices(id), token_hash TEXT NOT NULL UNIQUE, expires INTEGER NOT NULL, max_uses INTEGER NOT NULL, uses INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO devices (name, owner, client, expires_at) VALUES ('d','o','sing-box',4000000000)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := OpenStore(path, testKey, nil)
	if err != nil {
		t.Fatalf("an old database must open: %v", err)
	}
	defer s.Close()
	if devs, err := s.ListDevices(); err != nil || len(devs) != 1 {
		t.Fatalf("%v %v", devs, err)
	}
	if got, err := s.Approvals(); err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if err := s.Approve("ingress-3", "fp"); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalPinsTheNodeID(t *testing.T) {
	s := newStore(t, 1_800_000_000)
	if err := s.ApproveNode("ingress-3", "fp1", 7, "mkey:aa"); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ApprovalRows()
	if err != nil || rows["ingress-3"] != (ApprovalRow{Fingerprint: "fp1", NodeID: 7, MachineKey: "mkey:aa"}) {
		t.Fatalf("%v %v", rows, err)
	}
}

func TestApprovalsTableWithoutNodeIDIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mid.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE ingress_approvals (name TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, approved_at INTEGER NOT NULL)`,
		`INSERT INTO ingress_approvals VALUES ('ingress-3','fp1',1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := OpenStore(path, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.ApprovalRows()
	if rows["ingress-3"].Fingerprint != "fp1" || rows["ingress-3"].NodeID != 0 || rows["ingress-3"].MachineKey != "" {
		t.Fatalf("%v", rows)
	}
	if err := s.ApproveNode("ingress-4", "fp2", 4, "mkey:bb"); err != nil {
		t.Fatal(err)
	}
}

func TestApprovalsTableWithNodeIDButNoMachineKeyIsMigrated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mid2.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE ingress_approvals (name TEXT PRIMARY KEY, fingerprint TEXT NOT NULL, approved_at INTEGER NOT NULL, node_id INTEGER NOT NULL DEFAULT 0)`,
		`INSERT INTO ingress_approvals VALUES ('ingress-3','fp1',1,7)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()
	s, err := OpenStore(path, testKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	rows, _ := s.ApprovalRows()
	if rows["ingress-3"] != (ApprovalRow{Fingerprint: "fp1", NodeID: 7}) {
		t.Fatalf("%v", rows)
	}
}
