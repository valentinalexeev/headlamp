package enroll

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SelfStore keeps the people who enrol themselves through OIDC (#102) and their browser sessions. It is a small
// SQLite file of its own (OIDC_DB). Refresh tokens are sealed with a key derived from ENROLL_SECRET_KEY; sessions
// are stored by the hash of their cookie value, so the file alone cannot log anybody in.
type SelfStore struct {
	db    *sql.DB
	aead  cipher.AEAD
	clock func() time.Time
}

// SelfUser is one person, keyed by the issuer's subject.
type SelfUser struct {
	Subject     string
	Email       string
	HSUserID    uint64 // the headscale user created for this person; 0 until the first device
	Locked      bool
	LockedBy    string
	LockReason  string
	LastLogin   int64
	LastCheck   int64
	CheckFailed int64 // when the last re-check could not reach the issuer, 0 when it did
	refresh     []byte
}

const selfSchema = `
CREATE TABLE IF NOT EXISTS oidc_users (
	subject      TEXT PRIMARY KEY,
	email        TEXT NOT NULL DEFAULT '',
	hs_user_id   INTEGER NOT NULL DEFAULT 0,
	locked       INTEGER NOT NULL DEFAULT 0,
	locked_by    TEXT NOT NULL DEFAULT '',
	lock_reason  TEXT NOT NULL DEFAULT '',
	last_login   INTEGER NOT NULL DEFAULT 0,
	last_check   INTEGER NOT NULL DEFAULT 0,
	check_failed INTEGER NOT NULL DEFAULT 0,
	refresh      BLOB
);
CREATE TABLE IF NOT EXISTS oidc_sessions (
	id_hash    TEXT PRIMARY KEY,
	subject    TEXT NOT NULL,
	csrf       TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS oidc_sessions_subject ON oidc_sessions(subject);
CREATE TABLE IF NOT EXISTS oidc_flows (
	state      TEXT PRIMARY KEY,
	nonce      TEXT NOT NULL,
	verifier   BLOB NOT NULL,
	expires_at INTEGER NOT NULL
);
`

// OpenSelfStore opens (creating when needed) the store at path. key is ENROLL_SECRET_KEY.
func OpenSelfStore(path string, key []byte, clock func() time.Time) (*SelfStore, error) {
	if clock == nil {
		clock = time.Now
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(30000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open OIDC db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), selfSchema+inviteSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open OIDC db: %w", err)
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("headlamp oidc refresh token seal v1"))
	blk, _ := aes.NewCipher(mac.Sum(nil))
	aead, _ := cipher.NewGCM(blk)
	return &SelfStore{db: db, aead: aead, clock: clock}, nil
}

// Close closes the database.
func (s *SelfStore) Close() error { return s.db.Close() }

func (s *SelfStore) now() int64 { return s.clock().Unix() }

func (s *SelfStore) seal(sub, pt string) []byte {
	if pt == "" {
		return nil
	}
	nonce := make([]byte, s.aead.NonceSize())
	_, _ = rand.Read(nonce)
	return s.aead.Seal(nonce, nonce, []byte(pt), []byte(sub))
}

func (s *SelfStore) open(sub string, ct []byte) (string, error) {
	if len(ct) == 0 {
		return "", nil
	}
	n := s.aead.NonceSize()
	if len(ct) < n {
		return "", errors.New("sealed refresh token is truncated")
	}
	pt, err := s.aead.Open(nil, ct[:n], ct[n:], []byte(sub))
	if err != nil {
		return "", errors.New("sealed refresh token does not open")
	}
	return string(pt), nil
}

// RecordLogin creates or updates the person after a verified login. A new refresh token replaces the old one; an
// empty one keeps it.
func (s *SelfStore) RecordLogin(sub, email, refresh string) error {
	_, err := s.db.Exec(`INSERT INTO oidc_users(subject, email, last_login, last_check, refresh) VALUES(?,?,?,?,?)
		ON CONFLICT(subject) DO UPDATE SET email=excluded.email, last_login=excluded.last_login,
		last_check=excluded.last_check, check_failed=0, refresh=COALESCE(excluded.refresh, oidc_users.refresh)`,
		sub, email, s.now(), s.now(), s.seal(sub, refresh))
	return err
}

const userCols = "subject, email, hs_user_id, locked, locked_by, lock_reason, last_login, last_check, check_failed, refresh"

func scanUser(sc interface{ Scan(...any) error }) (SelfUser, error) {
	var u SelfUser
	var locked int
	err := sc.Scan(&u.Subject, &u.Email, &u.HSUserID, &locked, &u.LockedBy, &u.LockReason, &u.LastLogin, &u.LastCheck, &u.CheckFailed, &u.refresh)
	u.Locked = locked != 0
	return u, err
}

// User returns one person, ErrNotFound when unknown.
func (s *SelfStore) User(sub string) (SelfUser, error) {
	u, err := scanUser(s.db.QueryRow("SELECT "+userCols+" FROM oidc_users WHERE subject=?", sub))
	if errors.Is(err, sql.ErrNoRows) {
		return SelfUser{}, ErrNotFound
	}
	return u, err
}

// Users lists everyone, by subject.
func (s *SelfStore) Users() ([]SelfUser, error) {
	rows, err := s.db.Query("SELECT " + userCols + " FROM oidc_users ORDER BY subject")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SelfUser
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// RefreshToken opens the person's sealed refresh token ("" when none).
func (s *SelfStore) RefreshToken(u SelfUser) (string, error) { return s.open(u.Subject, u.refresh) }

// SetHSUser records the headscale user of a person.
func (s *SelfStore) SetHSUser(sub string, id uint64) error {
	_, err := s.db.Exec("UPDATE oidc_users SET hs_user_id=? WHERE subject=?", id, sub)
	return err
}

// Checked records a re-check: ok clears the failure mark, otherwise it is set (the issuer was unreachable).
func (s *SelfStore) Checked(sub string, ok bool, refresh string) error {
	if ok {
		_, err := s.db.Exec("UPDATE oidc_users SET last_check=?, check_failed=0, refresh=COALESCE(?, refresh) WHERE subject=?",
			s.now(), s.seal(sub, refresh), sub)
		return err
	}
	_, err := s.db.Exec("UPDATE oidc_users SET check_failed=? WHERE subject=?", s.now(), sub)
	return err
}

// SetLocked locks or unlocks a person. Locking also ends every session of the person and drops the refresh token.
func (s *SelfStore) SetLocked(sub string, locked bool, by, reason string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE oidc_users SET locked=?, locked_by=?, lock_reason=? WHERE subject=?", b2i(locked), by, reason, sub)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if locked {
		if _, err := tx.Exec("UPDATE oidc_users SET refresh=NULL WHERE subject=?", sub); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM oidc_sessions WHERE subject=?", sub); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func hashID(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// NewSession starts a session and returns the cookie value and its CSRF token.
func (s *SelfStore) NewSession(sub string, ttl time.Duration) (id, csrf string, err error) {
	id, csrf = randomToken(32), randomToken(16)
	_, err = s.db.Exec("INSERT INTO oidc_sessions(id_hash, subject, csrf, expires_at) VALUES(?,?,?,?)",
		hashID(id), sub, csrf, s.clock().Add(ttl).Unix())
	return id, csrf, err
}

// Session returns the subject and CSRF token of a live session, and ok=false for an unknown or expired one.
func (s *SelfStore) Session(id string) (sub, csrf string, ok bool, err error) {
	if id == "" {
		return "", "", false, nil
	}
	var exp int64
	err = s.db.QueryRow("SELECT subject, csrf, expires_at FROM oidc_sessions WHERE id_hash=?", hashID(id)).Scan(&sub, &csrf, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if exp <= s.now() {
		_, _ = s.db.Exec("DELETE FROM oidc_sessions WHERE id_hash=?", hashID(id))
		return "", "", false, nil
	}
	return sub, csrf, true, nil
}

// EndSession removes a session.
func (s *SelfStore) EndSession(id string) error {
	_, err := s.db.Exec("DELETE FROM oidc_sessions WHERE id_hash=?", hashID(id))
	return err
}

// SaveFlow keeps a login flow until the callback; the verifier is sealed. TakeFlow returns it once.
func (s *SelfStore) SaveFlow(state, nonce, verifier string, ttl time.Duration) error {
	_, _ = s.db.Exec("DELETE FROM oidc_flows WHERE expires_at<=?", s.now())
	_, err := s.db.Exec("INSERT INTO oidc_flows(state, nonce, verifier, expires_at) VALUES(?,?,?,?)",
		hashID(state), nonce, s.seal(state, verifier), s.clock().Add(ttl).Unix())
	return err
}

// TakeFlow returns and deletes the flow of state; ok=false when it is unknown, used or expired.
func (s *SelfStore) TakeFlow(state string) (nonce, verifier string, ok bool, err error) {
	if state == "" {
		return "", "", false, nil
	}
	var ct []byte
	var exp int64
	err = s.db.QueryRow("DELETE FROM oidc_flows WHERE state=? RETURNING nonce, verifier, expires_at", hashID(state)).Scan(&nonce, &ct, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	if exp <= s.now() {
		return "", "", false, nil
	}
	verifier, err = s.open(state, ct)
	return nonce, verifier, err == nil, err
}
