package serve

import (
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for a missing namespace or name.
var ErrNotFound = errors.New("not found")

// ErrTaken is returned when another node holds the name.
var ErrTaken = errors.New("name held by another node")

// Namespace is one serve namespace: the suffixes its nodes may claim names in.
type Namespace struct {
	Name     string   `json:"name"`
	Suffixes []string `json:"suffixes"`
	Note     string   `json:"note,omitempty"`
}

// Registration is one published name. NodeID is the stable headscale node id: a name stays with the node that
// claimed it, so another node of the namespace cannot take it over.
type Registration struct {
	Name       string     `json:"name"`
	Namespace  string     `json:"namespace"`
	NodeID     string     `json:"node_id"`
	NodeName   string     `json:"node_name"`
	IP         netip.Addr `json:"ip"`
	CertExpiry time.Time  `json:"cert_expiry,omitzero"`
	Updated    time.Time  `json:"updated"`
}

// Store keeps namespaces, registrations and the CA material. Every change to what headscale must serve bumps
// Version, so the records-sync agent can tell an unchanged list from a new one.
type Store interface {
	PutNamespace(ns Namespace) error
	DeleteNamespace(name string) error
	Namespaces() ([]Namespace, error)

	// Register claims name for a node, or refreshes the node's own claim. A name another node holds is ErrTaken.
	Register(r Registration) error
	SetCertExpiry(name string, exp time.Time) error
	Unregister(name, nodeID string) error
	// DropName removes a name whatever node holds it (admin action).
	DropName(name string) error
	Registrations() ([]Registration, error)

	Version() (int64, error)

	// GetSecret and PutSecret keep opaque, already-sealed blobs (the CA key).
	GetSecret(key string) ([]byte, error)
	PutSecret(key string, v []byte) error

	Close() error
}

const schema = `
CREATE TABLE IF NOT EXISTS namespaces (
  name TEXT PRIMARY KEY,
  suffixes TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS registrations (
  name TEXT PRIMARY KEY,
  namespace TEXT NOT NULL REFERENCES namespaces(name) ON DELETE CASCADE,
  node_id TEXT NOT NULL,
  node_name TEXT NOT NULL,
  ip TEXT NOT NULL,
  cert_expiry INTEGER NOT NULL DEFAULT 0,
  updated INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (k TEXT PRIMARY KEY, v BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v INTEGER NOT NULL);
INSERT OR IGNORE INTO meta (k, v) VALUES ('version', 0);
PRAGMA foreign_keys = ON;
`

type sqliteStore struct {
	db    *sql.DB
	clock func() time.Time
}

// OpenSQLite opens (creating when needed) the serve database at path. One connection, no WAL, as in funnel.
func OpenSQLite(path string, clock func() time.Time) (Store, error) {
	if clock == nil {
		clock = time.Now
	}
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open serve db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open serve db: %w", err)
	}
	return &sqliteStore{db: db, clock: clock}, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func (s *sqliteStore) inTx(f func(tx *sql.Tx) error) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func bump(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE meta SET v = v + 1 WHERE k = 'version'`)
	return err
}

func (s *sqliteStore) PutNamespace(ns Namespace) error {
	if !validLabel(ns.Name) {
		return refuse("invalid namespace name %q", ns.Name)
	}
	suf := slices.Clone(ns.Suffixes)
	slices.Sort(suf)
	suf = slices.Compact(suf)
	return s.inTx(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO namespaces (name, suffixes, note) VALUES (?, ?, ?)
			ON CONFLICT(name) DO UPDATE SET suffixes = excluded.suffixes, note = excluded.note`,
			ns.Name, strings.Join(suf, ","), ns.Note); err != nil {
			return err
		}
		// A name outside the namespace's new suffixes is no longer allowed: drop it now, not at the next renewal.
		rows, err := tx.Query(`SELECT name FROM registrations WHERE namespace = ?`, ns.Name)
		if err != nil {
			return err
		}
		var drop []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			if !underAny(n, suf) {
				drop = append(drop, n)
			}
		}
		rows.Close()
		for _, n := range drop {
			if _, err := tx.Exec(`DELETE FROM registrations WHERE name = ?`, n); err != nil {
				return err
			}
		}
		return bump(tx)
	})
}

