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

// Record is one name that node-control answers (#286, #285). Its Target has one type: an address
// record points at exactly one of fixed tailnet addresses, one headscale node (by its given name),
// or the online nodes with one tag (a node or tag target follows the current addresses of the
// nodes); a CNAME, TXT or SRV record holds its data. Rules give other data to some clients.
type Record struct {
	Name string `json:"name"`
	Target
	Rules   []Rule    `json:"rules,omitempty"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// fixType gives a record read from a store of an older version its type: a record without one is
// an address record (#286).
func fixType(r *Record) {
	if r.Type == "" {
		r.Type = TypeAddr
	}
	for i := range r.Rules {
		r.Rules[i].Type = r.Type
	}
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
	Zones() ([]Zone, error)
	CreateZone(z Zone) error
	DeleteZone(name string) error
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
CREATE TABLE IF NOT EXISTS zones (
  name TEXT PRIMARY KEY,
  note TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS audit (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts INTEGER NOT NULL,
  actor TEXT NOT NULL,
  action TEXT NOT NULL,
  detail TEXT NOT NULL
);`

// target is the JSON of the target column: the data and the rules of a record. A row of #286 has
// only addresses, node or tag.
type target struct {
	Target
	Rules []Rule `json:"rules,omitempty"`
}

type sqliteStore struct{ db *sql.DB }

// OpenSQLite opens (and creates) the store at path.
func OpenSQLite(path string) (Store, error) {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	const pragmas = "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", "file:"+esc+pragmas)
	if err != nil {
		return nil, fmt.Errorf("open tailnet DNS db: %w", err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := opCtx()
	defer cancel()
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open tailnet DNS db: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Records() ([]Record, error) {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, target, note, created, updated FROM records ORDER BY name`)
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
		r.Target, r.Rules = tg.Target, tg.Rules
		fixType(&r)
		r.Created, r.Updated = time.Unix(c, 0).UTC(), time.Unix(u, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func targetJSON(r Record) string {
	b, _ := json.Marshal(target{Target: r.Target, Rules: r.Rules})
	return string(b)
}

func (s *sqliteStore) Create(r Record) error {
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO records (name, target, note, created, updated) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(name) DO NOTHING`,
		r.Name, targetJSON(r), r.Note, r.Created.Unix(), r.Updated.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

func (s *sqliteStore) Replace(r Record) error {
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE records SET target = ?, note = ?, updated = ? WHERE name = ?`,
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
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Zones() ([]Zone, error) {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx, `SELECT name, note, created FROM zones ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Zone{}
	for rows.Next() {
		var z Zone
		var c int64
		if err := rows.Scan(&z.Name, &z.Note, &c); err != nil {
			return nil, err
		}
		z.Created = time.Unix(c, 0).UTC()
		out = append(out, z)
	}
	return out, rows.Err()
}

func (s *sqliteStore) CreateZone(z Zone) error {
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO zones (name, note, created) VALUES (?, ?, ?) ON CONFLICT(name) DO NOTHING`,
		z.Name, z.Note, z.Created.Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrExists
	}
	return nil
}

func (s *sqliteStore) DeleteZone(name string) error {
	ctx, cancel := opCtx()
	defer cancel()
	res, err := s.db.ExecContext(ctx, `DELETE FROM zones WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Audit(at time.Time, actor, action, detail string) error {
	ctx, cancel := opCtx()
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit (ts, actor, action, detail) VALUES (?, ?, ?, ?)`,
		at.Unix(), actor, action, detail)
	return err
}

func (s *sqliteStore) AuditLog(limit int) ([]AuditEntry, error) {
	ctx, cancel := opCtx()
	defer cancel()
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, actor, action, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
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
