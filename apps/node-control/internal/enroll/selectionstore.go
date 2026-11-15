package enroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
)

// The bridge selection overrides (#314) are one small JSON document in an additive table (SQLite)
// or hash field (Redis). Like the grant document, it is not part of the enrolment export.

const selectionSchema = `
CREATE TABLE IF NOT EXISTS selection_doc (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    doc TEXT NOT NULL
);
`

const maxSelectionDocBytes = 64 << 10

func encodeSelectionDoc(d SelectionDoc) ([]byte, error) {
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(b) > maxSelectionDocBytes {
		return nil, errors.New("the bridge selection document is too large")
	}
	return b, nil
}

func decodeSelectionDoc(b []byte) (SelectionDoc, error) {
	var d SelectionDoc
	if len(b) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return SelectionDoc{}, errors.New("corrupt bridge selection document")
	}
	return d, nil
}

// SelectionDoc reads the overrides; an empty store gives an empty document.
func (s *Store) SelectionDoc() (SelectionDoc, error) {
	var raw string
	err := s.db.QueryRowContext(context.Background(),
		"SELECT doc FROM selection_doc WHERE id = 1").Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SelectionDoc{}, nil
	}
	if err != nil {
		return SelectionDoc{}, err
	}
	return decodeSelectionDoc([]byte(raw))
}

// PutSelectionDoc replaces the overrides.
func (s *Store) PutSelectionDoc(d SelectionDoc) error {
	b, err := encodeSelectionDoc(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(context.Background(),
		"INSERT INTO selection_doc (id, doc) VALUES (1, ?) "+
			"ON CONFLICT(id) DO UPDATE SET doc=excluded.doc", string(b))
	return err
}

func (s *RedisStore) selectionKey() string { return s.prefix + ":selection" }

// SelectionDoc reads the overrides from the hash field "doc" (sealed like the grant document).
func (s *RedisStore) SelectionDoc() (SelectionDoc, error) {
	ctx, cancel := opCtx()
	defer cancel()
	raw, err := s.rdb.HGet(ctx, s.selectionKey(), "doc").Result()
	if errors.Is(err, redis.Nil) {
		return SelectionDoc{}, nil
	}
	if err != nil {
		return SelectionDoc{}, s.wrap("reading bridge selection from redis", err)
	}
	pt, err := s.cipher.Open(s.prefix, "selection", []byte(raw))
	if err != nil {
		return SelectionDoc{}, err
	}
	return decodeSelectionDoc(pt)
}

// PutSelectionDoc replaces the overrides with one HSET.
func (s *RedisStore) PutSelectionDoc(d SelectionDoc) error {
	b, err := encodeSelectionDoc(d)
	if err != nil {
		return err
	}
	sealed, err := s.cipher.Seal(s.prefix, "selection", b)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if err := s.rdb.HSet(ctx, s.selectionKey(), "doc", sealed).Err(); err != nil {
		return s.wrap("writing bridge selection to redis", err)
	}
	return nil
}
