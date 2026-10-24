package statestore

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"

	"tailscale.com/ipn"

	"example.com/node-control/internal/core"
)

// KeyFor returns the Redis hash key: REDIS_KEY, or "<FLY_APP_NAME or TAILNET_HOSTNAME>:tsnet". Shared Redis
// keys must be prefixed "<app>:", and a glob character could make a key match more than it names.
func KeyFor(env core.Env) (string, error) {
	key := env.Getenv("REDIS_KEY")
	if key == "" {
		app := env.Getenv("FLY_APP_NAME")
		if app == "" {
			app = env.Default("TAILNET_HOSTNAME", "node-control")
		}
		key = app + ":tsnet"
	}
	if strings.ContainsAny(key, `*?[\`) {
		return "", errors.New("the Redis key must not contain glob characters (* ? [ \\)")
	}
	app, rest, ok := strings.Cut(key, ":")
	if !ok || app == "" || rest == "" {
		return "", errors.New(`the Redis key must look like "<app>:<name>" with a non-empty app prefix`)
	}
	return key, nil
}

// ErrInterrupted reports an import marker left by a run that did not finish.
var ErrInterrupted = errors.New("an interrupted state import is pending")

// Recover undoes an unfinished import: it deletes the keys the marker lists and the marker itself. It reports
// whether there was anything to clean up.
func Recover(j Journal) (bool, error) {
	keys, pending, err := j.Pending()
	if err != nil {
		return pending, err
	}
	if !pending {
		return false, nil
	}
	return true, j.Abort(keys)
}

// Import reads a tailscaled/tsnet file store (a JSON object mapping state key to a base64 string, as written
// by tailscale's FileStore) and writes every entry through dst, then reads each back and compares. Unless
// force is set it refuses when dst already holds state. It logs key names only, never values, and never
// modifies the source.
//
// With a journal (the Redis store) the import is crash-safe: it first stores a marker naming the keys, writes
// and verifies them, and removes the marker last. Any failure rolls back the keys written by this run; a crash
// leaves the marker behind, which Recover (called at startup) turns into a clean slate. A nil journal skips
// this (tests, other stores).
func Import(ctx context.Context, from string, dst ipn.StateStore, force bool, j Journal) (int, error) {
	raw, err := os.ReadFile(from)
	if err != nil {
		return 0, fmt.Errorf("reading state file: %w", err)
	}
	var entries map[string][]byte // encoding/json decodes base64 strings into []byte
	if err := json.Unmarshal(raw, &entries); err != nil {
		return 0, errors.New("state file is not a tailscale state file (a JSON object of base64 strings)")
	}
	if _, bad := entries[MarkerField]; bad {
		return 0, errors.New("state file contains a reserved key")
	}
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	if j != nil {
		if _, pending, err := j.Pending(); err != nil {
			return 0, err
		} else if pending {
			return 0, ErrInterrupted
		}
	}
	if !force {
		if j != nil {
			n, err := j.Len()
			if err != nil {
				return 0, fmt.Errorf("checking the destination: %w", err)
			}
			if n > 0 {
				return 0, errExists
			}
		}
		for _, k := range keys {
			_, err := dst.ReadState(ipn.StateKey(k))
			if err == nil {
				return 0, errExists
			}
			if !errors.Is(err, ipn.ErrStateNotExist) {
				return 0, fmt.Errorf("checking the destination: %w", err)
			}
		}
	}

	// union is what the marker lists and what any rollback or recovery deletes: the keys to import and, for a
	// forced import, every real key already there (-force replaces the whole identity).
	union := keys
	var stale []string
	if j != nil && force {
		existing, err := j.Fields()
		if err != nil {
			return 0, fmt.Errorf("checking the destination: %w", err)
		}
		for _, k := range existing {
			if _, in := entries[k]; !in {
				stale = append(stale, k)
			}
		}
		union = append(append([]string(nil), keys...), stale...)
		sort.Strings(union)
	}
	if j != nil {
		if err := j.Begin(union); err != nil {
			return 0, err
		}
	}
	var written []string
	fail := func(err error) (int, error) {
		// best-effort rollback: remove everything the marker lists, and the marker
		if j != nil {
			j.Abort(union)
			if force {
				err = fmt.Errorf("%w: %w", ErrIdentityRemoved, err)
			}
		} else {
			for _, k := range written {
				dst.WriteState(ipn.StateKey(k), nil)
			}
		}
		return 0, err
	}
	if err := func() error {
		if j == nil {
			return nil
		}
		return j.Delete(stale)
	}(); err != nil {
		return fail(err)
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		v := entries[k]
		if v == nil {
			v = []byte{} // a nil value would delete
		}
		written = append(written, k)
		if err := dst.WriteState(ipn.StateKey(k), v); err != nil {
			return fail(fmt.Errorf("writing state key %q: %w", k, err))
		}
		got, err := dst.ReadState(ipn.StateKey(k))
		if err != nil {
			return fail(fmt.Errorf("verifying state key %q: %w", k, err))
		}
		if !bytes.Equal(got, v) {
			return fail(fmt.Errorf("verifying state key %q: stored value differs from the source", k))
		}
		slog.Debug("state key imported", "key", k)
	}
	if j != nil {
		if err := j.End(); err != nil {
			return fail(err)
		}
	}
	return len(keys), nil
}

// ErrIdentityRemoved is wrapped into the error of a failed forced import: the previous state was deleted and
// the new one was not completed, so Redis holds no tailscale state.
var ErrIdentityRemoved = errors.New("the previous tailscale state was removed from Redis (-force replaces the whole identity) and the import did not complete; " +
	"rerun the import with the state file, or delete the state and let the node register again with TAILSCALE_AUTH_KEY")

var errExists = errors.New("the destination already holds tailscale state; refusing to overwrite it (use -force to replace it)")
