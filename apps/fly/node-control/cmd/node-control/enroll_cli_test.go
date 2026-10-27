package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/enroll"
)

func cliEnv(mr *miniredis.Miniredis) core.Env {
	return core.MapEnv(map[string]string{
		"ENROLL_SECRET_KEY":      strings.Repeat("k", 32),
		"REDIS_URL":              "redis://:s3cretpw@" + mr.Addr(),
		"NODE_CONTROL_STATE_KEY": base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
}

func TestEnrollImportExportRoundTrip(t *testing.T) {
	mr := miniredis.RunT(t)
	env := cliEnv(mr)

	db := filepath.Join(t.TempDir(), "e.db")
	src, err := enroll.OpenStore(db, []byte(strings.Repeat("k", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := src.CreateDevice("phone", "alice", "clash", 4_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()

	var out bytes.Buffer
	if err := runEnrollImport([]string{"-from", db}, env, &out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "imported 1 devices") || strings.Contains(out.String(), "alice") {
		t.Fatalf("output = %q (counts only, no names)", out.String())
	}
	if err := runEnrollImport([]string{"-from", db}, env, &out, nil); err == nil {
		t.Fatal("a second import into a populated store must be refused")
	}

	out.Reset()
	if err := runEnrollExport(nil, env, &out); err != nil {
		t.Fatal(err)
	}
	var snap enroll.Snapshot
	if err := json.Unmarshal(out.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Devices) != 1 || snap.Devices[0].ID != d.ID || snap.Devices[0].UUIDs["auto"] != d.UUIDs["auto"] {
		t.Fatalf("export = %+v", snap)
	}

	file := filepath.Join(t.TempDir(), "backup.json")
	if err := runEnrollExport([]string{"-to", file}, env, &out); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("export file: %v, %v", fi, err)
	}
}

func TestEnrollCLIUsageErrors(t *testing.T) {
	mr := miniredis.RunT(t)
	var out bytes.Buffer
	if err := runEnrollImport(nil, cliEnv(mr), &out, nil); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("import without -from: %v", err)
	}
	if err := runEnrollExport(nil, core.MapEnv(nil), &out); err == nil || strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("export without configuration: %v", err)
	}
}

func seedViaImport(t *testing.T, env core.Env) (dbPath string, deviceID int64) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "e.db")
	src, err := enroll.OpenStore(db, []byte(strings.Repeat("k", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := src.CreateDevice("phone", "alice", "clash", 4_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()
	var out bytes.Buffer
	if err := runEnrollImport([]string{"-from", db}, env, &out, nil); err != nil {
		t.Fatal(err)
	}
	return db, d.ID
}

func TestEnrollImportJSONFromFileAndStdin(t *testing.T) {
	mrA := miniredis.RunT(t)
	envA := cliEnv(mrA)
	_, id := seedViaImport(t, envA)

	var exported bytes.Buffer
	if err := runEnrollExport(nil, envA, &exported); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backup.json")
	if err := os.WriteFile(file, exported.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	// From a file into an empty Redis.
	mrB := miniredis.RunT(t)
	var out bytes.Buffer
	if err := runEnrollImport([]string{"-json", file}, cliEnv(mrB), &out, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "imported 1 devices") || strings.Contains(out.String(), "alice") {
		t.Fatalf("output = %q", out.String())
	}
	// From stdin ("-") into another empty Redis; a populated one refuses.
	mrC := miniredis.RunT(t)
	out.Reset()
	if err := runEnrollImport([]string{"-json", "-"}, cliEnv(mrC), &out, bytes.NewReader(exported.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := runEnrollImport([]string{"-json", "-"}, cliEnv(mrC), &out, bytes.NewReader(exported.Bytes())); err == nil {
		t.Fatal("a restore into a populated store must be refused")
	}
	// The restored store exports the same device.
	var again bytes.Buffer
	if err := runEnrollExport(nil, cliEnv(mrC), &again); err != nil {
		t.Fatal(err)
	}
	var snap enroll.Snapshot
	if err := json.Unmarshal(again.Bytes(), &snap); err != nil || len(snap.Devices) != 1 || snap.Devices[0].ID != id {
		t.Fatalf("restored export = %+v, %v", snap, err)
	}
}

func TestEnrollImportNeedsExactlyOneSource(t *testing.T) {
	env := cliEnv(miniredis.RunT(t))
	var out bytes.Buffer
	for _, args := range [][]string{nil, {"-from", "a.db", "-json", "b.json"}} {
		if err := runEnrollImport(args, env, &out, nil); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Fatalf("args %v: %v", args, err)
		}
	}
	if err := runEnrollImport([]string{"-json", "-"}, env, &out, strings.NewReader("SECRET-VALUE")); err == nil || strings.Contains(err.Error(), "SECRET-VALUE") {
		t.Fatalf("garbage restore: %v", err)
	}
}

func TestEnrollExportSQLiteForRollback(t *testing.T) {
	mr := miniredis.RunT(t)
	env := cliEnv(mr)
	_, id := seedViaImport(t, env)

	path := filepath.Join(t.TempDir(), "rollback.db")
	var out bytes.Buffer
	if err := runEnrollExport([]string{"-format", "sqlite", "-to", path}, env, &out); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("nothing belongs on stdout, got %q", out.String())
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v, %v", fi, err)
	}
	s, err := enroll.OpenStore(path, []byte(strings.Repeat("k", 32)), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d, err := s.GetDevice(id)
	if err != nil || d.Name != "phone" {
		t.Fatalf("device in the rolled-back database: %+v, %v", d, err)
	}
	if err := runEnrollExport([]string{"-format", "sqlite", "-to", path}, env, &out); err == nil {
		t.Fatal("an existing file must not be overwritten")
	}
	if err := runEnrollExport([]string{"-format", "sqlite"}, env, &out); err == nil {
		t.Fatal("sqlite output needs -to")
	}
	if err := runEnrollExport([]string{"-format", "yaml"}, env, &out); err == nil {
		t.Fatal("an unknown format was accepted")
	}
}
