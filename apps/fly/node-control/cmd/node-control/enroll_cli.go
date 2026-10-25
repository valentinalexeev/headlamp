package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/enroll"
)

// enrollRedisFromEnv opens the Redis enrolment store the subcommands act on. ENROLL_REDIS is not required to be set:
// naming the subcommand is the intent.
func enrollRedisFromEnv(env core.Env) (*enroll.RedisStore, []byte, error) {
	key := env.Getenv("ENROLL_SECRET_KEY")
	if len(key) < 16 {
		return nil, nil, errors.New("ENROLL_SECRET_KEY must be set (at least 16 bytes)")
	}
	rs, err := enroll.OpenRedisStore(env, []byte(key), nil)
	if err != nil {
		return nil, nil, err
	}
	return rs, []byte(key), nil
}

// runEnrollImport implements `node-control enroll-import -from <enroll.db> [-force]`: it copies the SQLite
// enrolment database into Redis (the file is opened read-only) and verifies the copy. Only counts are printed.
func runEnrollImport(args []string, env core.Env, out io.Writer) error {
	fs := flag.NewFlagSet("enroll-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "enrolment SQLite database to import")
	force := fs.Bool("force", false, "overwrite records that already exist in Redis")
	if err := fs.Parse(args); err != nil || *from == "" {
		return errors.New("usage: node-control enroll-import -from <enroll.db> [-force]")
	}
	rs, key, err := enrollRedisFromEnv(env)
	if err != nil {
		return err
	}
	defer rs.Close()
	src, err := enroll.OpenStoreReadOnly(*from, key, nil)
	if err != nil {
		return err
	}
	defer src.Close()
	devices, links, approvals, err := enroll.Import(src, rs, *force)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "imported %d devices, %d links, %d approvals; verified\n", devices, links, approvals)
	return nil
}

// runEnrollExport implements `node-control enroll-export [-to <file>]`: it writes the enrolment state as JSON
// (stdout by default). The output holds device names, owners and credentials: treat it as a secret and encrypt it
// before it is stored. One-time links are not exported.
func runEnrollExport(args []string, env core.Env, out io.Writer) error {
	fs := flag.NewFlagSet("enroll-export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "file to write (mode 0600); default stdout")
	if err := fs.Parse(args); err != nil {
		return errors.New("usage: node-control enroll-export [-to <file>]")
	}
	rs, _, err := enrollRedisFromEnv(env)
	if err != nil {
		return err
	}
	defer rs.Close()
	snap, err := rs.Snapshot()
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if *to == "" {
		_, err = out.Write(b)
		return err
	}
	if err := os.WriteFile(*to, b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %d devices to %s\n", len(snap.Devices), *to)
	return nil
}
