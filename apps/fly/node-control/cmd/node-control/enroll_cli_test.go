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
	if err := runEnrollImport([]string{"-from", db}, env, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "imported 1 devices") || strings.Contains(out.String(), "alice") {
		t.Fatalf("output = %q (counts only, no names)", out.String())
	}
	if err := runEnrollImport([]string{"-from", db}, env, &out); err == nil {
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
	if err := runEnrollImport(nil, cliEnv(mr), &out); err == nil || !strings.Contains(err.Error(), "usage") {
		t.Fatalf("import without -from: %v", err)
	}
	if err := runEnrollExport(nil, core.MapEnv(nil), &out); err == nil || strings.Contains(err.Error(), "s3cretpw") {
		t.Fatalf("export without configuration: %v", err)
	}
}
