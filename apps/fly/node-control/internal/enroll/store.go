// Package enroll holds the enrolment state: devices, per-mode UUIDs and expiring one-time links.
//
// The store reads and writes the same SQLite files as the Python implementation it replaces.
// Tokens are 128-bit random values and only their SHA-256 hex digest is stored. Subscription
// secrets are not stored at all: they are derived as
// "<device id>.<base64url(HMAC-SHA256(key, "sub:<id>:<epoch>")[:16])>" and verified by
// recomputation. RotateSecret bumps the epoch, which invalidates the previous secret.
package enroll

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Modes are the routing modes, one UUID each per device. auto: split by the ingress agent's
// configured direct lists; direct: leave from the ingress host itself; exit: leave through the
// exit node.
var Modes = []string{"auto", "direct", "exit"}

// ErrNotFound means a device id does not exist.
var ErrNotFound = errors.New("device not found")

const schema = `
CREATE TABLE IF NOT EXISTS devices (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL,
    owner TEXT NOT NULL,
    client TEXT NOT NULL,
    expires_at INTEGER NOT NULL,
    revoked INTEGER NOT NULL DEFAULT 0,
    secret_epoch INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS device_uuids (
    device_id INTEGER NOT NULL REFERENCES devices(id),
    mode TEXT NOT NULL,
    uuid TEXT NOT NULL,
    PRIMARY KEY (device_id, mode)
);
CREATE TABLE IF NOT EXISTS tokens (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id INTEGER NOT NULL REFERENCES devices(id),
    token_hash TEXT NOT NULL UNIQUE,
    expires INTEGER NOT NULL,
    max_uses INTEGER NOT NULL,
    uses INTEGER NOT NULL DEFAULT 0
);
`

// Device is an enrolled device. It carries no secret material.
type Device struct {
	ID        int64
	Name      string
	Owner     string
	Client    string
	ExpiresAt int64
	Revoked   bool
	UUIDs     map[string]string // mode -> uuid
}

// Credential is one entry of the set pushed to the ingress agent (PUT /v1/credentials body
// {"credentials": [...]}); it mirrors the rows of the Python store's credentials().
type Credential struct {
	UUID     string `json:"uuid"`
	Mode     string `json:"mode"`
	NotAfter int64  `json:"not_after"`
}

// Store is the enrolment store.
type Store struct {
	db           *sql.DB
	key          []byte
	clock        func() time.Time
	legacyHash   bool
	secretHashFn func() string
}

// OpenStore opens (creating and migrating when needed) the database at path. The key must be at
// least 16 bytes. A nil clock means time.Now.
func OpenStore(path string, secretKey []byte, clock func() time.Time) (*Store, error) {
	if len(secretKey) < 16 {
		return nil, errors.New("secret key must be at least 16 bytes")
	}
	if clock == nil {
		clock = time.Now
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	dsn := "file:" + esc + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open enrolment db: %w", err)
	}
	s := &Store{db: db, key: append([]byte(nil), secretKey...), clock: clock, secretHashFn: func() string { return randHex(16) }}
	if err := s.init(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open enrolment db: %w", err)
	}
	return s, nil
}

