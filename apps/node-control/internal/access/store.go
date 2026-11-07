// Package access is the roles and grants page of the console (#123). It keeps the stored layer of the admin policy
// (authz.Policy.SetDynamic) in SQLite or Redis, with an audit log, and lets an admin change it within their own rights.
package access

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	_ "modernc.org/sqlite"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
)

// ErrConflict is returned by Save when the stored version is not the one the change was based on.
var ErrConflict = errors.New("the roles and grants changed meanwhile: reload and try again")

// auditKeep is how many audit entries a store keeps, newest first.
const auditKeep = 1000

// Entry is one audit record of a change to roles or grants.
type Entry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Action string    `json:"action"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail,omitempty"`
}

// Store keeps the stored layer of roles and grants, with a version that every Save bumps.
type Store interface {
	// Load returns the stored config and its version (0 for an empty store).
	Load() (authz.Config, int64, error)
	// Version returns the current version, cheaply: instances poll it to pick up changes.
	Version() (int64, error)
	// Save replaces the config if the version is still prev, and records e in the same step.
	Save(cfg authz.Config, prev int64, e Entry) (int64, error)
	// Audit returns at most limit entries, newest first.
	Audit(limit int) ([]Entry, error)
	Close() error
}

func decode(b []byte) (authz.Config, error) {
	var c authz.Config
	if len(b) == 0 {
		return authz.Config{Grants: []authz.Grant{}}, nil
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("admin store: %w", err)
	}
	if c.Grants == nil {
		c.Grants = []authz.Grant{}
	}
	return c, nil
}

// --- SQLite

const schema = `
CREATE TABLE IF NOT EXISTS admin_config (id INTEGER PRIMARY KEY CHECK (id = 1), version INTEGER NOT NULL, body TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS admin_audit (id INTEGER PRIMARY KEY AUTOINCREMENT, body TEXT NOT NULL);
`

type sqliteStore struct{ db *sql.DB }

// OpenSQLite opens (and creates) the admin store file.
func OpenSQLite(path string) (Store, error) {
	esc := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23").Replace(path)
	db, err := sql.Open("sqlite", "file:"+esc+"?_pragma=busy_timeout(30000)&_txlock=immediate")
	if err != nil {
		return nil, fmt.Errorf("open admin db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("open admin db: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

func (s *sqliteStore) Close() error { return s.db.Close() }

func (s *sqliteStore) Load() (authz.Config, int64, error) {
	var v int64
	var body string
	err := s.db.QueryRow("SELECT version, body FROM admin_config WHERE id = 1").Scan(&v, &body)
	if errors.Is(err, sql.ErrNoRows) {
		c, _ := decode(nil)
		return c, 0, nil
	}
	if err != nil {
		return authz.Config{}, 0, err
	}
	c, err := decode([]byte(body))
	return c, v, err
}

func (s *sqliteStore) Version() (int64, error) {
	var v int64
	err := s.db.QueryRow("SELECT version FROM admin_config WHERE id = 1").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return v, err
}

func (s *sqliteStore) Save(cfg authz.Config, prev int64, e Entry) (int64, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	eb, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var cur int64
	if err := tx.QueryRow("SELECT version FROM admin_config WHERE id = 1").Scan(&cur); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if cur != prev {
		return 0, ErrConflict
	}
	if _, err := tx.Exec("INSERT INTO admin_config (id, version, body) VALUES (1, ?, ?) ON CONFLICT(id) DO UPDATE SET version = excluded.version, body = excluded.body", prev+1, string(body)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("INSERT INTO admin_audit (body) VALUES (?)", string(eb)); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("DELETE FROM admin_audit WHERE id <= (SELECT MAX(id) FROM admin_audit) - ?", auditKeep); err != nil {
		return 0, err
	}
	return prev + 1, tx.Commit()
}

func (s *sqliteStore) Audit(limit int) ([]Entry, error) {
	rows, err := s.db.Query("SELECT body FROM admin_audit ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var e Entry
		if json.Unmarshal([]byte(b), &e) == nil {
			out = append(out, e)
		}
	}
	return out, rows.Err()
}

// --- Redis

type redisStore struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisStore keeps the admin store under prefix ("<prefix>:config" hash, "<prefix>:audit" list). Roles and grants
// are not secret, so they are stored in clear.
func NewRedisStore(rdb *redis.Client, prefix string) (Store, error) {
	if prefix == "" || strings.ContainsAny(prefix, "*?[\\") {
		return nil, fmt.Errorf("admin store: invalid Redis key prefix %q", prefix)
	}
	return &redisStore{rdb: rdb, prefix: prefix}, nil
}

func (r *redisStore) cfgKey() string   { return r.prefix + ":config" }
func (r *redisStore) auditKey() string { return r.prefix + ":audit" }

func (r *redisStore) Close() error { return r.rdb.Close() }

func ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (r *redisStore) Load() (authz.Config, int64, error) {
	c, cancel := ctx()
	defer cancel()
	m, err := r.rdb.HGetAll(c, r.cfgKey()).Result()
	if err != nil {
		return authz.Config{}, 0, fmt.Errorf("admin store: %w", err)
	}
	v, _ := strconv.ParseInt(m["version"], 10, 64)
	cfg, err := decode([]byte(m["body"]))
	return cfg, v, err
}

func (r *redisStore) Version() (int64, error) {
	c, cancel := ctx()
	defer cancel()
	s, err := r.rdb.HGet(c, r.cfgKey(), "version").Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("admin store: %w", err)
	}
	return strconv.ParseInt(s, 10, 64)
}

func (r *redisStore) Save(cfg authz.Config, prev int64, e Entry) (int64, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return 0, err
	}
	eb, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	c, cancel := ctx()
	defer cancel()
	err = r.rdb.Watch(c, func(tx *redis.Tx) error {
		s, err := tx.HGet(c, r.cfgKey(), "version").Result()
		if err != nil && !errors.Is(err, redis.Nil) {
			return err
		}
		cur, _ := strconv.ParseInt(s, 10, 64)
		if cur != prev {
			return ErrConflict
		}
		_, err = tx.TxPipelined(c, func(p redis.Pipeliner) error {
			p.HSet(c, r.cfgKey(), "version", prev+1, "body", string(body))
			p.LPush(c, r.auditKey(), string(eb))
			p.LTrim(c, r.auditKey(), 0, auditKeep-1)
			return nil
		})
		return err
	}, r.cfgKey())
	if errors.Is(err, redis.TxFailedErr) {
		return 0, ErrConflict
	}
	if err != nil {
		return 0, err
	}
	return prev + 1, nil
}

func (r *redisStore) Audit(limit int) ([]Entry, error) {
	c, cancel := ctx()
	defer cancel()
	items, err := r.rdb.LRange(c, r.auditKey(), 0, int64(limit)-1).Result()
	if err != nil {
		return nil, fmt.Errorf("admin store: %w", err)
	}
	out := make([]Entry, 0, len(items))
	for _, b := range items {
		var e Entry
		if json.Unmarshal([]byte(b), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}
