// Package statestore keeps the embedded tailscale node's state in a shared Redis hash, encrypted at rest.
//
// A port of node-control's statestore package (itself ported from the same owner's botboard: redisstore.go,
// encstore.go): the Redis URL never appears in an error, keys are validated, and an importer carries over an
// existing tailscaled/tsnet state file. scripts/check-statestore-port.sh shows any drift from the original.
//
// Shared Redis rules (umbrella): plain redis:// on the private network, DB 0 only, a key prefixed "<app>:",
// no TTL, no polling. The only commands sent are HELLO 2 (go-redis always opens with it; "HELLO 2 AUTH <user>
// <pass>" authenticates and keeps the connection on RESP2), PING, HGET, HSET, HDEL, HLEN, HEXISTS and (forced imports only) HKEYS. CLIENT
// SETINFO and the maintenance-notification handshake are off, and nothing selects a database, scans or flushes.
package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/redis/go-redis/v9/maintnotifications"
	"tailscale.com/ipn"

	"funnel-ingress/internal/scrub"
)

// opTimeout bounds the connection check and every single operation.
const opTimeout = 10 * time.Second

// MarkerField is the reserved hash field that marks an import in progress. It holds a JSON list of the state
// key NAMES being written (no values, no secrets), is stored unencrypted, and is never visible as tailscale state.
const MarkerField = "_funnel-ingress-import"

var (
	logOnce    sync.Once
	logMu      sync.Mutex
	logSecrets []string
)

// scrubLogger routes go-redis's internal log lines through slog with every known secret removed (the library
// would otherwise print raw error text to stderr).
type scrubLogger struct{}

func (scrubLogger) Printf(_ context.Context, format string, v ...any) {
	logMu.Lock()
	secrets := append([]string(nil), logSecrets...)
	logMu.Unlock()
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) }) // longest first: URL before password
	slog.Warn(scrub.Scrub(fmt.Sprintf(format, v...), secrets))
}

func installLogger(secrets []string) {
	logMu.Lock()
	logSecrets = append(logSecrets, secrets...)
	logMu.Unlock()
	logOnce.Do(func() { redis.SetLogger(scrubLogger{}) })
}

// Redis is an ipn.StateStore that keeps each state key as one field of a Redis hash.
type Redis struct {
	rdb     *redis.Client
	key     string
	secrets []string // the URL and its password, removed from every error text
}

// secretsFor lists the strings that must never reach an error: the whole URL, the password and its
// URL-escaped form.
func secretsFor(rawURL, password string) []string {
	s := []string{rawURL}
	if password != "" {
		s = append(s, password, url.QueryEscape(password), url.PathEscape(password))
	}
	return s
}

// NewRedis connects to rawURL (redis:// only; it does not select a database) and uses hash key. It pings once
// and returns an error when Redis is unreachable. No error text contains rawURL.
func NewRedis(rawURL, key string) (*Redis, error) {
	opt, err := redis.ParseURL(rawURL)
	if err != nil {
		// ParseURL errors may echo the URL, so none of it is passed on.
		return nil, errors.New("invalid REDIS_URL")
	}
	opt.DialTimeout = opTimeout
	opt.ReadTimeout = opTimeout
	opt.WriteTimeout = opTimeout
	opt.PoolSize = 3           // the app has a 256 MB budget and state traffic is tiny
	opt.DisableIdentity = true // no CLIENT SETINFO
	opt.Protocol = 2           // RESP2: no HELLO, AUTH instead
	opt.MaintNotificationsConfig = &maintnotifications.Config{Mode: maintnotifications.ModeDisabled}
	secrets := secretsFor(rawURL, opt.Password)
	installLogger(secrets)
	r := &Redis{rdb: redis.NewClient(opt), key: key, secrets: secrets}

	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := r.rdb.Ping(ctx).Err(); err != nil {
		r.rdb.Close()
		return nil, r.wrap("redis unreachable", err)
	}
	return r, nil
}

// wrap turns a go-redis error into one that is safe to log.
func (r *Redis) wrap(op string, err error) error {
	return errors.New(op + ": " + scrub.Scrub(err.Error(), r.secrets))
}

