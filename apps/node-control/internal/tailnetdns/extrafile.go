package tailnetdns

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
)

// extraFile publishes the managed records to headscale (#286): the file of headscale's dns.extra_records_path,
// which headscale watches and applies without a restart. headscale then sends the records to every node in the
// netmap, so tailnet devices resolve the names themselves and do not depend on node-control.
//
// It works only where node-control and headscale share a directory (the compose stack). node-control owns the
// whole file: other records in it are replaced. headscale does not start while the file is missing, so the operator
// creates it once with "[]".
//
// The file is written only when its content changes, sorted and stable (headscale reloads on a checksum change), and
// atomically: a temporary file in the same directory, renamed into place. Mount the directory, not the file: a
// single-file bind mount keeps the old inode, and headscale never sees the rename.
type extraFile struct {
	path string

	mu      sync.Mutex
	last    []byte
	written time.Time
	err     string
}

// extraEntry is one record of the file, in the format of headscale's extra_records_path.
type extraEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

// checkExtraPath validates TAILNET_DNS_EXTRA_RECORDS_FILE at start: an absolute path in a directory that exists.
func checkExtraPath(p string) error {
	if !filepath.IsAbs(p) {
		return errors.New("TAILNET_DNS_EXTRA_RECORDS_FILE must be an absolute path")
	}
	if filepath.Ext(p) != ".json" {
		return errors.New("TAILNET_DNS_EXTRA_RECORDS_FILE must end in .json (headscale reads the format from the extension)")
	}
	st, err := os.Stat(filepath.Dir(p))
	if err != nil || !st.IsDir() {
		return errors.New("TAILNET_DNS_EXTRA_RECORDS_FILE: its directory does not exist (mount a directory shared with headscale)")
	}
	return nil
}

// extraContent renders the records of a table as the file: one entry per name and address, sorted by name, then
// type, then address. A name without addresses has no entry.
func extraContent(t *servedns.Table) []byte {
	entries := []extraEntry{}
	if t != nil {
		for _, n := range t.Names() {
			addrs, _, _ := t.Lookup(n)
			sorted := append([]netip.Addr(nil), addrs...)
			sort.Slice(sorted, func(i, j int) bool { return sorted[i].Less(sorted[j]) })
			for _, a := range sorted {
				typ := "A"
				if a.Is6() && !a.Is4In6() {
					typ = "AAAA"
				}
				entries = append(entries, extraEntry{Name: n, Type: typ, Value: a.Unmap().String()})
			}
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].Type < entries[j].Type
	})
	b, _ := json.MarshalIndent(entries, "", "  ")
	return append(b, '\n')
}

// write puts content into the file when it differs from the file on disk. now is the time of a write.
func (f *extraFile) write(content []byte, now time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	err := f.writeLocked(content, now)
	f.err = ""
	if err != nil {
		f.err = err.Error()
	}
	return err
}

func (f *extraFile) writeLocked(content []byte, now time.Time) error {
	if bytes.Equal(content, f.last) {
		return nil
	}
	if old, err := os.ReadFile(f.path); err == nil && bytes.Equal(old, content) {
		f.last = content
		return nil
	}
	dir := filepath.Dir(f.path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(f.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("extra records file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) // a no-op after the rename
	_, werr := tmp.Write(content)
	cerr := tmp.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("extra records file: write: %w", errors.Join(werr, cerr))
	}
	// headscale runs as another user in its own container: the file must be readable by all.
	if err := os.Chmod(name, 0o644); err != nil {
		return fmt.Errorf("extra records file: %w", err)
	}
	if err := os.Rename(name, f.path); err != nil {
		return fmt.Errorf("extra records file: %w", err)
	}
	f.last, f.written = content, now
	return nil
}

// status gives the last write time and error, for /dns/state.
func (f *extraFile) status() (time.Time, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.written, f.err
}
