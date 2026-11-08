package enroll

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/redis/go-redis/v9"
)

// The grant state (#139) is one small JSON document: console targets, grants and mode overrides.
// Like the audit log, it is not part of the enrolment export: a restored store starts without
// grants (deny by default).

const tailnetSchema = `
CREATE TABLE IF NOT EXISTS tailnet_doc (
    id INTEGER PRIMARY KEY CHECK (id = 1),
    doc TEXT NOT NULL
);
`

// maxTailnetDocBytes limits the stored document.
const maxTailnetDocBytes = 256 << 10

func encodeTailnetDoc(d TailnetDoc) ([]byte, error) {
	if len(d.Grants) > maxGrants || len(d.Targets) > maxTargets {
		return nil, errors.New("too many tailnet grants or targets")
	}
	b, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(b) > maxTailnetDocBytes {
		return nil, errors.New("the tailnet grant document is too large")
	}
	return b, nil
}

func decodeTailnetDoc(b []byte) (TailnetDoc, error) {
	var d TailnetDoc
	if len(b) == 0 {
		return d, nil
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return TailnetDoc{}, errors.New("corrupt tailnet grant document")
	}
	return d, nil
}

// TailnetDoc reads the grant state; an empty store gives an empty document.
func (s *Store) TailnetDoc() (TailnetDoc, error) {
	var raw string
	const q = "SELECT doc FROM tailnet_doc WHERE id = 1"
	err := s.db.QueryRowContext(context.Background(), q).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return TailnetDoc{}, nil
	}
	if err != nil {
		return TailnetDoc{}, err
	}
	return decodeTailnetDoc([]byte(raw))
}

// PutTailnetDoc replaces the grant state.
func (s *Store) PutTailnetDoc(d TailnetDoc) error {
	b, err := encodeTailnetDoc(d)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(context.Background(),
		"INSERT INTO tailnet_doc (id, doc) VALUES (1, ?) "+
			"ON CONFLICT(id) DO UPDATE SET doc=excluded.doc",
		string(b),
	)
	return err
}

func (s *RedisStore) tailnetKey() string { return s.prefix + ":tailnet" }

// TailnetDoc reads the grant state from the hash field "doc" (sealed like the approvals).
func (s *RedisStore) TailnetDoc() (TailnetDoc, error) {
	ctx, cancel := opCtx()
	defer cancel()
	raw, err := s.rdb.HGet(ctx, s.tailnetKey(), "doc").Result()
	if errors.Is(err, redis.Nil) {
		return TailnetDoc{}, nil
	}
	if err != nil {
		return TailnetDoc{}, s.wrap("reading tailnet grants from redis", err)
	}
	pt, err := s.cipher.Open(s.prefix, "tailnet", []byte(raw))
	if err != nil {
		return TailnetDoc{}, err
	}
	return decodeTailnetDoc(pt)
}

// PutTailnetDoc replaces the grant state with one HSET.
func (s *RedisStore) PutTailnetDoc(d TailnetDoc) error {
	b, err := encodeTailnetDoc(d)
	if err != nil {
		return err
	}
	sealed, err := s.cipher.Seal(s.prefix, "tailnet", b)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if err := s.rdb.HSet(ctx, s.tailnetKey(), "doc", sealed).Err(); err != nil {
		return s.wrap("writing tailnet grants to redis", err)
	}
	return nil
}
