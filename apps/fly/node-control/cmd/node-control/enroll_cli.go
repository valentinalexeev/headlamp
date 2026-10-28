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

// runEnrollImport implements `node-control enroll-import (-from <enroll.db> | -json <file|->) [-force]`. -from copies
// the SQLite enrolment database into Redis (opened read-only); -json restores an `enroll-export` file ("-" reads
// stdin, so a sops-encrypted backup can be piped in without a plaintext file). Both verify the copy by reading it
// back. Only counts are printed.
func runEnrollImport(args []string, env core.Env, out io.Writer, in io.Reader) error {
	const usage = "usage: node-control enroll-import (-from <enroll.db> | -json <file|->) [-force]"
	fs := flag.NewFlagSet("enroll-import", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	from := fs.String("from", "", "enrolment SQLite database to import")
	jsonPath := fs.String("json", "", "enrolment export (JSON) to restore; - reads stdin")
	force := fs.Bool("force", false, "overwrite records that already exist in Redis")
	if err := fs.Parse(args); err != nil || (*from == "") == (*jsonPath == "") {
		return errors.New(usage)
	}
	rs, key, err := enrollRedisFromEnv(env)
	if err != nil {
		return err
	}
	defer rs.Close()
	var devices, links, approvals int
	if *from != "" {
		src, err := enroll.OpenStoreReadOnly(*from, key, nil)
		if err != nil {
			return err
		}
		defer src.Close()
		devices, links, approvals, err = enroll.Import(src, rs, *force)
		if err != nil {
			return err
		}
	} else {
		r := in
		if *jsonPath != "-" {
			f, err := os.Open(*jsonPath)
			if err != nil {
				return err
			}
			defer f.Close()
			r = f
		}
		snap, err := enroll.ParseSnapshot(r)
		if err != nil {
			return err
		}
		devices, links, approvals, err = enroll.ImportSnapshot(snap, rs, *force)
		if err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "imported %d devices, %d links, %d approvals; verified\n", devices, links, approvals)
	return nil
}

// runEnrollExport implements `node-control enroll-export [-format json|sqlite] [-to <file>]`. json (the default, to
// stdout unless -to is given) is the backup format `enroll-import -json` reads. sqlite writes a new enrolment database
// that the previous (SQLite) release opens as it is: it needs -to and refuses an existing file; use it to roll back.
// Either output holds device names, owners and credentials: treat it as a secret and encrypt it before it is stored.
// One-time links are not included.
func runEnrollExport(args []string, env core.Env, out io.Writer) error {
	fs := flag.NewFlagSet("enroll-export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	to := fs.String("to", "", "file to write (mode 0600); default stdout for json")
	format := fs.String("format", "json", "json or sqlite")
	if err := fs.Parse(args); err != nil || (*format != "json" && *format != "sqlite") || (*format == "sqlite" && *to == "") {
		return errors.New("usage: node-control enroll-export [-format json] [-to <file>] | -format sqlite -to <new file>")
	}
	rs, key, err := enrollRedisFromEnv(env)
	if err != nil {
		return err
	}
	defer rs.Close()
	snap, err := rs.Snapshot()
	if err != nil {
		return err
	}
	if *format == "sqlite" {
		if err := enroll.WriteSQLite(snap, *to, key); err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "wrote %d devices to %s (SQLite)\n", len(snap.Devices), *to)
		return nil
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
