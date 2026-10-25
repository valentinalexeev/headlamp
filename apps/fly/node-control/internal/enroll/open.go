package enroll

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/statestore"
)

// RedisEnabled reports whether the environment selects the Redis store (ENROLL_REDIS=1).
func RedisEnabled(env core.Env) bool { return env.Getenv("ENROLL_REDIS") == "1" }

// RedisPrefix is the key prefix of the enrolment data: ENROLL_REDIS_KEY, or "<app>:enroll" with the app named like
// the tailscale state key's (FLY_APP_NAME, else TAILNET_HOSTNAME, else node-control).
func RedisPrefix(env core.Env) string {
	if k := env.Getenv("ENROLL_REDIS_KEY"); k != "" {
		return k
	}
	app := env.Getenv("FLY_APP_NAME")
	if app == "" {
		app = env.Default("TAILNET_HOSTNAME", "node-control")
	}
	return app + ":enroll"
}

// OpenRedisStore connects the Redis store from the environment: REDIS_URL, NODE_CONTROL_STATE_KEY (values are sealed
// with it; without it the store refuses to start unless STATE_ALLOW_PLAINTEXT=1), ENROLL_REDIS_KEY. secretKey is
// ENROLL_SECRET_KEY, which derives the subscription secrets. A nil clock means time.Now.
func OpenRedisStore(env core.Env, secretKey []byte, clock func() time.Time) (*RedisStore, error) {
	url := env.Getenv("REDIS_URL")
	if url == "" {
		return nil, errors.New("ENROLL_REDIS is set but REDIS_URL is not")
	}
	stateKey := env.Getenv("NODE_CONTROL_STATE_KEY")
	if stateKey == "" && env.Getenv("STATE_ALLOW_PLAINTEXT") != "1" {
		return nil, errors.New("ENROLL_REDIS is set but NODE_CONTROL_STATE_KEY is not: enrolment records hold device names, owners and " +
			"credentials and Redis is shared, so they must be encrypted (or set STATE_ALLOW_PLAINTEXT=1)")
	}
	c, err := statestore.NewCipher(stateKey)
	if err != nil {
		return nil, err
	}
	prefix := RedisPrefix(env)
	if err := validatePrefix(prefix); err != nil {
		return nil, err
	}
	rdb, secrets, err := statestore.OpenClient(url)
	if err != nil {
		return nil, fmt.Errorf("enrolment redis store: %w", err)
	}
	s, err := NewRedisStore(rdb, secrets, c, prefix, secretKey, clock)
	if err != nil {
		rdb.Close()
		return nil, err
	}
	return s, nil
}

// ImportOnStart implements ENROLL_IMPORT_FROM: when the Redis store has no data and the SQLite file exists, copy it
// once (read-only on the file, verified), so a rebuilt machine needs no manual step. A store that already has data
// is left alone. Only counts are logged.
func ImportOnStart(env core.Env, dst *RedisStore, secretKey []byte) error {
	from := env.Getenv("ENROLL_IMPORT_FROM")
	if from == "" {
		return nil
	}
	has, err := dst.HasData()
	if err != nil {
		return err
	}
	if has {
		slog.Info("ENROLL_IMPORT_FROM ignored: Redis already holds enrolment data")
		return nil
	}
	if _, err := os.Stat(from); err != nil {
		slog.Info("ENROLL_IMPORT_FROM ignored: the file does not exist (fresh start)")
		return nil
	}
	src, err := OpenStoreReadOnly(from, secretKey, nil)
	if err != nil {
		return err
	}
	defer src.Close()
	devices, links, approvals, err := Import(src, dst, false)
	if err != nil {
		return fmt.Errorf("importing the enrolment database: %w", err)
	}
	slog.Info(fmt.Sprintf("imported the enrolment database: %d devices, %d links, %d approvals", devices, links, approvals))
	return nil
}
