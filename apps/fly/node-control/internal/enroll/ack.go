package enroll

import (
	"encoding/json"
	"log/slog"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

// Acknowledgements of attention items live next to the audit log and, like it, are not part of the enrolment export
// or backup (Snapshot): a restored store starts without them, so every item is simply back in the queue. They hold a
// login, an item id and a hash of what the item meant, never item text. The core decides when one ends, and it writes
// and gates their audit entries, which is why those actions are not in activityPrefixes.

const ackSchema = `
CREATE TABLE IF NOT EXISTS attention_acks (
    module TEXT NOT NULL,
    item_id TEXT NOT NULL,
    meaning TEXT NOT NULL,
    actor TEXT NOT NULL,
    at INTEGER NOT NULL,
    until INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (module, item_id)
);
`

func untilOf(sec int64) time.Time {
	if sec == 0 {
		return time.Time{}
	}
	return time.Unix(sec, 0)
}

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func (s *Store) Acknowledgements() ([]core.Acknowledgement, error) {
	rows, err := s.db.Query("SELECT module, item_id, meaning, actor, at, until FROM attention_acks")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []core.Acknowledgement
	for rows.Next() {
		var a core.Acknowledgement
		var at, until int64
		if err := rows.Scan(&a.Module, &a.ID, &a.Meaning, &a.By, &at, &until); err != nil {
			return nil, err
		}
		a.At, a.Until = time.Unix(at, 0), untilOf(until)
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) PutAcknowledgement(a core.Acknowledgement) error {
	_, err := s.db.Exec("INSERT OR REPLACE INTO attention_acks (module, item_id, meaning, actor, at, until) VALUES (?, ?, ?, ?, ?, ?)",
		a.Module, a.ID, a.Meaning, cleanAudit(a.By, auditMaxShort), a.At.Unix(), unixOrZero(a.Until))
	return err
}

func (s *Store) DeleteAcknowledgement(a core.Acknowledgement) (bool, error) {
	res, err := s.db.Exec("DELETE FROM attention_acks WHERE module = ? AND item_id = ? AND meaning = ? AND actor = ? AND at = ? AND until = ?",
		a.Module, a.ID, a.Meaning, cleanAudit(a.By, auditMaxShort), a.At.Unix(), unixOrZero(a.Until))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

func (s *RedisStore) acksKey() string { return s.prefix + ":acks" }

type redisAck struct {
	Module  string `json:"mod"`
	ID      string `json:"id"`
	Meaning string `json:"m"`
	By      string `json:"a"`
	At      int64  `json:"t"`
	Until   int64  `json:"u,omitempty"`
}

// ackField and ackValue are the hash field of an acknowledgement and its value. Encoding one is deterministic, which
// is what lets a delete name exactly the acknowledgement it read.
func ackField(a core.Acknowledgement) string { return a.Module + "/" + a.ID }

func ackValue(a core.Acknowledgement) ([]byte, error) {
	return json.Marshal(redisAck{a.Module, a.ID, a.Meaning, cleanAudit(a.By, auditMaxShort), a.At.Unix(), unixOrZero(a.Until)})
}

// deleteAckScript removes the field only while it still holds this value, in one step.
var deleteAckScript = redis.NewScript(`
if redis.call('HGET', KEYS[1], ARGV[1]) == ARGV[2] then
  return redis.call('HDEL', KEYS[1], ARGV[1])
end
return 0
`)

func (s *RedisStore) Acknowledgements() ([]core.Acknowledgement, error) {
	ctx, cancel := opCtx()
	defer cancel()
	raw, err := s.rdb.HGetAll(ctx, s.acksKey()).Result()
	if err != nil {
		return nil, s.wrap("reading acknowledgements from redis", err)
	}
	out := make([]core.Acknowledgement, 0, len(raw))
	for _, r := range raw {
		var a redisAck
		if json.Unmarshal([]byte(r), &a) != nil {
			// Nothing of it is repeated. Without it the item is simply back in the queue.
			slog.Warn("an acknowledgement in redis could not be read")
			continue
		}
		out = append(out, core.Acknowledgement{Module: a.Module, ID: a.ID, Meaning: a.Meaning, By: a.By, At: time.Unix(a.At, 0), Until: untilOf(a.Until)})
	}
	return out, nil
}

func (s *RedisStore) PutAcknowledgement(a core.Acknowledgement) error {
	v, err := ackValue(a)
	if err != nil {
		return err
	}
	ctx, cancel := opCtx()
	defer cancel()
	if err := s.rdb.HSet(ctx, s.acksKey(), ackField(a), v).Err(); err != nil {
		return s.wrap("writing an acknowledgement to redis", err)
	}
	return nil
}

func (s *RedisStore) DeleteAcknowledgement(a core.Acknowledgement) (bool, error) {
	v, err := ackValue(a)
	if err != nil {
		return false, err
	}
	ctx, cancel := opCtx()
	defer cancel()
	// Eval, not Run: Run would send EVALSHA, which the shared-Redis rules do not list.
	n, err := deleteAckScript.Eval(ctx, s.rdb, []string{s.acksKey()}, ackField(a), string(v)).Int()
	if err != nil {
		return false, s.wrap("deleting an acknowledgement in redis", err)
	}
	return n > 0, nil
}

// AckStore is where the core keeps acknowledgements, or nil when enrolment is off: there is nowhere to persist them,
// so the feature is off.
func (m *Module) AckStore() core.AckStore {
	if m.store == nil {
		return nil
	}
	return m.store
}
