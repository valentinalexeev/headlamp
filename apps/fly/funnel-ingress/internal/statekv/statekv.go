// Package statekv opens the state stores of funnel-ingress. With REDIS_URL the embedded tailscale node, the
// dynamic routes and the cached configuration share one encrypted Redis hash (field names keep them apart);
// without it the node keeps its own state file under the state directory and the routes and configuration
// go to a small file store beside it (standalone use of the public app).
//
// Shared Redis rules (umbrella) are enforced by the statestore package: plain redis://, DB 0 only, a key
// prefixed "<app>:", no TTL, no polling, a fixed command set. This package adds none of its own commands.
package statekv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"tailscale.com/ipn"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/statestore"
)

// Field names inside the shared store. They never collide with tailscale's own state keys.
const (
	RoutesField = "funnel/routes"
	ConfigField = "funnel/config"
)

// Stores are the opened state stores.
type Stores struct {
	// TSNet is the store for the embedded tailscale node. It is nil in file mode: tsnet then keeps its
	// default state file under Config.StateDir.
	TSNet ipn.StateStore
	// KV holds the routes and configuration blobs (RoutesField, ConfigField).
	KV    ipn.StateStore
	Close func()
}

// Open builds the stores for cfg. In Redis mode it fails fast when Redis is unreachable, so the node never
// registers a new identity just because Redis was briefly down, and it refuses to start on partial state
// left by an interrupted import.
func Open(ctx context.Context, cfg config.Config) (Stores, error) {
	if cfg.RedisURL == "" {
		return openFile(cfg)
	}
	rs, err := statestore.NewRedis(cfg.RedisURL, cfg.RedisKey)
	if err != nil {
		return Stores{}, fmt.Errorf("redis state store: %w", err)
	}
	var once sync.Once
	closeFn := func() { once.Do(func() { rs.Close() }) }
	fail := func(err error) (Stores, error) { closeFn(); return Stores{}, err }

	enc, err := statestore.NewEncrypted(rs, cfg.StateKey, cfg.RedisKey)
	if err != nil {
		return fail(err)
	}

	fileExists := false
	if cfg.ImportFrom != "" {
		_, serr := os.Stat(cfg.ImportFrom)
		fileExists = serr == nil
	}
	// An unfinished import (crash, restart) leaves a marker and possibly a subset of the keys. Undo it, and
	// never start the node on partial state.
	cleaned, err := statestore.Recover(rs)
	if err != nil {
		return fail(fmt.Errorf("cleaning up an interrupted state import: %w", err))
	}
	if cleaned {
		slog.Warn("an interrupted state import was cleaned up")
		if !fileExists {
			return fail(errors.New("an interrupted state import was cleaned up; set STATE_IMPORT_FROM to the old " +
				"state file and restart"))
		}
	}
	if cfg.ImportFrom != "" && fileExists {
		n, err := rs.Len()
		if err != nil {
			return fail(err)
		}
		if n > 0 {
			slog.Info("STATE_IMPORT_FROM ignored: Redis already holds state")
		} else {
			count, err := statestore.Import(ctx, cfg.ImportFrom, enc, false, rs)
			if err != nil {
				return fail(fmt.Errorf("importing tailscale state: %w", err))
			}
			slog.Info(fmt.Sprintf("imported %d state keys", count))
		}
	}
	return Stores{TSNet: enc, KV: enc, Close: closeFn}, nil
}

func openFile(cfg config.Config) (Stores, error) {
	if cfg.StateKey != "" {
		slog.Warn("FUNNEL_STATE_KEY is ignored: REDIS_URL is not set, so state stays in unencrypted files")
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return Stores{}, fmt.Errorf("creating the state directory: %w", err)
	}
	kv, err := newFileStore(filepath.Join(cfg.StateDir, "funnel-kv.json"))
	if err != nil {
		return Stores{}, err
	}
	return Stores{KV: kv, Close: func() {}}, nil
}

// fileStore is a tiny ipn.StateStore over one JSON file ({"field": "<base64>"}), written atomically.
type fileStore struct {
	path string
	mu   sync.Mutex
	m    map[string][]byte
}

func newFileStore(path string) (*fileStore, error) {
	f := &fileStore{path: path, m: map[string][]byte{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return f, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", filepath.Base(path), err)
	}
	if err := json.Unmarshal(b, &f.m); err != nil {
		// Never start empty and overwrite a file we cannot read.
		return nil, fmt.Errorf("%s is not a valid state file; fix or remove it", filepath.Base(path))
	}
	return f, nil
}

func (f *fileStore) ReadState(id ipn.StateKey) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.m[string(id)]
	if !ok {
		return nil, ipn.ErrStateNotExist
	}
	return append([]byte(nil), v...), nil
}

func (f *fileStore) WriteState(id ipn.StateKey, bs []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	next := make(map[string][]byte, len(f.m)+1)
	for k, v := range f.m {
		next[k] = v
	}
	if bs == nil {
		delete(next, string(id))
	} else {
		next[string(id)] = append([]byte(nil), bs...)
	}
	b, err := json.Marshal(next)
	if err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, f.path); err != nil {
		return err
	}
	f.m = next
	return nil
}

// Field reads and writes one field of a state store as an opaque blob. It satisfies routes.Persister and
// controlsync.Blob.
type Field struct {
	kv    ipn.StateStore
	field string
}

// NewPersister returns the persister for the dynamic routes (field RoutesField).
func NewPersister(kv ipn.StateStore, field string) *Field { return &Field{kv: kv, field: field} }

// NewBlob returns the blob for the configuration cache (field ConfigField).
func NewBlob(kv ipn.StateStore, field string) *Field { return &Field{kv: kv, field: field} }

// Load returns the stored bytes; ok is false when the field is absent.
func (f *Field) Load() ([]byte, bool, error) {
	b, err := f.kv.ReadState(ipn.StateKey(f.field))
	if errors.Is(err, ipn.ErrStateNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return b, true, nil
}

// Save stores b (one write).
func (f *Field) Save(b []byte) error { return f.kv.WriteState(ipn.StateKey(f.field), b) }
