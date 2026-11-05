package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/enroll"
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
	if got := strings.Join(names(mods), ","); got != "tailnet,nodes,enroll,policy,funnel,tasks,overview" {
		t.Fatalf("module order %s", got)
	}
	tn, _ := serve(t, env, mods)
	base := "http://" + tn.AdminAddr()
	for _, p := range []string{"/", "/nodes", "/nodes.json", "/status", "/tailnet/status", "/healthz"} {
		if code, _ := fetch(t, base+p); code != 200 {
			t.Errorf("GET %s = %d", p, code)
		}
	}
	for _, p := range []string{"/funnel", "/funnel/state", "/ingress/v1/config"} {
		if code, _ := fetch(t, base+p); code != 404 {
			t.Errorf("funnel route %s present without FUNNEL_DB: %d", p, code)
		}
	}
	if code, _ := fetch(t, base+"/enroll/devices"); code != 404 {
		t.Errorf("enrolment admin route present without ENROLL_DB: %d", code)
	}
	if tn.PublicListened() {
		t.Fatal("public listener opened without ENROLL_DB")
	}
}

func TestBuildModulesIncludesTasks(t *testing.T) {
	env := core.MapEnv(map[string]string{"NODES_CONFIG": filepath.Join(t.TempDir(), "absent.json")})
	mods, err := buildModules(env, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if last := mods[len(mods)-1]; last.Name() != "overview" || mods[len(mods)-2].Name() != "tasks" || mods[len(mods)-3].Name() != "funnel" || mods[len(mods)-4].Name() != "policy" {
		t.Fatalf("module order %v", names(mods))
	}
	// Without its variables the module registers nothing and node-control serves as before.
	tn, _ := serve(t, env, mods)
	if code, _ := fetch(t, "http://"+tn.AdminAddr()+"/tasks"); code != 404 {
		t.Fatalf("tasks route present without its env: %d", code)
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

func navEnv(t *testing.T, withEnroll bool) core.Env {
	t.Helper()
	dir := t.TempDir()
	m := map[string]string{"NODES_CONFIG": filepath.Join(dir, "absent.json"), "ENROLL_ALLOWED_HOSTS": "admin.test"}
	if withEnroll {
		cfg := filepath.Join(dir, "ingress.json")
		if err := os.WriteFile(cfg, []byte(`{"host": "ingress.example.test", "public_key": "P", "short_id": "ab", "sni": "s.example.test"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		m["ENROLL_DB"] = filepath.Join(dir, "e.db")
		m["ENROLL_CONFIG"] = cfg
		m["ENROLL_SECRET_KEY"] = strings.Repeat("k", 32)
		m["AGENT_URL"] = "http://agent.test"
		m["AGENT_TOKEN"] = "agent-token-value"
	}
	return core.MapEnv(m)
}

func getHost(t *testing.T, addr, host, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest("GET", "http://"+addr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestTopBarWiring(t *testing.T) {
	t.Run("with ENROLL_DB both entries and the bar on both pages", func(t *testing.T) {
		env := navEnv(t, true)
		mods, err := buildModules(env, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		tn, _ := serve(t, env, mods)
		admin := tn.AdminAddr()

		_, nav := getHost(t, admin, "admin.test", "/_nav.json")
		var items []core.NavItem
		if err := json.Unmarshal([]byte(nav), &items); err != nil {
			t.Fatalf("%v: %s", err, nav)
		}
		if len(items) != 4 || items[3].Label != "Activity" || items[3].Path != "/activity" || items[3].Order != 900 || items[0].Label != "Overview" || items[0].Path != "/" || items[0].Order != 1 ||
			items[1].Label != "Nodes" || items[1].Path != "/nodes" || items[1].Order != 10 ||
			items[2].Label != "Devices" || items[2].Path != "/enroll" || items[2].Order != 20 {
			t.Fatalf("entries %+v", items)
		}
		for path, active := range map[string]string{"/": `href="/" aria-current="page"`, "/nodes": `href="/nodes" aria-current="page"`, "/enroll": `href="/enroll" aria-current="page"`, "/activity": `href="/activity" aria-current="page"`} {
			code, body := getHost(t, admin, "admin.test", path)
			if code != 200 || strings.Count(body, "<nav data-nc-nav") != 1 || !strings.Contains(body, active) {
				t.Errorf("GET %s = %d, bar/active missing", path, code)
			}
		}
		// JSON endpoints are untouched.
		if _, body := getHost(t, admin, "admin.test", "/nodes.json"); strings.Contains(body, "data-nc-nav") {
			t.Errorf("bar in nodes.json")
		}
		// The public listener never carries the bar.
		for _, p := range []string{"/e/not-a-token", "/sub/not-a-secret", "/", "/_nav.json"} {
			code, body := getHost(t, tn.PublicAddr(), "ingress.example.test", p)
			if strings.Contains(body, "data-nc-nav") || strings.Contains(body, "<nav") {
				t.Errorf("public %s (%d) carries the bar", p, code)
			}
			if p == "/_nav.json" && code != 404 {
				t.Errorf("public /_nav.json = %d, want 404", code)
			}
		}
		// The Host guard answers first.
		if code, body := getHost(t, admin, "evil.test", "/_nav.json"); code != 400 || strings.Contains(body, "Nodes") {
			t.Errorf("bad host: %d %s", code, body)
		}
		if code, body := getHost(t, admin, "evil.test", "/"); code != 400 || strings.Contains(body, "data-nc-nav") {
			t.Errorf("bad host page: %d", code)
		}
	})

	t.Run("without ENROLL_DB only Nodes and a compact header without sidebar", func(t *testing.T) {
		env := navEnv(t, false)
		mods, err := buildModules(env, http.DefaultClient)
		if err != nil {
			t.Fatal(err)
		}
		tn, _ := serve(t, env, mods)
		admin := tn.AdminAddr()
		_, nav := getHost(t, admin, "admin.test", "/_nav.json")
		var items []core.NavItem
		if err := json.Unmarshal([]byte(nav), &items); err != nil || len(items) != 1 || items[0].Label != "Nodes" {
			t.Fatalf("entries %v: %s", err, nav)
		}
		// With one page the front page leads straight to it.
		if code, body := getHost(t, admin, "admin.test", "/"); code != 200 || !strings.Contains(body, "data-nc-nav") ||
			strings.Contains(body, "<nav") || strings.Contains(body, "Enrol device") || !strings.Contains(body, `id="nodes"`) {
			t.Fatalf("compact header and the Nodes page expected with one entry: %d", code)
		}
	})
}

// The enrolment module gets the same cached headscale source as the other modules: it shows the
// online flag of a configured ingress and lists a tagged node it does not know as pending.
func TestEnrolmentUsesTheHeadscaleSource(t *testing.T) {
	var fetches atomic.Int32
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		io.WriteString(w, `{"nodes":[
		  {"id":"1","name":"ingress-1","givenName":"ingress-1","online":true,"tags":["tag:ingress"],"ipAddresses":["100.64.0.5"]},
		  {"id":"2","name":"ingress-2","givenName":"ingress-2","online":true,"tags":["tag:ingress"],"ipAddresses":["100.64.0.6"]}]}`)
	}))
	defer hs.Close()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ingress.json")
	os.WriteFile(cfg, []byte(`{"link_host":"links.example.com","ingresses":[
	  {"name":"ingress-1","host":"ingress-1.example.com","public_key":"P","short_id":"ab","uuids":"base"}]}`), 0o600)
	env := core.MapEnv(map[string]string{
		"NODES_CONFIG":           filepath.Join(dir, "absent.json"),
		"HEADSCALE_LOGIN_SERVER": hs.URL,
		"HEADSCALE_API_KEY":      "test-key",
		"ENROLL_DB":              filepath.Join(dir, "e.db"),
		"ENROLL_CONFIG":          cfg,
		"ENROLL_SECRET_KEY":      strings.Repeat("k", 32),
		"AGENT_URL":              "http://agent.test",
		"AGENT_TOKEN":            "agent-token-value",
		"ENROLL_ALLOWED_HOSTS":   "admin.test",
	})
	mods, err := buildModules(env, hs.Client())
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := serve(t, env, mods)
	code, body := getHost(t, tn.AdminAddr(), "admin.test", "/enroll/ingresses")
	if code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	var out struct {
		Ingresses []struct {
			Name     string `json:"name"`
			Approval string `json:"approval"`
			Online   *bool  `json:"online"`
		} `json:"ingresses"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("%v: %s", err, body)
	}
	if len(out.Ingresses) != 2 {
		t.Fatalf("%s", body)
	}
	if o := out.Ingresses[0]; o.Name != "ingress-1" || o.Approval != "configured" || o.Online == nil || !*o.Online {
		t.Fatalf("%+v", o)
	}
	// ingress-2 has no token variable: it is listed, never asked and never used
	if o := out.Ingresses[1]; o.Name != "ingress-2" || o.Approval != "no token" {
		t.Fatalf("%+v", o)
	}
	if fetches.Load() == 0 {
		t.Fatal("the headscale source was not used")
	}
}

// TestEnrolmentInRedisIsWiredEndToEnd boots the real module list on ENROLL_REDIS=1 with no ENROLL_DB at all, as the
// deployed app runs: the public listener must exist, the one-time import from an old SQLite file must happen, and the
// public handler must accept a subscription secret issued before the move. A unit test of the module alone passes a
// public mux in by hand and so cannot see a core that never creates one.
func TestEnrolmentInRedisIsWiredEndToEnd(t *testing.T) {
	mr := miniredis.RunT(t)
	dir := t.TempDir()
	cfg := filepath.Join(dir, "ingress.json")
	if err := os.WriteFile(cfg, []byte(`{"host": "ingress.example.test", "public_key": "P", "short_id": "ab", "sni": "s.example.test"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	key := strings.Repeat("k", 32)

	old := filepath.Join(dir, "old-enroll.db")
	src, err := enroll.OpenStore(old, []byte(key), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, secret, err := src.CreateDevice("phone", "alice", "clash", 4_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	src.Close()

	env := core.MapEnv(map[string]string{
		"NODES_CONFIG":           filepath.Join(dir, "absent.json"),
		"ENROLL_ALLOWED_HOSTS":   "admin.test",
		"ENROLL_CONFIG":          cfg,
		"ENROLL_SECRET_KEY":      key,
		"AGENT_URL":              "http://agent.test",
		"AGENT_TOKEN":            "agent-token-value",
		"ENROLL_REDIS":           "1",
		"ENROLL_IMPORT_FROM":     old,
		"REDIS_URL":              "redis://:s3cretpw@" + mr.Addr(),
		"NODE_CONTROL_STATE_KEY": "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
	})
	mods, err := buildModules(env, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := serve(t, env, mods)

	if tn.PublicAddr() == "" {
		t.Fatal("no public listener with ENROLL_REDIS=1")
	}
	code, body := getHost(t, tn.AdminAddr(), "admin.test", "/enroll/devices")
	if code != 200 || !strings.Contains(body, `"name":"phone"`) {
		t.Fatalf("admin list after the import: %d %s", code, body)
	}
	if code, _ := getHost(t, tn.PublicAddr(), "ingress.example.test", "/sub/"+secret); code != 200 {
		t.Fatalf("a subscription secret issued before the move was refused by the public handler: %d", code)
	}
	if mr.Exists("node-control:enroll:d:"+strconv.FormatInt(d.ID, 10)) == false {
		t.Fatal("the device is not in Redis")
	}
}

// A node action is recorded in the audit log that the enrolment store keeps; the nodes module reaches it through a
// function because the store opens only at Register.
func TestNodeActionsReachTheEnrolmentAuditLog(t *testing.T) {
	m := map[string]string{}
	base := navEnv(t, true)
	for _, k := range []string{"ENROLL_DB", "ENROLL_CONFIG", "ENROLL_SECRET_KEY", "AGENT_URL", "AGENT_TOKEN", "ENROLL_ALLOWED_HOSTS"} {
		m[k] = base.Getenv(k)
	}
	nodesFile := filepath.Join(t.TempDir(), "nodes.json")
	if err := os.WriteFile(nodesFile, []byte(`{"title":"t","nodes":{"plain":{"provider":"yandex"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m["NODES_CONFIG"] = nodesFile
	env := core.MapEnv(m)
	mods, err := buildModules(env, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	tn, _ := serve(t, env, mods)
	req, _ := http.NewRequest("POST", "http://"+tn.AdminAddr()+"/nodes/plain/stop", nil)
	req.Host = "admin.test"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("stop = %d", resp.StatusCode)
	}
	var log core.AuditLog
	for _, mod := range mods {
		if p, ok := mod.(interface{ AuditLog() core.AuditLog }); ok {
			log = p.AuditLog()
		}
	}
	if log == nil {
		t.Fatal("the enrolment module has no audit log")
	}
	got, err := log.AuditLog(10)
	if err != nil || len(got) != 1 || got[0].Action != "node.stop" || got[0].Detail != "plain: failed (not_configured)" || got[0].Actor != "local" {
		t.Fatalf("audit log = %+v, %v", got, err)
	}
}

func TestKeyViewers(t *testing.T) {
	env := func(m map[string]string) core.Env { return core.MapEnv(m) }
	if keyViewers(env(map[string]string{"TASKS_ADMIN_USERS": " , "})) != nil {
		t.Error("with no admin list there is nobody to limit it to")
	}
	has := keyViewers(env(map[string]string{"POLICY_ADMIN_USERS": "alice@", "FUNNEL_ADMIN_USERS": "bob@", "TASKS_ADMIN_USERS": "carol@"}))
	for login, want := range map[string]bool{"alice@": true, "bob@": true, "carol@": true, "dave@": false} {
		if has(login) != want {
			t.Errorf("%s: want %v", login, want)
		}
	}
}

// A deployment that runs the published image mounts nodes.json anywhere and points NODES_CONFIG at it (#97). Without
// the variable the image's default path is used, and a missing file there is not an error.
func TestNodesConfigFromAMountedPath(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "mounted-nodes.json")
	if err := os.WriteFile(p, []byte(`{"nodes": [1, 2`), 0o600); err != nil {
		t.Fatal(err)
	}
	// A broken file at the mounted path proves that path is the one read.
	if _, err := buildModules(core.MapEnv(map[string]string{"NODES_CONFIG": p}), http.DefaultClient); err == nil ||
		!strings.Contains(err.Error(), p) {
		t.Fatalf("NODES_CONFIG not read: %v", err)
	}
	if defaultNodesConfig != "/srv/app/nodes.json" {
		t.Fatalf("default path changed to %q: deployments that overlay app/ would break", defaultNodesConfig)
	}
	if _, err := os.Stat(defaultNodesConfig); err == nil {
		t.Skip("a real file exists at the default path on this machine")
	}
	if _, err := buildModules(core.MapEnv(nil), http.DefaultClient); err != nil {
		t.Fatalf("without NODES_CONFIG and without a file at the default path: %v", err)
	}
}