func (s *Store) init() error {
	ctx := context.Background()
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return err
	}
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info(devices)")
	if err != nil {
		return err
	}
	cols := map[string]bool{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if !cols["secret_epoch"] {
		if _, err := s.db.ExecContext(ctx, "ALTER TABLE devices ADD COLUMN secret_epoch INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	// Databases created before secrets were derived keep a NOT NULL secret_hash column; it is
	// ignored, and filled with a random placeholder on insert.
	s.legacyHash = cols["secret_hash"]
	return nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

func randBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand: " + err.Error())
	}
	return b
}

func randHex(n int) string { return hex.EncodeToString(randBytes(n)) }

func newUUID() string {
	b := randBytes(16)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func hashToken(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// nowSec is the clock as float seconds, matching the Python comparisons.
func (s *Store) nowSec() float64 {
	t := s.clock()
	return float64(t.UnixNano()) / 1e9
}

func (s *Store) mac(deviceID, epoch int64) string {
	m := hmac.New(sha256.New, s.key)
	fmt.Fprintf(m, "sub:%d:%d", deviceID, epoch)
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil)[:16])
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

const deviceCols = "id, name, owner, client, expires_at, revoked"

// scanDevices reads device rows (deviceCols) and attaches their UUIDs.
func scanDevices(ctx context.Context, q queryer, rows *sql.Rows) ([]Device, error) {
	var out []Device
	for rows.Next() {
		var d Device
		var revoked int64
		if err := rows.Scan(&d.ID, &d.Name, &d.Owner, &d.Client, &d.ExpiresAt, &revoked); err != nil {
			rows.Close()
			return nil, err
		}
		d.Revoked = revoked != 0
		out = append(out, d)
	}
	err := rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].UUIDs = map[string]string{}
		ur, err := q.QueryContext(ctx, "SELECT mode, uuid FROM device_uuids WHERE device_id=?", out[i].ID)
		if err != nil {
			return nil, err
		}
		for ur.Next() {
			var mode, u string
			if err := ur.Scan(&mode, &u); err != nil {
				ur.Close()
				return nil, err
			}
			out[i].UUIDs[mode] = u
		}
		err = ur.Err()
		ur.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// GetDevice returns one device, or ErrNotFound.
func (s *Store) GetDevice(id int64) (Device, error) {
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, "SELECT "+deviceCols+" FROM devices WHERE id=?", id)
	if err != nil {
		return Device{}, err
	}
	devs, err := scanDevices(ctx, s.db, rows)
	if err != nil {
		return Device{}, err
	}
	if len(devs) == 0 {
		return Device{}, ErrNotFound
	}
	return devs[0], nil
}

// CreateDevice adds a device with one fresh UUID per mode and returns it with its subscription
// secret.
func (s *Store) CreateDevice(name, owner, client string, expiresAt int64) (Device, string, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, "", err
	}
	defer tx.Rollback()
	var res sql.Result
	if s.legacyHash {
		res, err = tx.ExecContext(ctx,
			"INSERT INTO devices (name, owner, client, expires_at, secret_hash) VALUES (?,?,?,?,?)",
			name, owner, client, expiresAt, s.secretHashFn())
	} else {
		res, err = tx.ExecContext(ctx,
			"INSERT INTO devices (name, owner, client, expires_at) VALUES (?,?,?,?)",
			name, owner, client, expiresAt)
	}
	if err != nil {
		return Device{}, "", err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return Device{}, "", err
	}
	for _, m := range Modes {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO device_uuids (device_id, mode, uuid) VALUES (?,?,?)", id, m, newUUID()); err != nil {
			return Device{}, "", err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT "+deviceCols+" FROM devices WHERE id=?", id)
	if err != nil {
		return Device{}, "", err
	}
	devs, err := scanDevices(ctx, tx, rows)
	if err != nil || len(devs) != 1 {
		return Device{}, "", fmt.Errorf("read back device %d: %v", id, err)
	}
	if err := tx.Commit(); err != nil {
		return Device{}, "", err
	}
	secret, err := s.SubscriptionSecret(id)
	if err != nil {
		return Device{}, "", err
	}
	return devs[0], secret, nil
}

// SubscriptionSecret derives the current secret of a device (ErrNotFound if it does not exist).
func (s *Store) SubscriptionSecret(deviceID int64) (string, error) {
	var epoch int64
	err := s.db.QueryRow("SELECT secret_epoch FROM devices WHERE id=?", deviceID).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(deviceID, 10) + "." + s.mac(deviceID, epoch), nil
}

// RotateSecret bumps the device's epoch, invalidating its previous subscription secret.
func (s *Store) RotateSecret(deviceID int64) error {
	_, err := s.db.Exec("UPDATE devices SET secret_epoch = secret_epoch + 1 WHERE id=?", deviceID)
	return err
}

// NewLink creates a token for the device valid for ttl and max uses; only its hash is stored.
func (s *Store) NewLink(deviceID int64, ttl time.Duration, maxUses int) (string, error) {
	token := base64.RawURLEncoding.EncodeToString(randBytes(16))
	expires := int64(s.nowSec() + ttl.Seconds())
	_, err := s.db.Exec("INSERT INTO tokens (device_id, token_hash, expires, max_uses) VALUES (?,?,?,?)",
		deviceID, hashToken(token), expires, maxUses)
	if err != nil {
		return "", err
	}
	return token, nil
}

// ConsumeToken spends one use of a token and returns its device, or nil when the token is
// unknown, used up, expired, or its device is revoked or expired.
func (s *Store) ConsumeToken(token string) (*Device, error) {
	ctx := context.Background()
	now := s.nowSec()
	h := hashToken(token)
	res, err := s.db.ExecContext(ctx,
		"UPDATE tokens SET uses = uses + 1"+
			" WHERE token_hash=? AND uses < max_uses AND expires > ?"+
			" AND device_id IN (SELECT id FROM devices WHERE revoked=0 AND expires_at > ?)",
		h, now, now)
	if err != nil {
		return nil, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		"SELECT d.id, d.name, d.owner, d.client, d.expires_at, d.revoked FROM devices d"+
			" JOIN tokens t ON t.device_id=d.id WHERE t.token_hash=?", h)
	if err != nil {
		return nil, err
	}
	devs, err := scanDevices(ctx, s.db, rows)
	if err != nil || len(devs) == 0 {
		return nil, err
	}
	return &devs[0], nil
}

// VerifySecret returns the active device a subscription secret belongs to, or nil.
func (s *Store) VerifySecret(secret string) (*Device, error) {
	idStr, mac, ok := strings.Cut(secret, ".")
	if !ok || idStr == "" || len(idStr) > 12 {
		return nil, nil
	}
	for i := 0; i < len(idStr); i++ {
		if idStr[i] < '0' || idStr[i] > '9' {
			return nil, nil
		}
	}
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return nil, nil
	}
	ctx := context.Background()
	var epoch int64
	err = s.db.QueryRowContext(ctx,
		"SELECT secret_epoch FROM devices WHERE id=? AND revoked=0 AND expires_at > ?", id, s.nowSec()).Scan(&epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(mac), []byte(s.mac(id, epoch))) {
		return nil, nil
	}
	d, err := s.GetDevice(id)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Revoke marks a device revoked.
func (s *Store) Revoke(deviceID int64) error {
	_, err := s.db.Exec("UPDATE devices SET revoked=1 WHERE id=?", deviceID)
	return err
}

// Extend sets a device's expiry (unix seconds).
func (s *Store) Extend(deviceID int64, expiresAt int64) error {
	_, err := s.db.Exec("UPDATE devices SET expires_at=? WHERE id=?", expiresAt, deviceID)
	return err
}

// ListDevices returns all devices ordered by id.
func (s *Store) ListDevices() ([]Device, error) {
	ctx := context.Background()
	rows, err := s.db.QueryContext(ctx, "SELECT "+deviceCols+" FROM devices ORDER BY id")
	if err != nil {
		return nil, err
	}
	return scanDevices(ctx, s.db, rows)
}

// ActiveCredentials returns one entry per mode of every non-revoked, unexpired device, ordered
// by device id then mode. The result is never nil, so it marshals to [] when empty. It returns
// an error rather than an empty set on failure: an empty set is a meaningful push.
func (s *Store) ActiveCredentials() ([]Credential, error) {
	rows, err := s.db.Query(
		"SELECT u.uuid, u.mode, d.expires_at FROM devices d JOIN device_uuids u ON u.device_id=d.id"+
			" WHERE d.revoked=0 AND d.expires_at > ? ORDER BY d.id, u.mode", s.nowSec())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Credential{}
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.UUID, &c.Mode, &c.NotAfter); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
