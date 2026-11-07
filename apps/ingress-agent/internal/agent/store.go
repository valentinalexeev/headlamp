// Package agent is the ingress agent (#133): it holds opaque credentials and owns the xray config, nothing else.
// It is the Go port of the former Python app, with the same HTTP API, environment and files. It never logs a
// uuid, an email label or a token: only counts.
package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Cred is one credential: a uuid, its routing mode and its expiry (unix seconds).
type Cred struct {
	UUID     string `json:"uuid"`
	Mode     string `json:"mode"`
	NotAfter int64  `json:"not_after"`
}

// Modes are the routing modes a credential can have.
var Modes = []string{"auto", "direct", "exit"}

// Diff returns the credentials to add and the uuids to remove. A changed mode or expiry counts as remove and add.
func Diff(current, desired []Cred) (added []Cred, removed []string) {
	cur := map[Cred]bool{}
	for _, c := range current {
		cur[c] = true
	}
	want := map[Cred]bool{}
	for _, c := range desired {
		want[c] = true
		if !cur[c] {
			added = append(added, c)
		}
	}
	seen := map[string]bool{}
	for _, c := range current {
		if !want[c] && !seen[c.UUID] {
			seen[c.UUID] = true
			removed = append(removed, c.UUID)
		}
	}
	slices.Sort(removed)
	return added, removed
}

// Sweep drops the credentials that expired at now.
func Sweep(creds []Cred, now int64) []Cred {
	out := make([]Cred, 0, len(creds))
	for _, c := range creds {
		if c.NotAfter > now {
			out = append(out, c)
		}
	}
	return out
}

// Load reads the persisted set; a missing file is an empty set.
func Load(path string) ([]Cred, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Cred
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("credentials file: %w", err)
	}
	return out, nil
}

// Save writes the set atomically with mode 0600.
func Save(path string, creds []Cred) error {
	if creds == nil {
		creds = []Cred{}
	}
	b, _ := json.Marshal(creds)
	return WriteAtomic(path, b, 0o600, "")
}

// WriteAtomic writes data next to path and renames it into place. With suffix, the temporary name ends in it.
func WriteAtomic(path string, data []byte, mode os.FileMode, suffix string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*"+suffix)
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// EmailFor is the opaque label of a credential in xray: routing rules and stats refer to it, and it carries
// nothing about the person. It is the same as the Python agent's, so a running xray needs no re-render.
func EmailFor(uuid string) string {
	sum := sha256.Sum256([]byte(uuid))
	return "u" + hex.EncodeToString(sum[:])[:12] + "@i"
}

// OpaqueID is the stats key of a credential: the first 8 hex characters of SHA-256(uuid).
func OpaqueID(uuid string) string {
	sum := sha256.Sum256([]byte(uuid))
	return hex.EncodeToString(sum[:])[:8]
}

// ParseList is a comma-separated list from an environment variable, with empty entries dropped.
func ParseList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
