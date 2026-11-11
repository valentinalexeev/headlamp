package tailnetdns

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for a missing record.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when a record with the name exists.
var ErrExists = errors.New("already exists")

// Record is one name that node-control answers (#286). It points at exactly one of: fixed tailnet addresses, one
// headscale node (by its given name), or the online nodes with one tag. A node or tag target follows the current
// addresses of the nodes.
type Record struct {
	Name      string    `json:"name"`
	Addresses []string  `json:"addresses,omitempty"`
	Node      string    `json:"node,omitempty"`
	Tag       string    `json:"tag,omitempty"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// AuditEntry is one admin change.
type AuditEntry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

// Store keeps the records and the audit log.
type Store interface {
	Records() ([]Record, error)
	Create(r Record) error
	Replace(r Record) error
	Delete(name string) error
	Audit(at time.Time, actor, action, detail string) error
	AuditLog(limit int) ([]AuditEntry, error)
	Close() error
}

const schema = `
CREATE TABLE IF NOT EXISTS records (
  name TEXT PRIMARY KEY,
  target TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL,
  updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  detail TEXT NOT NULL
);`

type target struct {
	Addresses []string `json:"addresses,omitempty"`
	Node      string   `json:"node,omitempty"`
	Tag       string   `json:"tag,omitempty"`
}

type sqliteStore struct{ db *sql.DB }

// OpenSQLite opens (and creates) the store at path.
func OpenSQLite(path string) (Store, error) {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)")
	if err != nil {
		return nil, fmt.Errorf("open tailnet DNS db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open tailnet DNS db: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Records() ([]Record, error) {
	rows, err := s.db.Query(`SELECT name, target, note, created, updated FROM records ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		var t string
		var c, u int64
		if err := rows.Scan(&r.Name, &t, &r.Note, &c, &u); err != nil {
			return nil, err
		}
		var tg target
		if err := json.Unmarshal([]byte(t), &tg); err != nil {
			return nil, fmt.Errorf("record %s: %w", r.Name, err)
		}
		r.Addresses, r.Node, r.Tag = tg.Addresses, tg.Node, tg.Tag
		r.Created, r.Updated = time.Unix(c, 0).UTC(), time.Unix(u, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func targetJSON(r Record) string {
	b, _ := json.Marshal(target{Addresses: r.Addresses, Node: r.Node, Tag: r.Tag})
	return string(b)
}

func (s *sqliteStore) Create(r Record) error {
	res, err := s.db.Exec(`INSERT INTO records (name, target, note, created, updated) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING`, r.Name, targetJSON(r), r.Note, r.Created.Unix(), r.Updated.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

func (s *sqliteStore) Replace(r Record) error {
	res, err := s.db.Exec(`UPDATE records SET target = ?, note = ?, updated = ? WHERE name = ?`,
		targetJSON(r), r.Note, r.Updated.Unix(), r.Name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Delete(name string) error {
	res, err := s.db.Exec(`DELETE FROM records WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Audit(at time.Time, actor, action, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit (ts, actor, action, detail) VALUES (?, ?, ?, ?)`,
		at.Unix(), actor, action, detail)
	return err
}

func (s *sqliteStore) AuditLog(limit int) ([]AuditEntry, error) {
	rows, err := s.db.Query(`SELECT ts, actor, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		var ts int64
		if err := rows.Scan(&ts, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(ts, 0).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Close() error { return s.db.Close() }
