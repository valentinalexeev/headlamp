package tailnetdns

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// rigWithFile is a rig that writes the headscale extra records file into a temporary directory.
func rigWithFile(t *testing.T) (*rig, string) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "extra-records.json")
	if err := os.WriteFile(p, []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := newRigWith(t, func(t *testing.T) map[string]string {
		env := sqliteEnv(t)
		env["TAILNET_DNS_EXTRA_RECORDS_FILE"] = p
		return env
	})
	return r, p
}

func readEntries(t *testing.T, p string) []extraEntry {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var out []extraEntry
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
	return out
}

func inode(t *testing.T, p string) uint64 {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Sys().(*syscall.Stat_t).Ino
}

func TestExtraFileFollowsRecords(t *testing.T) {
	r, p := rigWithFile(t)
	for _, body := range []map[string]any{
		{"name": "proxy.example.com", "tag": "tag:llm-proxy"},
		{"name": "nas.example.com", "node": "nas"},
		{"name": "fixed.example.com", "addresses": []string{"100.64.0.7", "100.64.0.5"}},
	} {
		if c, b := r.do("POST", "/dns/records", ipOwner, body); c != http.StatusCreated {
			t.Fatalf("%v: %d %s", body, c, b)
		}
	}
	got := readEntries(t, p)
	want := []extraEntry{
		{"fixed.example.com", "A", "100.64.0.5"}, {"fixed.example.com", "A", "100.64.0.7"},
		{"nas.example.com", "A", "100.64.0.30"}, {"nas.example.com", "AAAA", "fd7a:115c:a1e0::30"},
		{"proxy.example.com", "A", "100.64.0.41"}, // llm-b is offline
	}
	if len(got) != len(want) {
		t.Fatalf("entries: %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o644 {
		t.Errorf("mode %v: headscale must be able to read it", st.Mode().Perm())
	}
	// a node of the tag comes online: the file follows at the next refresh
	r.hs.mu.Lock()
	r.hs.nodes[2].Online = true
	r.hs.mu.Unlock()
	r.mod.Refresh(t.Context())
	if got := readEntries(t, p); len(got) != 6 || got[5].Value != "100.64.0.42" {
		t.Errorf("after llm-b came online: %+v", got)
	}
	// delete: the name leaves the file
	if c, _ := r.do("DELETE", "/dns/records/fixed.example.com", ipOwner, nil); c != http.StatusNoContent {
		t.Fatal(c)
	}
	for _, e := range readEntries(t, p) {
		if e.Name == "fixed.example.com" {
			t.Error("a deleted record stays in the file")
		}
	}
}

// An unchanged set does not touch the file: headscale reloads on every change of the file.
func TestExtraFileWrittenOnlyOnChange(t *testing.T) {
	r, p := rigWithFile(t)
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "nas.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatal(c)
	}
	ino, st0 := inode(t, p), func() time.Time { s, _ := os.Stat(p); return s.ModTime() }()
	r.mod.Refresh(t.Context())
	r.mod.Refresh(t.Context())
	if inode(t, p) != ino {
		t.Error("an unchanged set rewrote the file")
	}
	s, _ := os.Stat(p)
	if !s.ModTime().Equal(st0) {
		t.Error("an unchanged set touched the file")
	}
	// a change replaces the file by rename: a new inode, never a half-written file
	if c, _ := r.do("PUT", "/dns/records/nas.example.com", ipOwner, map[string]any{"addresses": []string{"100.64.0.9"}}); c != http.StatusOK {
		t.Fatal(c)
	}
	if inode(t, p) == ino {
		t.Error("a change must replace the file by rename")
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".*.tmp"))
	if len(left) != 0 {
		t.Errorf("temporary files left: %v", left)
	}
}

// A headscale failure keeps the file: a lost source must never remove the records from every device.
func TestExtraFileKeptOnHeadscaleFailure(t *testing.T) {
	r, p := rigWithFile(t)
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "nas.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatal(c)
	}
	before, _ := os.ReadFile(p)
	r.hs.mu.Lock()
	r.hs.err = os.ErrDeadlineExceeded
	r.hs.mu.Unlock()
	r.mod.Refresh(t.Context())
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Errorf("file changed on a headscale error:\n%s\n%s", before, after)
	}
}

func TestExtraFileState(t *testing.T) {
	r, p := rigWithFile(t)
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "nas.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatal(c)
	}
	// headscale sends the written record back as an extra record
	r.mod.cur.Lock()
	r.mod.cur.extra = []string{"llm-proxy.example.com", "nas.example.com"}
	r.mod.cur.Unlock()
	_, b := r.do("GET", "/dns/state", ipViewer, nil)
	var st stateResp
	_ = json.Unmarshal(b, &st)
	if st.ExtraFile != p || st.ExtraFileWritten == nil || st.ExtraFileError != "" {
		t.Errorf("file state: %s", b)
	}
	if len(st.Split) != 0 {
		t.Errorf("no split line is needed with the file: %v", st.Split)
	}
	if len(st.Extra) != 1 || st.Extra[0] != "llm-proxy.example.com" {
		t.Errorf("a managed record is listed again as an extra record: %v", st.Extra)
	}
}

func TestExtraFileWriteError(t *testing.T) {
	r, p := rigWithFile(t)
	// the target is a directory: the rename fails, also for root
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		t.Fatal(err)
	}
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "nas.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatal(c)
	}
	_, b := r.do("GET", "/dns/state", ipViewer, nil)
	var st stateResp
	_ = json.Unmarshal(b, &st)
	if st.ExtraFileError == "" || !strings.Contains(st.Error, "extra records file") {
		t.Errorf("a write error is not shown: %s", b)
	}
	left, _ := filepath.Glob(filepath.Join(filepath.Dir(p), ".*.tmp"))
	if len(left) != 0 {
		t.Errorf("temporary files left after a failed write: %v", left)
	}
}

func TestCheckExtraPath(t *testing.T) {
	dir := t.TempDir()
	for p, want := range map[string]string{
		"relative.json":                          "absolute",
		filepath.Join(dir, "x.yaml"):             ".json",
		filepath.Join(dir, "missing", "x.json"):  "directory",
		filepath.Join(dir, "extra-records.json"): "",
	} {
		err := checkExtraPath(p)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s: %v", p, err)
		}
	}
}
