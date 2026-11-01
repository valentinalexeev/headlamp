package enroll

import (
	"encoding/json"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/redis/go-redis/v9"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

// The audit log records device and node actions in the enrolment backend, newest first, capped at auditKeep entries.
// It is not part of the enrolment export or backup (Snapshot), so those formats do not change.
const (
	auditKeep      = 1000
	auditMaxShort  = 100 // actor, action
	auditMaxDetail = 500
)

const auditSchema = `
CREATE TABLE IF NOT EXISTS audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    ts INTEGER NOT NULL,
    actor TEXT NOT NULL,
    action TEXT NOT NULL,
    detail TEXT NOT NULL
);
`

// cleanAudit replaces control characters and truncates to the field limit, so a long or odd device name can never
// make the record (or the action it describes) fail.
func cleanAudit(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max])
	}
	return s
}

func (s *Store) Audit(actor, action, detail string) error {
	actor, action, detail = cleanAudit(actor, auditMaxShort), cleanAudit(action, auditMaxShort), cleanAudit(detail, auditMaxDetail)
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("INSERT INTO audit (ts, actor, action, detail) VALUES (?, ?, ?, ?)", s.clock().Unix(), actor, action, detail)
	if err != nil {
		return err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec("DELETE FROM audit WHERE id <= ?", id-auditKeep); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AuditLog(limit int) ([]core.AuditEntry, error) {
	limit = min(max(limit, 1), auditKeep)
	rows, err := s.db.Query("SELECT ts, actor, action, detail FROM audit ORDER BY id DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.AuditEntry
	for rows.Next() {
		var e core.AuditEntry
		var ts int64
		if err := rows.Scan(&ts, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		e.At = time.Unix(ts, 0)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *RedisStore) auditKey() string { return s.prefix + ":audit" }

type redisAudit struct {
	T      int64  `json:"t"`
	Actor  string `json:"a"`
	Action string `json:"x"`
	Detail string `json:"d"`
}

func (s *RedisStore) Audit(actor, action, detail string) error {
	b, err := json.Marshal(redisAudit{s.clock().Unix(), cleanAudit(actor, auditMaxShort), cleanAudit(action, auditMaxShort), cleanAudit(detail, auditMaxDetail)})
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	_, err = s.rdb.TxPipelined(ctx, func(p redis.Pipeliner) error {
		p.LPush(ctx, s.auditKey(), b)
		p.LTrim(ctx, s.auditKey(), 0, auditKeep-1)
		return nil
	})
	if err != nil {
		return s.wrap("recording audit entry in redis", err)
	}
	return nil
}

func (s *RedisStore) AuditLog(limit int) ([]core.AuditEntry, error) {
	limit = min(max(limit, 1), auditKeep)
	ctx, cancel := opCtx()
	defer cancel()
	raw, err := s.rdb.LRange(ctx, s.auditKey(), 0, int64(limit-1)).Result()
	if err != nil {
		return nil, s.wrap("reading audit log from redis", err)
	}
	out := make([]core.AuditEntry, 0, len(raw))
	for _, r := range raw {
		var a redisAudit
		if json.Unmarshal([]byte(r), &a) != nil {
			continue
		}
		out = append(out, core.AuditEntry{At: time.Unix(a.T, 0), Actor: a.Actor, Action: a.Action, Detail: a.Detail})
	}
	return out, nil
}
