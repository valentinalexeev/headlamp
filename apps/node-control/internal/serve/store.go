package serve

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned for a missing service.
var ErrNotFound = errors.New("not found")

// ErrExists is returned when a service or name already exists.
var ErrExists = errors.New("already exists")

// Service is one published name (#117). Its proxies join the tailnet with the service's reusable ephemeral
// pre-auth key; node-control knows a node belongs to the service by the key it registered with. The name is exact
// ("grafana.svc.example.com") or a wildcard ("*.apps.svc.example.com", for an ingress controller behind one proxy).
type Service struct {
	ID        string    `json:"id"` // short, URL-safe; also part of the node hostname hint
	Name      string    `json:"name"`
	Namespace string    `json:"namespace"`
	Note      string    `json:"note,omitempty"`
	Created   time.Time `json:"created"`
	// Keys holds the headscale pre-auth key ids of the service, newest first. During a rotation there are two:
	// nodes on either key belong to the service until the old key is retired.
	Keys []Key `json:"keys"`
}

// Key is one pre-auth key of a service. The secret is never stored: headscale shows it once, at creation.
type Key struct {
	ID      uint64    `json:"id"`
	Expires time.Time `json:"expires"`
	Created time.Time `json:"created"`
}

// Store keeps the services and the sealed CA material.
type Store interface {
	CreateService(s Service) error
	DeleteService(id string) error
	Services() ([]Service, error)
	Service(id string) (Service, error)
	// AddKey puts a new key in front of the service's keys. RetireKey removes one.
	AddKey(serviceID string, k Key) error
	RetireKey(serviceID string, keyID uint64) error

	// GetSecret and PutSecret keep opaque, already-sealed blobs (the CA key).
	GetSecret(key string) ([]byte, error)
	PutSecret(key string, v []byte) error

	Close() error
}

const schema = `
CREATE TABLE IF NOT EXISTS services (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL UNIQUE,
  namespace TEXT NOT NULL,
  note TEXT NOT NULL DEFAULT '',
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS service_keys (
  key_id INTEGER PRIMARY KEY,
  service_id TEXT NOT NULL REFERENCES services(id) ON DELETE CASCADE,
  expires INTEGER NOT NULL,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS secrets (k TEXT PRIMARY KEY, v BLOB NOT NULL);
PRAGMA foreign_keys = ON;
`

type sqliteStore struct{ db *sql.DB }

// OpenSQLite opens (creating when needed) the serve database at path. One connection, no WAL, as in funnel.
func OpenSQLite(path string) (Store, error) {
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
	return &sqliteStore{db: db}, nil
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

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func (s *sqliteStore) CreateService(sv Service) error {
	return s.inTx(func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT INTO services (id, name, namespace, note, created) VALUES (?, ?, ?, ?, ?)`,
			sv.ID, sv.Name, sv.Namespace, sv.Note, sv.Created.Unix())
		if isUnique(err) {
			return ErrExists
		}
		if err != nil {
			return err
		}
		for _, k := range sv.Keys {
			if _, err := tx.Exec(`INSERT INTO service_keys (key_id, service_id, expires, created) VALUES (?, ?, ?, ?)`,
				k.ID, sv.ID, k.Expires.Unix(), k.Created.Unix()); err != nil {
				if isUnique(err) {
					return ErrExists
				}
				return err
			}
		}
		return nil
	})
}

func (s *sqliteStore) DeleteService(id string) error {
	res, err := s.db.Exec(`DELETE FROM services WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *sqliteStore) Services() ([]Service, error) {
	rows, err := s.db.Query(`SELECT id, name, namespace, note, created FROM services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var out []Service
	for rows.Next() {
		var sv Service
		var created int64
		if err := rows.Scan(&sv.ID, &sv.Name, &sv.Namespace, &sv.Note, &created); err != nil {
			rows.Close()
			return nil, err
		}
		sv.Created = time.Unix(created, 0).UTC()
		out = append(out, sv)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Keys, err = s.keys(out[i].ID); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *sqliteStore) Service(id string) (Service, error) {
	var sv Service
	var created int64
	err := s.db.QueryRow(`SELECT id, name, namespace, note, created FROM services WHERE id = ?`, id).
		Scan(&sv.ID, &sv.Name, &sv.Namespace, &sv.Note, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Service{}, ErrNotFound
	}
	if err != nil {
		return Service{}, err
	}
	sv.Created = time.Unix(created, 0).UTC()
	sv.Keys, err = s.keys(id)
	return sv, err
}

func (s *sqliteStore) keys(id string) ([]Key, error) {
	rows, err := s.db.Query(`SELECT key_id, expires, created FROM service_keys WHERE service_id = ? ORDER BY created DESC, key_id DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		var k Key
		var exp, created int64
		if err := rows.Scan(&k.ID, &exp, &created); err != nil {
			return nil, err
		}
		k.Expires, k.Created = time.Unix(exp, 0).UTC(), time.Unix(created, 0).UTC()
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) AddKey(serviceID string, k Key) error {
	_, err := s.db.Exec(`INSERT INTO service_keys (key_id, service_id, expires, created) VALUES (?, ?, ?, ?)`,
		k.ID, serviceID, k.Expires.Unix(), k.Created.Unix())
	switch {
	case isUnique(err):
		return ErrExists
	case err != nil && strings.Contains(err.Error(), "FOREIGN KEY"):
		return ErrNotFound
	}
	return err
}

func (s *sqliteStore) RetireKey(serviceID string, keyID uint64) error {
	res, err := s.db.Exec(`DELETE FROM service_keys WHERE service_id = ? AND key_id = ?`, serviceID, keyID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
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
