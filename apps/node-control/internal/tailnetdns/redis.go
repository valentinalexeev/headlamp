package tailnetdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

// auditKeep is how many audit entries the Redis store keeps.
const auditKeep = 1000

// redisStore keeps the records in the shared Redis: the hash "<prefix>:records" (name -> record JSON) and the list
// "<prefix>:audit" (newest first). Records are not secret, so they are stored in clear.
type redisStore struct {
	rdb    *redis.Client
	prefix string
}

// NewRedisStore keeps the store under prefix.
func NewRedisStore(rdb *redis.Client, prefix string) (Store, error) {
	if prefix == "" || strings.ContainsAny(prefix, "*?[\\") {
		return nil, fmt.Errorf("tailnet DNS store: invalid Redis key prefix %q", prefix)
	}
	return &redisStore{rdb: rdb, prefix: prefix}, nil
}

func (r *redisStore) recKey() string   { return r.prefix + ":records" }
func (r *redisStore) auditKey() string { return r.prefix + ":audit" }

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

func (r *redisStore) Records() ([]Record, error) {
	c, cancel := opCtx()
	defer cancel()
	m, err := r.rdb.HGetAll(c, r.recKey()).Result()
	if err != nil {
		return nil, fmt.Errorf("tailnet DNS store: %w", err)
	}
	out := make([]Record, 0, len(m))
	for name, v := range m {
		var rec Record
		if err := json.Unmarshal([]byte(v), &rec); err != nil {
			return nil, fmt.Errorf("tailnet DNS store: record %s: %w", name, err)
		}
		rec.Name = name
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func recJSON(rec Record) string {
	b, _ := json.Marshal(rec)
	return string(b)
}

func (r *redisStore) Create(rec Record) error {
	c, cancel := opCtx()
	defer cancel()
	ok, err := r.rdb.HSetNX(c, r.recKey(), rec.Name, recJSON(rec)).Result()
	if err != nil {
		return fmt.Errorf("tailnet DNS store: %w", err)
	}
	if !ok {
		return ErrExists
	}
	return nil
}

// Replace writes the record only if it exists, in one transaction: a concurrent delete wins.
func (r *redisStore) Replace(rec Record) error {
	c, cancel := opCtx()
	defer cancel()
	err := r.rdb.Watch(c, func(tx *redis.Tx) error {
		ok, err := tx.HExists(c, r.recKey(), rec.Name).Result()
		if err != nil {
			return err
		}
		if !ok {
			return ErrNotFound
		}
		_, err = tx.TxPipelined(c, func(p redis.Pipeliner) error {
			p.HSet(c, r.recKey(), rec.Name, recJSON(rec))
			return nil
		})
		return err
	}, r.recKey())
	if errors.Is(err, ErrNotFound) {
		return err
	}
	if errors.Is(err, redis.TxFailedErr) {
		return fmt.Errorf("tailnet DNS store: concurrent change, try again")
	}
	if err != nil {
		return fmt.Errorf("tailnet DNS store: %w", err)
	}
	return nil
}

func (r *redisStore) Delete(name string) error {
	c, cancel := opCtx()
	defer cancel()
	n, err := r.rdb.HDel(c, r.recKey(), name).Result()
	if err != nil {
		return fmt.Errorf("tailnet DNS store: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (r *redisStore) Audit(at time.Time, actor, action, detail string) error {
	b, _ := json.Marshal(AuditEntry{At: at.UTC(), Actor: actor, Action: action, Detail: detail})
	c, cancel := opCtx()
	defer cancel()
	_, err := r.rdb.TxPipelined(c, func(p redis.Pipeliner) error {
		p.LPush(c, r.auditKey(), string(b))
		p.LTrim(c, r.auditKey(), 0, auditKeep-1)
		return nil
	})
	return err
}

func (r *redisStore) AuditLog(limit int) ([]AuditEntry, error) {
	c, cancel := opCtx()
	defer cancel()
	items, err := r.rdb.LRange(c, r.auditKey(), 0, int64(limit)-1).Result()
	if err != nil {
		return nil, fmt.Errorf("tailnet DNS store: %w", err)
	}
	out := make([]AuditEntry, 0, len(items))
	for _, s := range items {
		var e AuditEntry
		if json.Unmarshal([]byte(s), &e) == nil {
			out = append(out, e)
		}
	}
	return out, nil
}

func (r *redisStore) Close() error { return r.rdb.Close() }
