package serve

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

type fakeCP struct {
	version  atomic.Int64
	body     atomic.Value // string
	status   atomic.Int32
	reported atomic.Value // syncState
}

func (f *fakeCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/serve/v1/records":
		if s := f.status.Load(); s != 0 {
			w.WriteHeader(int(s))
			return
		}
		w.Write([]byte(f.body.Load().(string)))
	case "/serve/v1/state":
		var s syncState
		json.NewDecoder(r.Body).Decode(&s)
		f.reported.Store(s)
		w.WriteHeader(204)
	}
}

func syncer(t *testing.T, url string) (*Syncer, string) {
	p := filepath.Join(t.TempDir(), "extra-records.json")
	return &Syncer{URL: url, Path: p, BaseDomain: "vpn.example.com", Client: http.DefaultClient,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}, p
}

func TestSyncWritesHeadscaleFormatAndReports(t *testing.T) {
	cp := &fakeCP{}
	cp.body.Store(`{"version":4,"records":[
	  {"name":"grafana.cluster.vpn.example.com","type":"A","value":"100.64.0.10"},
	  {"name":"v6.cluster.vpn.example.com","type":"AAAA","value":"fd7a:115c:a1e0::5"}]}`)
	srv := httptest.NewServer(cp)
	defer srv.Close()
	s, p := syncer(t, srv.URL)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	var got []recordJSON
	b, _ := os.ReadFile(p)
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("not the JSON array headscale reads: %s", b)
	}
	if len(got) != 2 || got[0] != (recordJSON{"grafana.cluster.vpn.example.com", "A", "100.64.0.10"}) {
		t.Fatalf("%+v", got)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode %v", fi.Mode())
	}
	if st := cp.reported.Load().(syncState); st.Version != 4 || st.Error != "" {
		t.Fatalf("report %+v", st)
	}
}

// The agent is a second line of defence: it drops anything node-control should never have sent.
func TestSyncDropsRecordsOutsideBaseOrTailnet(t *testing.T) {
	cp := &fakeCP{}
	cp.body.Store(`{"version":1,"records":[
	  {"name":"ok.cluster.vpn.example.com","type":"A","value":"100.64.0.10"},
	  {"name":"bank.example.com","type":"A","value":"100.64.0.10"},
	  {"name":"vpn.example.com","type":"A","value":"100.64.0.10"},
	  {"name":"lan.cluster.vpn.example.com","type":"A","value":"192.168.1.1"},
	  {"name":"lie.cluster.vpn.example.com","type":"AAAA","value":"100.64.0.10"}]}`)
	srv := httptest.NewServer(cp)
	defer srv.Close()
	s, p := syncer(t, srv.URL)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	if strings.Count(string(b), `"name"`) != 1 || !strings.Contains(string(b), "ok.cluster") {
		t.Fatalf("wrote %s", b)
	}
}

// A failure keeps the last good file, so headscale keeps serving the old names, and the failure is reported.
func TestSyncFailureKeepsLastFile(t *testing.T) {
	cp := &fakeCP{}
	cp.body.Store(`{"version":1,"records":[{"name":"a.cluster.vpn.example.com","type":"A","value":"100.64.0.10"}]}`)
	srv := httptest.NewServer(cp)
	defer srv.Close()
	s, p := syncer(t, srv.URL)
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(p)
	cp.status.Store(403)
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("a 403 must be an error")
	}
	cp.status.Store(0)
	cp.body.Store(`not json`)
	if err := s.Once(context.Background()); err == nil {
		t.Fatal("bad JSON must be an error")
	}
	after, _ := os.ReadFile(p)
	if string(before) != string(after) {
		t.Fatal("a failed pull changed the file")
	}
	if st := cp.reported.Load().(syncState); st.Error == "" || st.Version != 1 {
		t.Fatalf("report %+v", st)
	}
	entries, _ := os.ReadDir(filepath.Dir(p))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestSyncSkipsUnchangedVersion(t *testing.T) {
	cp := &fakeCP{}
	cp.body.Store(`{"version":7,"records":[]}`)
	srv := httptest.NewServer(cp)
	defer srv.Close()
	s, p := syncer(t, srv.URL)
	s.Once(context.Background())
	os.Remove(p)
	s.Once(context.Background())
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("an unchanged version must not rewrite the file")
	}
}

func TestSyncRunNeedsConfig(t *testing.T) {
	if err := (&Syncer{}).Run(context.Background()); err == nil {
		t.Fatal("ran without config")
	}
}
