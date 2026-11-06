package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/enroll"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/nodes"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/overview"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policy"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/tailnet"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/tasks"
)

// scenario says which modules run, who the caller is and which problems are seeded.
type scenario struct {
	login     string // the WhoIs answer once the data is in place
	nodesOnly bool   // only the nodes module is switched on
	problems  bool   // seed the attention state
}

// scenarios: attention seeds one of every kind of problem and an acknowledged item, clear seeds none, minimal switches on the
// nodes module only, partial is clear for a caller who is in no *_ADMIN_USERS list.
var scenarios = map[string]scenario{
	"attention": {login: "alice", problems: true},
	"clear":     {login: "alice"},
	"minimal":   {login: "alice", nodesOnly: true},
	"partial":   {login: "bob"},
}

const (
	adminLogin = "alice"
	// The invented hosts of the agents that cannot be reached in the attention scenario.
	offlineBridgeHost      = "bridge-2.demo.example.net"
	intermittentBridgeHost = "bridge-4.demo.example.net"
	// The item the attention scenario leaves acknowledged, for 7 days.
	acknowledgedItemInDemo  = "devices-expiring"
	acknowledgedItemSeconds = 7 * 24 * 60 * 60
	// The tailnet ports the fakes answer on (the agent port and the task runner port).
	agentPort  = "9000"
	runnerPort = "9100"
)

// world is everything the console talks to, all local.
type world struct {
	sc  scenario
	dir string

	hs     *httptest.Server
	agent  *httptest.Server
	runner *httptest.Server
	redis  *miniredis.Miniredis

	wg sync.WaitGroup // background loops started by the seeding

	hsState *fakeHeadscale
	prov    *fakeProvider
	run     *fakeRunner
}

func newWorld(sc scenario) (*world, error) {
	dir, err := os.MkdirTemp("", "console-demo-")
	if err != nil {
		return nil, err
	}
	w := &world{sc: sc, dir: dir}
	w.hsState = newFakeHeadscale(sc)
	w.prov = newFakeProvider()
	w.run = newFakeRunner(sc)
	w.hs = httptest.NewServer(w.hsState)
	w.agent = httptest.NewServer(http.HandlerFunc(agentHandler))
	w.runner = httptest.NewServer(w.run)
	if !sc.nodesOnly {
		if w.redis, err = miniredis.Run(); err != nil {
			w.close()
			return nil, err
		}
	}
	return w, nil
}

func (w *world) close() {
	if w.hs != nil {
		w.hs.Close()
	}
	if w.agent != nil {
		w.agent.Close()
	}
	if w.runner != nil {
		w.runner.Close()
	}
	if w.redis != nil {
		w.redis.Close()
	}
	os.RemoveAll(w.dir)
}

// route maps an invented tailnet address to the local fake that stands in for it.
func (w *world) route(addr string) (string, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "", err
	}
	switch {
	case (host == offlineBridgeHost || host == intermittentBridgeHost) && w.sc.problems:
		return "", errors.New("demo: no route to host")
	case port == agentPort:
		return w.agent.Listener.Addr().String(), nil
	case port == runnerPort:
		return w.runner.Listener.Addr().String(), nil
	}
	return "", errors.New("demo: only the fakes can be reached")
}

