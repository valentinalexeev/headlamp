package controlsync

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const sampleBody = `{
  "version": "v1",
  "public_domain": "example.com",
  "namespaces": {"pdl": {"ports": [{"lo": 8080, "hi": 8080}, {"lo": 9000, "hi": 9010}]}},
  "routes": [{"ns": "pdl", "service": "docs", "target": "pdl-1", "port": 8080, "scheme": "https", "insecure_skip_verify": true}],
  "blocked": [{"ns": "pdl", "service": "admin"}]
}`

func TestFileSourceParsesAndVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "funnel.json")
	if err := os.WriteFile(path, []byte(sampleBody), 0o600); err != nil {
		t.Fatal(err)
	}
	src := NewFileSource(path)
	cfg, changed, err := src.Fetch(context.Background(), "")
	if err != nil || !changed {
		t.Fatalf("first fetch: %v %v", changed, err)
	}
	if len(cfg.Version) != 64 || cfg.Version == "v1" {
		t.Fatalf("the version must be the SHA-256 of the file, got %q", cfg.Version)
	}
	if cfg.PublicDomain != "example.com" || len(cfg.Namespaces["pdl"]) != 2 || cfg.Namespaces["pdl"][1].Hi != 9010 ||
		len(cfg.Routes) != 1 || cfg.Routes[0].Scheme != "https" || !cfg.Routes[0].InsecureSkipVerify || len(cfg.Blocked) != 1 {
		t.Fatalf("parsed: %+v", cfg)
	}
	if _, changed, err := src.Fetch(context.Background(), cfg.Version); err != nil || changed {
		t.Fatalf("unchanged etag: changed=%v err=%v", changed, err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(sampleBody, "example.com", "example.org", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, changed, err := src.Fetch(context.Background(), cfg.Version)
	if err != nil || !changed || cfg2.Version == cfg.Version || cfg2.PublicDomain != "example.org" {
		t.Fatalf("after edit: %+v %v %v", cfg2, changed, err)
	}
}

func TestFileSourceErrors(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := NewFileSource(filepath.Join(dir, "missing.json")).Fetch(context.Background(), ""); err == nil {
		t.Error("missing file accepted")
	}
	bad := filepath.Join(dir, "bad.json")
	os.WriteFile(bad, []byte("{"), 0o600)
	if _, _, err := NewFileSource(bad).Fetch(context.Background(), ""); err == nil {
		t.Error("bad JSON accepted")
	}
	big := filepath.Join(dir, "big.json")
	os.WriteFile(big, []byte(`{"public_domain":"`+strings.Repeat("a", MaxBody)+`"}`), 0o600)
	if _, _, err := NewFileSource(big).Fetch(context.Background(), ""); err == nil {
		t.Error("oversize file accepted")
	}
}

func TestNodeControlSourceSendsETagAndHandles304(t *testing.T) {
	var gotETag, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotETag, gotPath = r.Header.Get("If-None-Match"), r.URL.Path
		if gotETag == "v1" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		w.Write([]byte(sampleBody))
	}))
	defer srv.Close()
	src := NewNodeControlSource(srv.URL+"/", srv.Client())
	cfg, changed, err := src.Fetch(context.Background(), "")
	if err != nil || !changed || cfg.Version != "v1" || gotPath != "/ingress/v1/config" || gotETag != "" {
		t.Fatalf("first: %+v %v %v path=%q etag=%q", cfg, changed, err, gotPath, gotETag)
	}
	if _, changed, err := src.Fetch(context.Background(), "v1"); err != nil || changed || gotETag != "v1" {
		t.Fatalf("second: changed=%v err=%v etag=%q", changed, err, gotETag)
	}
}

func TestNodeControlSourceVersionFromETagWhenBodyHasNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"abc"`)
		w.Write([]byte(`{"public_domain":"example.com"}`))
	}))
	defer srv.Close()
	cfg, _, err := NewNodeControlSource(srv.URL, srv.Client()).Fetch(context.Background(), "")
	if err != nil || cfg.Version != "abc" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

func TestNodeControlSourceRejectsOversizeAndBadJSON(t *testing.T) {
	cases := map[string]func(w http.ResponseWriter){
		"oversize": func(w http.ResponseWriter) {
			w.Write([]byte(`{"version":"v","public_domain":"` + strings.Repeat("a", MaxBody) + `"}`))
		},
		"bad json":   func(w http.ResponseWriter) { w.Write([]byte(`<html>`)) },
		"no version": func(w http.ResponseWriter) { w.Write([]byte(`{"public_domain":"example.com"}`)) },
		"500":        func(w http.ResponseWriter) { w.WriteHeader(500) },
		"403":        func(w http.ResponseWriter) { w.WriteHeader(403) },
		"wrong type": func(w http.ResponseWriter) { w.Write([]byte(`{"version":"v","namespaces":[1]}`)) },
	}
	for name, h := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { h(w) }))
			defer srv.Close()
			cfg, changed, err := NewNodeControlSource(srv.URL, srv.Client()).Fetch(context.Background(), "")
			if err == nil || changed || cfg.Version != "" || cfg.PublicDomain != "" {
				t.Fatalf("accepted: %+v %v %v", cfg, changed, err)
			}
		})
	}
}

func TestNodeControlSourceUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	if _, _, err := NewNodeControlSource(url, http.DefaultClient).Fetch(context.Background(), ""); err == nil {
		t.Fatal("unreachable source reported success")
	}
}
