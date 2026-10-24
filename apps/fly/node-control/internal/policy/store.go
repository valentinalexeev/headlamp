// Package policy manages the headscale ACL policy: a service that applies a change with a
// confirm-or-revert deadline, and the SQLite store that keeps the pending change and the history.
//
// Policy text is stored (so a change can be reverted) but is never logged.
package policy

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Errors returned by the store and the service.
var (
	ErrConflict       = errors.New("policy changed since it was read")
	ErrTooLarge       = errors.New("policy is too large")
	ErrBusy           = errors.New("another policy change is pending")
	ErrNothingPending = errors.New("no policy change is pending")
	ErrNoChange       = errors.New("policy is unchanged")
	ErrUncertain      = errors.New("policy write outcome unknown; an automatic revert is armed")
)

// Outcome is the state of a change.
type Outcome string

const (
	OutcomePending   Outcome = "pending"
	OutcomeConfirmed Outcome = "confirmed"
	OutcomeReverted  Outcome = "reverted"
	OutcomeUnknown   Outcome = "unknown"
)

// Pending is the change waiting for confirmation.
type Pending struct {
	ID       int64
	Actor    string
	Reason   string
	Before   string
	Deadline time.Time
	Attempts int // failed revert attempts so far
}

// Entry is one history row. It carries no policy text.
type Entry struct {
	ID       int64
	At       time.Time
	Actor    string
	Reason   string
	State    Outcome
	Deadline time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS changes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    actor TEXT NOT NULL,
    reason TEXT NOT NULL,
    before TEXT NOT NULL,
    after TEXT NOT NULL,
    deadline INTEGER NOT NULL,
    state TEXT NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS one_pending ON changes(state) WHERE state = 'pending';
`

// Store is the SQLite change history. It uses one connection and no WAL to keep memory low.
type Store struct {
	db    *sql.DB
	clock func() time.Time
}

// OpenStore opens (creating when needed) the database at path. A file that is not a database is
// an error; it is never deleted or recreated. A nil clock means time.Now.
func OpenStore(path string, clock func() time.Time) (*Store, error) {
	if clock == nil {
		clock = time.Now
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(30000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open policy db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open policy db: %w", err)
	}
	return &Store{db: db, clock: clock}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Begin records a new pending change. It returns ErrBusy when one is already pending.
func (s *Store) Begin(actor, reason, before, after string, deadline time.Time) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var n int
	if err := tx.QueryRow("SELECT COUNT(*) FROM changes WHERE state = 'pending'").Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return 0, ErrBusy
	}
	res, err := tx.Exec(`INSERT INTO changes (ts, actor, reason, before, after, deadline, state)
		VALUES (?, ?, ?, ?, ?, ?, 'pending')`,
		s.clock().Unix(), actor, reason, before, after, deadline.Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrBusy
		}
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Abort deletes a just-begun change whose application failed, so nothing stays pending.
func (s *Store) Abort(id int64) error {
	_, err := s.db.Exec("DELETE FROM changes WHERE id = ? AND state = 'pending'", id)
	return err
}

// Pending returns the pending change, if any.
func (s *Store) Pending() (Pending, bool, error) {
	var p Pending
	var dl int64
	err := s.db.QueryRow(`SELECT id, actor, reason, before, deadline, attempts
		FROM changes WHERE state = 'pending'`).Scan(&p.ID, &p.Actor, &p.Reason, &p.Before, &dl, &p.Attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return Pending{}, false, nil
	}
	if err != nil {
		return Pending{}, false, err
	}
	p.Deadline = time.Unix(dl, 0)
	return p, true, nil
}

func (s *Store) settle(id int64, state Outcome) error {
	res, err := s.db.Exec("UPDATE changes SET state = ? WHERE id = ? AND state = 'pending'", string(state), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNothingPending
	}
	return nil
}

// Confirm marks a pending change confirmed.
func (s *Store) Confirm(id int64) error { return s.settle(id, OutcomeConfirmed) }

// MarkReverted records a revert attempt: on success the change becomes reverted; otherwise it
// stays pending and its attempt counter grows (the watcher retries).
func (s *Store) MarkReverted(id int64, ok bool) error {
	if ok {
		return s.settle(id, OutcomeReverted)
	}
	res, err := s.db.Exec("UPDATE changes SET attempts = attempts + 1 WHERE id = ? AND state = 'pending'", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNothingPending
	}
	return nil
}

// Outcome returns the state of a change; OutcomeUnknown for a missing id or on a read error.
func (s *Store) Outcome(id int64) Outcome {
	var st string
	if err := s.db.QueryRow("SELECT state FROM changes WHERE id = ?", id).Scan(&st); err != nil {
		return OutcomeUnknown
	}
	return Outcome(st)
}

// History returns up to limit changes, newest first, without policy text.
func (s *Store) History(limit int) ([]Entry, error) {
	rows, err := s.db.Query(`SELECT id, ts, actor, reason, state, deadline
		FROM changes ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var ts, dl int64
		var st string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Reason, &st, &dl); err != nil {
			return nil, err
		}
		e.At, e.Deadline, e.State = time.Unix(ts, 0), time.Unix(dl, 0), Outcome(st)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TextOf returns the policy text stored before or after a change.
func (s *Store) TextOf(id int64, which string) (string, error) {
	if which != "before" && which != "after" {
		return "", fmt.Errorf("unknown policy text %q", which)
	}
	var text string
	if err := s.db.QueryRow("SELECT "+which+" FROM changes WHERE id = ?", id).Scan(&text); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("change %d not found", id)
		}
		return "", err
	}
	return text, nil
}
