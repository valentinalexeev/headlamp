package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

const resourceURL = "https://node-control.example.com/mcp"

// fakeTailnet reports one fixed peer for every remote address.
type fakeTailnet struct {
	mu    sync.Mutex
	login string
	tags  []string
}

func (f *fakeTailnet) set(login string, tags ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.login, f.tags = login, tags
}

func (f *fakeTailnet) ListenAdmin() (net.Listener, error)  { return nil, nil }
func (f *fakeTailnet) ListenPublic() (net.Listener, error) { return nil, nil }
func (f *fakeTailnet) Dial(context.Context, string, string) (net.Conn, error) {
	return nil, nil
}
func (f *fakeTailnet) WhoIs(context.Context, string) (string, []string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.login, f.tags, nil
}

type rig struct {
	tn     *fakeTailnet
	admin  *httptest.Server
	public *httptest.Server
	issuer *oidctest.Issuer
	mu     sync.Mutex
	actors []string // who the node routes saw, by WhoIs
}

func (r *rig) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.actors...)
}

// newRig builds an admin router with a few real route patterns, the MCP module and the real
// Authorizer.
func newRig(t *testing.T, env map[string]string, cfg authz.Config) *rig {
	t.Helper()
	p, err := authz.NewPolicy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	rg := &rig{tn: &fakeTailnet{}}
	if _, ok := env["MCP_OIDC_ISSUER"]; ok {
		rg.issuer = oidctest.New(t, "mcp-client", "")
		env["MCP_OIDC_ISSUER"] = rg.issuer.URL
	}
	tn := core.CallerAware(rg.tn)
	admin, public := core.NewMux(), http.NewServeMux()
	node := func(w http.ResponseWriter, r *http.Request) {
		login, _, _ := tn.WhoIs(r.Context(), r.RemoteAddr)
		rg.mu.Lock()
		rg.actors = append(rg.actors, login+" "+r.Method+" "+r.URL.Path)
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"`+r.PathValue("id")+`"}`)
	}
	admin.HandleFunc("GET /nodes.json", node)
	admin.HandleFunc("POST /nodes/{id}/stop", node)
	admin.HandleFunc("POST /nodes/{id}/start", node)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	m := New(core.MapEnv(env))
	routes := &core.Routes{Admin: admin, Public: public, Env: core.MapEnv(env), Tailnet: tn, Log: log}
	if err := m.Register(routes); err != nil {
		t.Fatal(err)
	}
	if err := admin.Seal(&authz.Authorizer{Policy: p, Tailnet: rg.tn, Log: log}); err != nil {
		t.Fatal(err)
	}
	rg.admin = httptest.NewServer(admin)
	rg.public = httptest.NewServer(public)
	t.Cleanup(rg.admin.Close)
	t.Cleanup(rg.public.Close)
	return rg
}

var grants = authz.Config{
	Roles: map[string][]string{"mcp-viewer": {"*.view", "mcp.admin"}},
	Grants: []authz.Grant{
		{Subject: "owner@", Roles: []string{"owner"}},
		{Subject: "owner@example.com", Roles: []string{"owner"}},
		{Subject: "viewer@", Roles: []string{"mcp-viewer"}},
		{Subject: "operator@", Roles: []string{"operator"}},
		{Subject: "group:ops", Roles: []string{"owner"}},
	},
}

type reply struct {
	status int
	header http.Header
	body   map[string]any
}

func post(t *testing.T, url, token, origin string, msg any) reply {
	t.Helper()
	b, _ := json.Marshal(msg)
	req, _ := http.NewRequest(http.MethodPost, url+"/mcp", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out := reply{status: resp.StatusCode, header: resp.Header}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 && raw[0] == '{' {
		if err := json.Unmarshal(raw, &out.body); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return out
}

func call(id int, method string, params any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}
}

func toolNames(t *testing.T, r reply) []string {
	t.Helper()
	res, _ := r.body["result"].(map[string]any)
	list, _ := res["tools"].([]any)
	var out []string
	for _, x := range list {
		out = append(out, x.(map[string]any)["name"].(string))
	}
	return out
}

func toolText(t *testing.T, r reply) (string, bool) {
	t.Helper()
	res, ok := r.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %v", r.body)
	}
	content := res["content"].([]any)[0].(map[string]any)
	return content["text"].(string), res["isError"].(bool)
}

func has(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