// modules wires the same modules as cmd/node-control, in the same order, against the fakes.
func (w *world) modules() ([]core.Module, core.Env, error) {
	nodesFile := filepath.Join(w.dir, "nodes.json")
	if err := os.WriteFile(nodesFile, []byte(`{"title":"Nodes","subtitle":"demo data"}`), 0o600); err != nil {
		return nil, core.Env{}, err
	}
	cfgFile := filepath.Join(w.dir, "enroll.json")
	if err := os.WriteFile(cfgFile, []byte(enrollConfig), 0o600); err != nil {
		return nil, core.Env{}, err
	}
	m := map[string]string{
		"NODES_CONFIG":           nodesFile,
		"HEADSCALE_LOGIN_SERVER": w.hs.URL,
		"HEADSCALE_API_KEY":      "demo-key",
		"ENROLL_ALLOWED_HOSTS":   "127.0.0.1,localhost,[::1]",
		"TAILNET_HOSTNAME":       "console-demo",
	}
	if !w.sc.nodesOnly {
		stateKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
		for k, v := range map[string]string{
			"ENROLL_DB":              filepath.Join(w.dir, "enroll.db"),
			"ENROLL_CONFIG":          cfgFile,
			"ENROLL_SECRET_KEY":      "demo-enrol-key-0123456789",
			"AGENT_TOKEN_BRIDGE_1":   "demo-agent-token",
			"AGENT_TOKEN_BRIDGE_2":   "demo-agent-token",
			"AGENT_TOKEN_BRIDGE_3":   "demo-agent-token",
			"AGENT_TOKEN_BRIDGE_4":   "demo-agent-token",
			"POLICY_DB":              filepath.Join(w.dir, "policy.db"),
			"POLICY_ADMIN_USERS":     adminLogin,
			"POLICY_REVERT_SECONDS":  "900",
			"FUNNEL_DB":              filepath.Join(w.dir, "funnel.db"),
			"FUNNEL_PUBLIC_DOMAIN":   "funnel.example.net",
			"FUNNEL_KEY_USER":        "demo",
			"FUNNEL_TAG_OWNERS":      "demo@",
			"FUNNEL_ADMIN_USERS":     adminLogin,
			"REDIS_URL":              "redis://" + w.redis.Addr(),
			"NODE_CONTROL_STATE_KEY": stateKey,
			"TASKS_RUNNER_TOKEN":     "demo-runner-token",
			"TASKS_ADMIN_USERS":      adminLogin,
			"TASKS_POLL_SECONDS":     "1",
			"SESSIONS_TAG":           "tag:browser-session",
		} {
			m[k] = v
		}
	}
	env := core.MapEnv(m)

	cfg, err := nodes.LoadConfig(nodesFile)
	if err != nil {
		return nil, env, err
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	client := &headscale.Client{BaseURL: w.hs.URL, APIKey: "demo-key", HTTP: hc}
	src := headscale.Cached(client, 2*time.Second)
	providers := map[string]nodes.Provider{}
	for _, p := range nodes.Providers {
		providers[p] = w.prov
	}
	nm, ok := nodes.New(cfg, src, providers).(interface {
		core.Module
		Targets(context.Context) []nodes.Target
	})
	if !ok {
		return nil, env, errors.New("nodes module does not expose Targets")
	}
	tracked := func(ctx context.Context) map[string]bool { return nodes.TailnetNames(cfg, nm.Targets(ctx)) }
	if w.sc.nodesOnly {
		mods := []core.Module{tailnet.New(src, tracked), nm}
		others := slices.Clone(mods)
		return append(mods, overview.New(func() []core.Module { return others })), env, nil
	}
	pol := policy.New(env, client)
	mods := []core.Module{
		tailnet.New(src, tracked),
		nm,
		enroll.New(env, enroll.WithNodeSource(src)),
		pol,
		funnel.New(env, client, func() funnel.PolicyService {
			if p, ok := pol.(interface{ Service() *policy.Service }); ok {
				if svc := p.Service(); svc != nil {
					return svc
				}
			}
			return nil
		}),
		tasks.New(env, src),
	}
	others := slices.Clone(mods)
	return append(mods, overview.New(func() []core.Module { return others })), env, nil
}

// enrollConfig lists three configured bridges; another is discovered from the node list. bridge-4 runs on a machine
// with another name and is marked intermittent.
const enrollConfig = `{
  "profile_name": "tunnel",
  "link_host": "links.example.net",
  "ingresses": [
    {"name": "bridge-1", "host": "bridge-1.example.net", "public_key": "demo-public-key-1", "short_id": "a1b2c3d4",
     "agent_url": "http://bridge-1.demo.example.net:9000", "uuids": "base"},
    {"name": "bridge-2", "host": "bridge-2.example.net", "public_key": "demo-public-key-2", "short_id": "e5f6a7b8",
     "agent_url": "http://bridge-2.demo.example.net:9000", "uuids": "derived"},
    {"name": "bridge-4", "host": "bridge-4.example.net", "public_key": "demo-public-key-4", "short_id": "f1e2d3c4",
     "agent_url": "http://bridge-4.demo.example.net:9000", "uuids": "derived", "node": "vm-4", "intermittent": true}
  ]
}`

// ---- headscale ----

type fakeHeadscale struct {
	mu        sync.Mutex
	nodes     []map[string]any
	policy    string
	updatedAt time.Time
	nextKey   int
	keys      []map[string]any // pre-auth keys, newest last
}

const basePolicy = `{"acls":[{"action":"accept","src":["*"],"dst":["*:*"]}]}`

func newFakeHeadscale(sc scenario) *fakeHeadscale {
	now := time.Now().UTC()
	node := func(id int, name string, online bool, ip string, seen time.Duration, tags ...string) map[string]any {
		return map[string]any{
			"id": fmt.Sprint(id), "machineKey": fmt.Sprintf("mkey:%064d", id), "name": name, "givenName": name,
			"online": online, "lastSeen": now.Add(-seen).Format(time.RFC3339),
			"ipAddresses": []string{ip}, "tags": tags,
		}
	}
	exitB := node(2, "exit-b", true, "100.64.0.12", time.Minute, "tag:exit", "tag:cloud-fly")
	if sc.problems {
		// The provider reports it running, the tailnet does not see it.
		exitB["online"] = false
		exitB["lastSeen"] = now.Add(-38 * time.Minute).Format(time.RFC3339)
	}
	ns := []map[string]any{
		node(1, "exit-a", true, "100.64.0.11", time.Minute, "tag:exit", "tag:cloud-oci"),
		exitB,
		node(3, "exit-c", false, "100.64.0.13", 11*time.Hour, "tag:exit", "tag:cloud-yandex"),
		node(4, "bridge-1", true, "100.64.0.21", time.Minute, "tag:ingress"),
		node(5, "bridge-2", true, "100.64.0.22", time.Minute, "tag:ingress"),
		node(6, "bridge-3", true, "100.64.0.23", time.Minute, "tag:ingress"),
		node(8, "vm-4", true, "100.64.0.24", time.Minute, "tag:exit", "tag:cloud-oci"), // the machine of bridge-4
		// runner-1 also publishes into the two funnel namespaces, so manual routes can point at it.
		node(7, "runner-1", true, "100.64.0.31", time.Minute, "tag:task-runner", "tag:browser-session", "tag:funnel", "tag:ns-demo", "tag:ns-lab"),
	}
	return &fakeHeadscale{nodes: ns, policy: basePolicy, updatedAt: now.Add(-24 * time.Hour), nextKey: 1}
}

func (f *fakeHeadscale) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch r.Method + " " + r.URL.Path {
	case "GET /api/v1/node":
		reply(200, map[string]any{"nodes": f.nodes})
	case "GET /api/v1/policy":
		reply(200, map[string]any{"policy": f.policy, "updatedAt": f.updatedAt.Format(time.RFC3339Nano)})
	case "PUT /api/v1/policy":
		var in struct {
			Policy string `json:"policy"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil || !json.Valid([]byte(in.Policy)) {
			reply(400, map[string]any{"code": 3, "message": "invalid policy"})
			return
		}
		f.policy, f.updatedAt = in.Policy, time.Now().UTC()
		reply(200, map[string]any{"policy": f.policy, "updatedAt": f.updatedAt.Format(time.RFC3339Nano)})
	case "POST /api/v1/policy/check":
		reply(200, map[string]any{})
	case "GET /api/v1/user":
		reply(200, map[string]any{"users": []any{map[string]any{"id": "1", "name": "demo"}}})
	case "GET /api/v1/preauthkey":
		reply(200, map[string]any{"preAuthKeys": f.keys})
	case "POST /api/v1/preauthkey":
		var in struct {
			Reusable   bool     `json:"reusable"`
			Ephemeral  bool     `json:"ephemeral"`
			Expiration string   `json:"expiration"`
			ACLTags    []string `json:"aclTags"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in) != nil {
			reply(400, map[string]any{"code": 3, "message": "invalid key request"})
			return
		}
		k := map[string]any{"id": fmt.Sprint(f.nextKey), "key": fmt.Sprintf("demo-key-%04d-not-a-real-secret", f.nextKey),
			"user": map[string]any{"id": "1", "name": "demo"}, "reusable": in.Reusable, "ephemeral": in.Ephemeral,
			"used": false, "expiration": in.Expiration, "aclTags": in.ACLTags}
		f.nextKey++
		f.keys = append(f.keys, k)
		reply(200, map[string]any{"preAuthKey": k})
	case "POST /api/v1/preauthkey/expire":
		var in struct {
			ID string `json:"id"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
		for _, k := range f.keys {
			if k["id"] == in.ID {
				k["expiration"] = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
			}
		}
		reply(200, map[string]any{})
	default:
		reply(404, map[string]any{"code": 5, "message": "not found"})
	}
}

// ---- provider ----

// fakeProvider answers for every provider name; the status vocabulary is the real one per provider.
type fakeProvider struct {
	mu    sync.Mutex
	state map[string]bool // node id -> running
}

func newFakeProvider() *fakeProvider {
	return &fakeProvider{state: map[string]bool{"exit-a": true, "exit-b": true, "exit-c": false, "vm-4": true}}
}

func (p *fakeProvider) status(n nodes.Target) nodes.Status {
	running := p.state[n.ID]
	words := [2]string{"STOPPED", "RUNNING"}
	if n.Provider == "fly" {
		words = [2]string{"STOPPED", "STARTED"}
	}
	st := nodes.Status{"status": words[b2i(running)]}
	if n.Provider == "fly" {
		st["machine_id"] = "demo0001"
	}
	return st
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (p *fakeProvider) Status(_ context.Context, n nodes.Target) (nodes.Status, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status(n), nil
}

func (p *fakeProvider) set(n nodes.Target, running bool) (string, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev := p.status(n)["status"].(string)
	p.state[n.ID] = running
	return prev, p.status(n)["status"].(string), nil
}

func (p *fakeProvider) Start(_ context.Context, n nodes.Target) (string, string, error) {
	return p.set(n, true)
}

func (p *fakeProvider) Stop(_ context.Context, n nodes.Target) (string, string, error) {
	return p.set(n, false)
}

// ---- enrolment agent ----

type ingressInfo struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	SNI       string `json:"sni"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
}

// agentHandler stands in for every ingress agent: it takes credentials, reports no traffic and
// publishes the parameters of the discovered bridge (found by its tailnet address).
func agentHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/v1/credentials":
		_, _ = io.Copy(io.Discard, io.LimitReader(r.Body, 1<<20))
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/stats":
		_, _ = w.Write([]byte(`{}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/info":
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "100.64.0.23" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(ingressInfo{Name: "bridge-3", Host: "bridge-3.example.net", Port: 443,
			SNI: "bridge-3.example.net", PublicKey: "demo-public-key-3", ShortID: "c9d0e1f2"})
	default:
		http.NotFound(w, r)
	}
}

// ---- task runner ----

// fakeRunner is the runner on runner-1. A task whose params say demo_outcome=fail fails with a
// sign-in timeout; every other run succeeds at once. It also reports one browser session.
type fakeRunner struct {
	mu      sync.Mutex
	next    int
	outcome map[string]string // run id -> outcome
	session bool
	expires time.Time
}

func newFakeRunner(sc scenario) *fakeRunner {
	return &fakeRunner{outcome: map[string]string{}, session: sc.problems, expires: time.Now().Add(42 * time.Minute)}
}

func (f *fakeRunner) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1/health":
		reply(200, map[string]any{"busy": false})
	case r.Method == http.MethodPost && r.URL.Path == "/v1/runs":
		var in struct {
			Params map[string]any `json:"params"`
		}
		_ = json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in)
		f.next++
		id := fmt.Sprintf("demo-run-%d", f.next)
		f.outcome[id], _ = in.Params["demo_outcome"].(string)
		reply(201, map[string]any{"id": id})
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/runs/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/runs/")
		out, ok := f.outcome[id]
		if !ok {
			reply(404, map[string]any{})
			return
		}
		if out == "fail" {
			reply(200, map[string]any{"state": "failed", "error": "timeout waiting for the sign-in form on runner-1",
				"log_tail": "navigating\nwaiting for the sign-in form\ntimeout"})
			return
		}
		reply(200, map[string]any{"state": "succeeded", "result": map[string]any{"ok": true}})
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/runs/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		if !f.session {
			reply(200, map[string]any{"session": nil})
			return
		}
		reply(200, f.sessionJSON())
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sessions":
		f.session, f.expires = true, time.Now().Add(30*time.Minute)
		reply(201, f.sessionJSON()["session"])
	case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/v1/sessions/"):
		f.session = false
		w.WriteHeader(http.StatusNoContent)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRunner) sessionJSON() map[string]any {
	return map[string]any{"session": map[string]any{"id": "demo-session", "state": "running", "port": 6080,
		"token": "demo-session-token", "started_at": f.expires.Add(-18 * time.Minute).UTC().Format(time.RFC3339),
		"expires_at": f.expires.UTC().Format(time.RFC3339)}}
}

// ---- seeding ----

// call sends one JSON request to the console and returns the status and body.
func call(ctx context.Context, hc *http.Client, base, method, path string, body any) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, data, nil
}

// seed fills the modules through their own public APIs, the way an operator would.
func (w *world) seed(ctx context.Context, admin string, ingress *http.Client) error {
	if w.sc.nodesOnly {
		return nil
	}
	hc := &http.Client{Timeout: 20 * time.Second}
	must := func(what string, want int, method, path string, body any) ([]byte, error) {
		code, data, err := call(ctx, hc, admin, method, path, body)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", what, err)
		}
		if code != want {
			return nil, fmt.Errorf("%s: status %d", what, code)
		}
		return data, nil
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	keep(w.seedDevices(ctx, must))
	keep(w.seedBridges(must))
	keep(w.seedFunnel(must))
	keep(w.seedTasks(ctx, hc, admin, must))
	keep(w.seedFunnelReport(ctx, ingress, admin))
	keep(w.seedAcknowledged(must))
	return firstErr
}

// seedAcknowledged acknowledges one item in the attention scenario, for 7 days, so the collapsed section has a row.
func (w *world) seedAcknowledged(must mustFn) error {
	if !w.sc.problems {
		return nil
	}
	data, err := must("reading attention", 200, "GET", "/_attention.json", nil)
	if err != nil {
		return err
	}
	var att struct {
		Items []struct {
			ID       string `json:"id"`
			AckToken string `json:"ack_token"`
		} `json:"items"`
	}
	if err := json.Unmarshal(data, &att); err != nil {
		return fmt.Errorf("reading attention: %w", err)
	}
	for _, it := range att.Items {
		if it.ID == acknowledgedItemInDemo {
			_, err := must("acknowledging an item", 200, "POST", "/_attention/acknowledge",
				map[string]any{"id": it.ID, "ack_token": it.AckToken, "expires_in_seconds": acknowledgedItemSeconds})
			return err
		}
	}
	return errors.New(acknowledgedItemInDemo + " is not in the attention list")
}

type mustFn func(what string, want int, method, path string, body any) ([]byte, error)

func (w *world) seedDevices(_ context.Context, must mustFn) error {
	day := 24 * time.Hour
	far := time.Now().Add(180 * day).Unix()
	devices := []struct {
		name, owner string
		expires     int64
	}{
		{"phone-1", "alice", far},
		{"tablet-2", "alice", far},
		{"laptop-3", "bob", far},
		{"phone-4", "bob", far},
		{"tv-5", "carol", far},
		{"phone-6", "carol", far},
	}
	if w.sc.problems {
		// Two devices that expire within seven days, the nearest in four.
		devices[3].expires = time.Now().Add(4 * day).Unix()
		devices[5].expires = time.Now().Add(6 * day).Unix()
	}
	for _, d := range devices {
		if _, err := must("creating device "+d.name, 200, "POST", "/enroll/devices",
			map[string]any{"name": d.name, "owner": d.owner, "client": "sing-box", "expires_at": d.expires}); err != nil {
			return err
		}
	}
	return nil
}

// seedBridges approves the discovered bridge unless the scenario wants it waiting.
func (w *world) seedBridges(must mustFn) error {
	if w.sc.problems {
		return nil
	}
	data, err := must("listing bridges", 200, "GET", "/enroll/ingresses", nil)
	if err != nil {
		return err
	}
	var out struct {
		Ingresses []enroll.IngressView `json:"ingresses"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return fmt.Errorf("listing bridges: %w", err)
	}
	for _, v := range out.Ingresses {
		if v.Name == "bridge-3" && v.Fingerprint != "" {
			_, err := must("approving bridge-3", 200, "POST", "/enroll/ingresses/bridge-3/approve",
				map[string]any{"fingerprint": v.Fingerprint, "node_id": v.NodeID})
			return err
		}
	}
	return errors.New("bridge-3 was not discovered")
}

// seedFunnel creates the two namespaces through the funnel API, the way an operator would. In the clear scenario
// the policy changes they cause are confirmed; in the attention scenario demo is left waiting for its confirm
// (it reverts by itself after 15 minutes), which is also the pending policy change that scenario shows. It
// then adds a manual route, a block and a few publisher keys.
func (w *world) seedFunnel(must mustFn) error {
	create := func(name string, lo, hi int, note string, confirm bool) error {
		data, err := must("creating namespace "+name, 202, "POST", "/funnel/namespaces",
			map[string]any{"name": name, "ports": []map[string]int{{"lo": lo, "hi": hi}}, "note": note})
		if err != nil {
			return err
		}
		if !confirm {
			return nil
		}
		var out struct {
			ChangeID int64 `json:"change_id"`
		}
		if json.Unmarshal(data, &out) != nil || out.ChangeID < 1 {
			return errors.New("creating namespace " + name + ": no change id")
		}
		_, err = must("confirming the policy change for "+name, 204, "POST", "/policy/confirm", map[string]any{"change_id": out.ChangeID})
		return err
	}
	// The policy console allows one pending change at a time, so the namespace that stays waiting goes last.
	if err := create("lab", 9200, 9210, "Experiments", true); err != nil {
		return err
	}
	if err := create("demo", 9000, 9010, "Showcase", !w.sc.problems); err != nil {
		return err
	}
	if _, err := must("adding a manual route", 204, "POST", "/funnel/routes",
		map[string]any{"ns": "lab", "service": "grafana", "target": "runner-1", "port": 9202, "scheme": "http"}); err != nil {
		return err
	}
	if _, err := must("blocking a name", 204, "POST", "/funnel/blocks",
		map[string]any{"ns": "lab", "service": "test", "reason": "Under repair"}); err != nil {
		return err
	}
	for _, k := range []map[string]any{{"reusable": true}, {"reusable": true, "ephemeral": true, "ttl_hours": 72}} {
		if _, err := must("minting a publisher key", 200, "POST", "/funnel/namespaces/lab/keys", k); err != nil {
			return err
		}
	}
	return nil
}

func (w *world) seedTasks(ctx context.Context, hc *http.Client, admin string, must mustFn) error {
	// The module opens its store in the background.
	if err := waitFor(ctx, 20*time.Second, func() bool {
		code, _, err := call(ctx, hc, admin, "GET", "/tasks.json", nil)
		return err == nil && code == 200
	}); err != nil {
		return errors.New("the tasks store did not open")
	}
	ids := []string{"balance-check", "cert-check", "backup-check", "dns-check", "uptime-check"}
	cfg := map[string]any{}
	scripts := map[string]string{}
	add := func(id string, params map[string]any) {
		cfg[id] = map[string]any{
			"script": id + ".js", "params": params, "target": map[string]any{"node": "runner-1"},
			"schedule": map[string]any{"every": "24h"}, "timeout": "1m", "retry": map[string]any{"attempts": 1},
		}
		scripts[id+".js"] = "// demo script, never run: the runner is a fake\n"
	}
	for _, id := range ids {
		add(id, map[string]any{})
	}
	failing := w.sc.problems
	if failing {
		add("login-check", map[string]any{"demo_outcome": "fail"})
	} else {
		add("login-check", map[string]any{})
	}
	if _, err := must("storing the task config", 200, "PUT", "/tasks/config", map[string]any{"config": map[string]any{"tasks": cfg}, "scripts": scripts}); err != nil {
		return err
	}
	// Every task runs once at start-up; the failing one is run on until it has failed three times.
	return waitFor(ctx, 90*time.Second, func() bool {
		code, data, err := call(ctx, hc, admin, "GET", "/tasks.json", nil)
		if err != nil || code != 200 {
			return false
		}
		var list []struct {
			ID          string  `json:"id"`
			Running     bool    `json:"running"`
			LastSuccess *string `json:"last_success"`
			Streak      int     `json:"streak"`
		}
		if json.Unmarshal(data, &list) != nil || len(list) != len(ids)+1 {
			return false
		}
		done := true
		for _, t := range list {
			switch {
			case t.ID == "login-check" && failing:
				if t.Streak < 3 {
					done = false
					if !t.Running {
						_, _, _ = call(ctx, hc, admin, "POST", "/tasks/login-check/run", map[string]any{})
					}
				}
			case t.LastSuccess == nil:
				done = false
			}
		}
		return done
	})
}

func waitFor(ctx context.Context, d time.Duration, ok func() bool) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if ok() {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return errors.New("timed out")
}

// seedFunnelReport posts the ingress's state report once and then every 30 s, as the real ingress does, so
// the funnel reads as fresh for as long as the demo runs.
func (w *world) seedFunnelReport(ctx context.Context, hc *http.Client, admin string) error {
	// What the ingress reports: registered services owned by a node, one manual route (no owner) and a blocked
	// name. In the attention scenario one target is offline.
	route := func(ns, service string, port int, owner string, online bool) map[string]any {
		r := map[string]any{"ns": ns, "service": service, "target": "100.64.0.31", "port": port, "scheme": "http", "target_online": online}
		if owner != "" {
			r["owner_node_id"], r["owner_name"] = "7", owner
		}
		return r
	}
	routes := []map[string]any{
		route("demo", "status", 9000, "runner-1", true),
		route("demo", "docs", 9001, "runner-1", true),
		route("lab", "api", 9200, "runner-1", !w.sc.problems),
		route("lab", "grafana", 9202, "", true),
		route("lab", "test", 9205, "runner-1", true),
	}
	post := func() error {
		code, _, err := call(ctx, hc, admin, "POST", "/ingress/v1/state", map[string]any{
			"applied_version": "demo", "sent_at": time.Now().UTC().Format(time.RFC3339), "snapshot_loaded": true,
			"peer_count": 3, "routes": routes, "errors": []string{}})
		if err != nil {
			return err
		}
		if code != http.StatusNoContent {
			return fmt.Errorf("posting the ingress report: status %d", code)
		}
		return nil
	}
	if err := post(); err != nil {
		return err
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = post()
			}
		}
	}()
	return nil
}