func (r *Redis) ReadState(id ipn.StateKey) ([]byte, error) {
	if string(id) == MarkerField {
		return nil, ipn.ErrStateNotExist
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	b, err := r.rdb.HGet(ctx, r.key, string(id)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ipn.ErrStateNotExist
	}
	if err != nil {
		return nil, r.wrap("reading state from redis", err)
	}
	return b, nil
}

// WriteState stores bs under id; a nil bs deletes the field.
func (r *Redis) WriteState(id ipn.StateKey, bs []byte) error {
	if string(id) == MarkerField {
		return errors.New("reserved state key")
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if bs == nil {
		if err := r.rdb.HDel(ctx, r.key, string(id)).Err(); err != nil {
			return r.wrap("deleting state in redis", err)
		}
		return nil
	}
	if err := r.rdb.HSet(ctx, r.key, string(id), bs).Err(); err != nil {
		return r.wrap("writing state to redis", err)
	}
	return nil
}

// Len returns how many state keys the hash holds, not counting the import marker (HLEN, plus HEXISTS).
func (r *Redis) Len() (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	n, err := r.rdb.HLen(ctx, r.key).Result()
	if err != nil {
		return 0, r.wrap("counting state in redis", err)
	}
	if n > 0 {
		has, err := r.rdb.HExists(ctx, r.key, MarkerField).Result()
		if err != nil {
			return 0, r.wrap("counting state in redis", err)
		}
		if has {
			n--
		}
	}
	return n, nil
}

// Journal records an import in progress so that an interrupted one can be detected and undone.
type Journal interface {
	// Len counts real state keys, ignoring the marker.
	Len() (int64, error)
	// Keys lists the real state key names, not the marker (HKEYS on the small state hash).
	Fields() ([]string, error)
	// Delete removes the given keys (one HDEL).
	Delete(keys []string) error
	// Begin stores the marker listing the key names about to be written.
	Begin(keys []string) error
	// Pending reports an unfinished import and the key names it listed.
	Pending() (keys []string, pending bool, err error)
	// Abort deletes keys and the marker in one command.
	Abort(keys []string) error
	// End removes the marker.
	End() error
}

var _ Journal = (*Redis)(nil)

func (r *Redis) Fields() ([]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	all, err := r.rdb.HKeys(ctx, r.key).Result()
	if err != nil {
		return nil, r.wrap("listing state in redis", err)
	}
	out := all[:0]
	for _, k := range all {
		if k != MarkerField {
			out = append(out, k)
		}
	}
	return out, nil
}

func (r *Redis) Delete(keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := r.rdb.HDel(ctx, r.key, keys...).Err(); err != nil {
		return r.wrap("deleting state in redis", err)
	}
	return nil
}

func (r *Redis) Begin(keys []string) error {
	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := r.rdb.HSet(ctx, r.key, MarkerField, b).Err(); err != nil {
		return r.wrap("writing the import marker", err)
	}
	return nil
}

func (r *Redis) Pending() ([]string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	b, err := r.rdb.HGet(ctx, r.key, MarkerField).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, r.wrap("reading the import marker", err)
	}
	var keys []string
	if err := json.Unmarshal(b, &keys); err != nil {
		return nil, true, errors.New("the import marker in Redis is unreadable; delete the state hash and start again")
	}
	return keys, true, nil
}

func (r *Redis) Abort(keys []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	fields := append(append([]string(nil), keys...), MarkerField)
	if err := r.rdb.HDel(ctx, r.key, fields...).Err(); err != nil {
		return r.wrap("rolling back the import", err)
	}
	return nil
}

func (r *Redis) End() error {
	ctx, cancel := context.WithTimeout(context.Background(), opTimeout)
	defer cancel()
	if err := r.rdb.HDel(ctx, r.key, MarkerField).Err(); err != nil {
		return r.wrap("finishing the import", err)
	}
	return nil
}

// Close releases the connections.
func (r *Redis) Close() error { return r.rdb.Close() }
