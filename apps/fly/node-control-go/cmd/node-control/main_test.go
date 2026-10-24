package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/core"
)

// serve runs the built modules on a plain-TCP tailnet and returns it with a stop func.
func serve(t *testing.T, env core.Env, mods []core.Module) (*core.Local, func()) {
	t.Helper()
	tn := core.NewLocal("127.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- core.Run(ctx, mods, env, tn) }()
	var once bool
	stop := func() {
		if once {
			return
		}
		once = true
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	}
	t.Cleanup(stop)
	return tn, stop
}

func fetch(t *testing.T, url string) (int, string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func names(mods []core.Module) []string {
	var out []string
	for _, m := range mods {
		out = append(out, m.Name())
	}
	return out
}

func TestBuildModulesWithoutEnroll(t *testing.T) {
	env := core.MapEnv(map[string]string{"NODES_CONFIG": filepath.Join(t.TempDir(), "absent.json")})
	mods, err := buildModules(env, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(mods), ","); got != "tailnet,nodes,enroll" {
		t.Fatalf("module order %s", got)
	}
	tn, _ := serve(t, env, mods)
	base := "http://" + tn.AdminAddr()
	for _, p := range []string{"/", "/nodes.json", "/status", "/tailnet/status", "/healthz"} {
		if code, _ := fetch(t, base+p); code != 200 {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	if code, _ := fetch(t, base+"/enroll/devices"); code != 404 {
		t.Errorf("enrolment admin route present without ENROLL_DB: %d", code)
	}
	if tn.PublicListened() {
		t.Fatal("public listener opened without ENROLL_DB")
	}
}

func TestBuildModulesRejectsBrokenNodesJSON(t *testing.T) {
	p := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(p, []byte(`{"nodes": [1, 2`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := buildModules(core.MapEnv(map[string]string{"NODES_CONFIG": p}), http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), p) {
		t.Fatalf("want an error naming the file, got %v", err)
	}
}

func TestTailnetStatusSeesDiscoveredExitNodes(t *testing.T) {
	var fetches int
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/node" || r.Header.Get("Authorization") != "Bearer test-key" {
			http.Error(w, "unexpected", 400)
			return
		}
		fetches++
		io.WriteString(w, `{"nodes":[
		  {"id":"1","name":"node-a","online":true,"tags":["tag:exit","tag:cloud-fly"],"ipAddresses":["100.64.0.2"]},
		  {"id":"2","name":"laptop","online":true,"tags":["tag:user"],"ipAddresses":["100.64.0.9"]}]}`)
	}))
	defer hs.Close()

	env := core.MapEnv(map[string]string{
		"NODES_CONFIG":           filepath.Join(t.TempDir(), "absent.json"),
		"HEADSCALE_LOGIN_SERVER": hs.URL,
		"HEADSCALE_API_KEY":      "test-key",
	})
	mods, err := buildModules(env, hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := serve(t, env, mods)
	base := "http://" + tn.AdminAddr()

	_, body := fetch(t, base+"/tailnet/status")
	var st map[string]struct {
		Online bool `json:"online"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if e, ok := st["node-a"]; !ok || !e.Online {
		t.Fatalf("discovered exit node missing from tailnet status: %s", body)
	}
	if _, ok := st["laptop"]; ok {
		t.Fatalf("untracked node reported: %s", body)
	}
	_, nj := fetch(t, base+"/nodes.json")
	if !strings.Contains(nj, "node-a") {
		t.Fatalf("nodes.json lacks the discovered node: %s", nj)
	}
	if fetches != 1 {
		t.Fatalf("headscale fetched %d times; the tailnet and nodes modules must share one cached source", fetches)
	}
}

func TestCollapsedDuplicateKeepsTailnetLineAndCanonicalName(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"nodes":[
		  {"id":"1","name":"node-a","online":false,"tags":["tag:exit","tag:cloud-fly"]},
		  {"id":"2","name":"node-a-1","online":true,"tags":["tag:exit","tag:cloud-fly"],"ipAddresses":["100.64.0.2"]}]}`)
	}))
	defer hs.Close()
	env := core.MapEnv(map[string]string{
		"NODES_CONFIG":           filepath.Join(t.TempDir(), "absent.json"),
		"HEADSCALE_LOGIN_SERVER": hs.URL,
		"HEADSCALE_API_KEY":      "test-key",
	})
	mods, err := buildModules(env, hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := serve(t, env, mods)
	base := "http://" + tn.AdminAddr()

	_, body := fetch(t, base+"/tailnet/status")
	var st map[string]struct {
		Online bool `json:"online"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if e, ok := st["node-a"]; !ok || !e.Online {
		t.Fatalf("tailnet line lost for the collapsed duplicate: %s", body)
	}
	_, nj := fetch(t, base+"/nodes.json")
	if !strings.Contains(nj, `"tailnetName":"node-a"`) || strings.Contains(nj, "node-a-1") {
		t.Fatalf("nodes.json must carry the canonical name: %s", nj)
	}
}