// underAny reports whether full name n is strictly below one of the suffixes: it ends in "." + suffix, on a label
// boundary, and is not the suffix itself.
func underAny(n string, suffixes []string) bool {
	for _, s := range suffixes {
		if strings.HasSuffix(n, "."+s) {
			return true
		}
	}
	return false
}

func (s *sqliteStore) DeleteNamespace(name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM namespaces WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		if _, err := tx.Exec(`DELETE FROM registrations WHERE namespace = ?`, name); err != nil {
			return err
		}
		return bump(tx)
	})
}

func (s *sqliteStore) Namespaces() ([]Namespace, error) {
	rows, err := s.db.Query(`SELECT name, suffixes, note FROM namespaces ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Namespace
	for rows.Next() {
		var ns Namespace
		var suf string
		if err := rows.Scan(&ns.Name, &suf, &ns.Note); err != nil {
			return nil, err
		}
		if suf != "" {
			ns.Suffixes = strings.Split(suf, ",")
		}
		out = append(out, ns)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Register(r Registration) error {
	if r.NodeID == "" || !r.IP.IsValid() {
		return errors.New("serve: registration needs a node id and an address")
	}
	now := s.clock().Unix()
	return s.inTx(func(tx *sql.Tx) error {
		var holder, ip string
		switch err := tx.QueryRow(`SELECT node_id, ip FROM registrations WHERE name = ?`, r.Name).Scan(&holder, &ip); {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO registrations (name, namespace, node_id, node_name, ip, updated) VALUES (?, ?, ?, ?, ?, ?)`,
				r.Name, r.Namespace, r.NodeID, r.NodeName, r.IP.String(), now); err != nil {
				return err
			}
			return bump(tx)
		case err != nil:
			return err
		case holder != r.NodeID:
			return ErrTaken
		}
		if _, err := tx.Exec(`UPDATE registrations SET node_name = ?, ip = ?, updated = ? WHERE name = ?`,
			r.NodeName, r.IP.String(), now, r.Name); err != nil {
			return err
		}
		if ip != r.IP.String() {
			return bump(tx) // only a new address changes what headscale serves
		}
		return nil
	})
}

func (s *sqliteStore) SetCertExpiry(name string, exp time.Time) error {
	res, err := s.db.Exec(`UPDATE registrations SET cert_expiry = ? WHERE name = ?`, exp.Unix(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Unregister(name, nodeID string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM registrations WHERE name = ? AND node_id = ?`, name, nodeID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return bump(tx)
	})
}

func (s *sqliteStore) DropName(name string) error {
	return s.inTx(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM registrations WHERE name = ?`, name)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return bump(tx)
	})
}

func (s *sqliteStore) Registrations() ([]Registration, error) {
	rows, err := s.db.Query(`SELECT name, namespace, node_id, node_name, ip, cert_expiry, updated FROM registrations ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Registration
	for rows.Next() {
		var r Registration
		var ip string
		var exp, upd int64
		if err := rows.Scan(&r.Name, &r.Namespace, &r.NodeID, &r.NodeName, &ip, &exp, &upd); err != nil {
			return nil, err
		}
		r.IP, _ = netip.ParseAddr(ip)
		if exp > 0 {
			r.CertExpiry = time.Unix(exp, 0).UTC()
		}
		r.Updated = time.Unix(upd, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqliteStore) Version() (int64, error) {
	var v int64
	err := s.db.QueryRow(`SELECT v FROM meta WHERE k = 'version'`).Scan(&v)
	return v, err
}

func (s *sqliteStore) GetSecret(key string) ([]byte, error) {
	var v []byte
	err := s.db.QueryRow(`SELECT v FROM secrets WHERE k = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return v, err
}

func (s *sqliteStore) PutSecret(key string, v []byte) error {
	_, err := s.db.Exec(`INSERT INTO secrets (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, v)
	return err
}
