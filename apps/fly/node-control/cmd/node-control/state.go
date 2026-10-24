package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"tailscale.com/ipn"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/statestore"
)

// openState builds the encrypted Redis state store from the environment. It returns nil (and no error) when
// REDIS_URL is unset: the embedded node then keeps its state in a file under /var/lib/tailscale, as before.
// The second result is the Redis store itself, for the emptiness check; closeFn releases it.
func openState(env core.Env) (store ipn.StateStore, rs *statestore.Redis, closeFn func(), err error) {
	closeFn = func() {}
	url := env.Getenv("REDIS_URL")
	stateKey := env.Getenv("NODE_CONTROL_STATE_KEY")
	if url == "" {
		if stateKey != "" {
			slog.Warn("NODE_CONTROL_STATE_KEY is ignored: REDIS_URL is not set, so tailscale state stays in an unencrypted file")
		}
		return nil, nil, closeFn, nil
	}
	if stateKey == "" && env.Getenv("STATE_ALLOW_PLAINTEXT") != "1" {
		return nil, nil, closeFn, errors.New("REDIS_URL is set but NODE_CONTROL_STATE_KEY is not: tailscale state holds private keys and Redis is shared, " +
			"so it must be encrypted. Generate a key with `openssl rand -base64 32` and store it as a Fly secret " +
			"(or set STATE_ALLOW_PLAINTEXT=1 to store the state unencrypted)")
	}
	if err := statestore.ValidateStateKey(stateKey); err != nil {
		return nil, nil, closeFn, err
	}
	key, err := statestore.KeyFor(env)
	if err != nil {
		return nil, nil, closeFn, err
	}
	rs, err = statestore.NewRedis(url, key)
	if err != nil {
		return nil, nil, closeFn, fmt.Errorf("redis state store: %w", err)
	}
	enc, err := statestore.NewEncrypted(rs, stateKey, key)
	if err != nil {
		rs.Close()
		return nil, nil, closeFn, err
	}
	var once sync.Once
	return enc, rs, func() { once.Do(func() { rs.Close() }) }, nil
}

// buildStateStore returns the store for tsnet (nil: use the default file) and its close function. When
// STATE_IMPORT_FROM names a file and the Redis hash is still empty, it imports that file once.
func buildStateStore(ctx context.Context, env core.Env) (ipn.StateStore, func(), error) {
	store, rs, closeFn, err := openState(env)
	if err != nil || store == nil {
		return nil, closeFn, err
	}
	from := env.Getenv("STATE_IMPORT_FROM")
	fileExists := false
	if from != "" {
		_, serr := os.Stat(from)
		fileExists = serr == nil
	}

	// An unfinished import (crash, restart) leaves a marker and possibly a subset of the keys. Undo it, and never
	// start the node on partial state.
	cleaned, err := statestore.Recover(rs)
	if err != nil {
		closeFn()
		return nil, func() {}, fmt.Errorf("cleaning up an interrupted state import: %w", err)
	}
	if cleaned {
		slog.Warn("an interrupted state import was cleaned up")
		if !fileExists {
			closeFn()
			return nil, func() {}, errors.New("an interrupted state import was cleaned up; set STATE_IMPORT_FROM to the old " +
				"state file and restart, or run `node-control state-import -from <file>`")
		}
	}
	if from == "" {
		return store, closeFn, nil
	}
	n, err := rs.Len()
	if err != nil {
		closeFn()
		return nil, func() {}, err
	}
	if n > 0 {
		slog.Info("STATE_IMPORT_FROM ignored: Redis already holds tailscale state")
		return store, closeFn, nil
	}
	if !fileExists {
		return store, closeFn, nil // fresh start: the node registers with TAILSCALE_AUTH_KEY
	}
	count, err := statestore.Import(ctx, from, store, false, rs)
	if err != nil {
		closeFn()
		return nil, func() {}, fmt.Errorf("importing tailscale state: %w", err)
	}
	slog.Info(fmt.Sprintf("imported %d state keys", count))
	return store, closeFn, nil
}

// runStateImport implements `node-control state-import -from <file> [-force]`.
func runStateImport(ctx context.Context, args []string, env core.Env, out io.Writer) error {
	fs := flag.NewFlagSet("state-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "tailscaled/tsnet state file to import")
	force := fs.Bool("force", false, "overwrite state that already exists in Redis")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("usage: node-control state-import -from <file> [-force]: %w", err)
	}
	if *from == "" {
		return errors.New("usage: node-control state-import -from <file> [-force]")
	}
	store, rs, closeFn, err := openState(env)
	if err != nil {
		return err
	}
	defer closeFn()
	if store == nil {
		return errors.New("state-import needs REDIS_URL")
	}
	if cleaned, err := statestore.Recover(rs); err != nil {
		return fmt.Errorf("cleaning up an interrupted state import: %w", err)
	} else if cleaned {
		fmt.Fprintln(out, "cleaned up an interrupted earlier import")
	}
	n, err := statestore.Import(ctx, *from, store, *force, rs)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "imported %d state keys\n", n)
	return nil
}
