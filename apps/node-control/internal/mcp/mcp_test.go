package mcp

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

func on() map[string]string { return map[string]string{"MCP_ENABLED": "1"} }

func withOIDC(public bool) map[string]string {
	env := on()
	env["MCP_OIDC_ISSUER"] = "" // newRig puts the fake issuer's URL here
	env["MCP_RESOURCE_URL"] = resourceURL
	env["MCP_OIDC_GROUPS_CLAIM"] = "groups"
	if public {
		env["MCP_PUBLIC"] = "1"
	}
	return env
}

func TestTailnetOwnerRunsAToolAsThemselves(t *testing.T) {
	rg := newRig(t, on(), grants)
	rg.tn.set("Owner@")
	r := post(t, rg.admin.URL, "", "", call(1, "initialize",
		map[string]any{"protocolVersion": "2025-06-18"}))
	if r.status != 200 || r.body["result"].(map[string]any)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize: %d %v", r.status, r.body)
	}
	if !has(toolNames(t, post(t, rg.admin.URL, "", "", call(2, "tools/list", nil))), "node_stop") {
		t.Fatal("the owner must see node_stop")
	}
	r = post(t, rg.admin.URL, "", "", call(3, "tools/call",
		map[string]any{"name": "node_stop", "arguments": map[string]any{"id": "n1"}}))
	if text, isErr := toolText(t, r); isErr || !strings.Contains(text, `"n1"`) {
		t.Fatalf("node_stop: %q %v", text, isErr)
	}
	if got := rg.seen(); len(got) != 1 || got[0] != "owner POST /nodes/n1/stop" {
		t.Fatalf("the route must see the caller: %v", got)
	}
}

func TestToolsFollowTheCallersPermissions(t *testing.T) {
	rg := newRig(t, on(), grants)
	rg.tn.set("viewer@")
	names := toolNames(t, post(t, rg.admin.URL, "", "", call(1, "tools/list", nil)))
	if !has(names, "nodes_list") || has(names, "node_stop") {
		t.Fatalf("a viewer sees read tools only: %v", names)
	}
	r := post(t, rg.admin.URL, "", "", call(2, "tools/call",
		map[string]any{"name": "node_stop", "arguments": map[string]any{"id": "n1"}}))
	if r.body["error"] == nil || len(rg.seen()) != 0 {
		t.Fatalf("a viewer must not stop a node: %v %v", r.body, rg.seen())
	}
}

func TestOnlyMCPAdminsConnect(t *testing.T) {
	rg := newRig(t, withOIDC(false), grants)
	rg.tn.set("operator@") // an operator holds no mcp.admin
	if r := post(t, rg.admin.URL, "", "", call(1, "ping", nil)); r.status != http.StatusForbidden {
		t.Fatalf("operator: %d, want 403", r.status)
	}
	rg.tn.set("", "tag:ingress") // a tagged node must bring a token
	r := post(t, rg.admin.URL, "", "", call(1, "ping", nil))
	want := `Bearer resource_metadata="https://node-control.example.com/.well-known/oauth-protected-resource/mcp"`
	if r.status != http.StatusUnauthorized || r.header.Get("WWW-Authenticate") != want {
		t.Fatalf("tagged: %d %q", r.status, r.header.Get("WWW-Authenticate"))
	}
	rg.tn.set("owner@")
	if r := post(t, rg.admin.URL, "", "https://evil.example.net", call(1, "ping", nil)); r.status != 403 {
		t.Fatalf("cross-origin: %d, want 403", r.status)
	}
}

func TestOIDCTokenNamesTheCaller(t *testing.T) {
	rg := newRig(t, withOIDC(true), grants)
	rg.tn.set("", "tag:ingress")
	rg.issuer.SetUser(oidctest.User{Subject: "s1", Email: "Owner@Example.com"})
	for _, url := range []string{rg.admin.URL, rg.public.URL} {
		r := post(t, url, rg.issuer.AccessToken(resourceURL, nil), "", call(1, "tools/call",
			map[string]any{"name": "node_start", "arguments": map[string]any{"id": "n2"}}))
		if _, isErr := toolText(t, r); isErr {
			t.Fatalf("%s: %v", url, r.body)
		}
	}
	if got := rg.seen(); len(got) != 2 || got[0] != "owner@example.com POST /nodes/n2/start" {
		t.Fatalf("actors: %v", got)
	}
	// The public listener has no tailnet identity to fall back on.
	if r := post(t, rg.public.URL, "", "", call(1, "ping", nil)); r.status != 401 {
		t.Fatalf("public without a token: %d", r.status)
	}
}

func TestOIDCGroupGrantCounts(t *testing.T) {
	rg := newRig(t, withOIDC(false), grants)
	rg.issuer.SetUser(oidctest.User{Subject: "s2", Email: "new@example.com", Groups: []string{"ops"}})
	r := post(t, rg.admin.URL, rg.issuer.AccessToken(resourceURL, nil), "", call(1, "tools/call",
		map[string]any{"name": "node_stop", "arguments": map[string]any{"id": "n3"}}))
	if _, isErr := toolText(t, r); isErr {
		t.Fatalf("a group owner must stop a node: %v", r.body)
	}
}

func TestBadTokensAreRefused(t *testing.T) {
	rg := newRig(t, withOIDC(false), grants)
	rg.tn.set("owner@") // a bad token never falls back to the tailnet login
	rg.issuer.SetUser(oidctest.User{Subject: "s1", Email: "owner@example.com"})
	cases := map[string]string{
		"wrong audience": rg.issuer.AccessToken("https://other.example.com/mcp", nil),
		"unverified email": rg.issuer.AccessToken(resourceURL, func(c jwt.MapClaims) {
			c["email_verified"] = false
		}),
		"expired": rg.issuer.AccessToken(resourceURL, func(c jwt.MapClaims) {
			c["exp"] = c["iat"].(int64) - 600
		}),
		"garbage": "not-a-jwt",
	}
	for name, token := range cases {
		r := post(t, rg.admin.URL, token, "", call(1, "ping", nil))
		if r.status != 401 || !strings.Contains(r.header.Get("WWW-Authenticate"), "invalid_token") {
			t.Errorf("%s: %d %q", name, r.status, r.header.Get("WWW-Authenticate"))
		}
	}
	rg.issuer.SetUser(oidctest.User{Subject: "s9", Email: "stranger@example.com"})
	if r := post(t, rg.admin.URL, rg.issuer.AccessToken(resourceURL, nil), "", call(1, "ping", nil)); r.status != 403 {
		t.Errorf("a valid token of a non-admin: %d, want 403", r.status)
	}
}

func TestBearerWithoutOIDCIsRefused(t *testing.T) {
	rg := newRig(t, on(), grants)
	rg.tn.set("owner@")
	if r := post(t, rg.admin.URL, "x.y.z", "", call(1, "ping", nil)); r.status != 401 {
		t.Fatalf("%d, want 401", r.status)
	}
}

func TestProtocolEdges(t *testing.T) {
	rg := newRig(t, on(), grants)
	rg.tn.set("owner@")
	r := post(t, rg.admin.URL, "", "", map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if r.status != http.StatusAccepted {
		t.Fatalf("notification: %d", r.status)
	}
	r = post(t, rg.admin.URL, "", "", []any{call(1, "ping", nil)})
	if r.body["error"] == nil {
		t.Fatalf("a batch must be refused: %v", r.body)
	}
	r = post(t, rg.admin.URL, "", "", call(1, "resources/list", nil))
	if e, _ := r.body["error"].(map[string]any); e == nil || e["code"].(float64) != codeMethodNotFound {
		t.Fatalf("unknown method: %v", r.body)
	}
	r = post(t, rg.admin.URL, "", "", call(2, "tools/call",
		map[string]any{"name": "node_stop", "arguments": map[string]any{"id": "..", "x": 1}}))
	if _, isErr := toolText(t, r); !isErr || len(rg.seen()) != 0 {
		t.Fatalf("bad arguments must not reach the route: %v", r.body)
	}
	r = post(t, rg.admin.URL, "", "", call(3, "initialize", map[string]any{"protocolVersion": "1999-01-01"}))
	if r.body["result"].(map[string]any)["protocolVersion"] != protocolVersions[0] {
		t.Fatalf("an unknown version gets the newest: %v", r.body)
	}
}

func TestMetadata(t *testing.T) {
	rg := newRig(t, withOIDC(false), grants)
	resp, err := http.Get(rg.admin.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("metadata: %v %v", err, resp)
	}
	resp.Body.Close()
}

func TestOffByDefault(t *testing.T) {
	admin := core.NewMux()
	m := New(core.MapEnv(nil))
	if err := m.Register(&core.Routes{Admin: admin, Env: core.MapEnv(nil)}); err != nil {
		t.Fatal(err)
	}
	if len(admin.Patterns()) != 0 {
		t.Fatalf("without MCP_ENABLED nothing registers: %v", admin.Patterns())
	}
}

func TestSettingsErrors(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"bad flag":            {"MCP_ENABLED": "yes"},
		"public without oidc": {"MCP_ENABLED": "1", "MCP_PUBLIC": "1"},
		"issuer without resource": {"MCP_ENABLED": "1",
			"MCP_OIDC_ISSUER": "https://id.example.com"},
		"bad issuer": {"MCP_ENABLED": "1", "MCP_OIDC_ISSUER": "id.example.com",
			"MCP_RESOURCE_URL": resourceURL},
	} {
		if _, err := readSettings(core.MapEnv(env)); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	m := New(core.MapEnv(map[string]string{"MCP_ENABLED": "1", "MCP_PUBLIC": "1",
		"MCP_OIDC_ISSUER": "https://id.example.com", "MCP_RESOURCE_URL": resourceURL}))
	if err := m.Register(&core.Routes{Admin: core.NewMux()}); err == nil {
		t.Error("MCP_PUBLIC=1 without a public listener must fail")
	}
}

// Every tool names a real admin route that a permission checks, and no tool name repeats.
func TestToolCatalogue(t *testing.T) {
	var names []string
	for _, tl := range tools {
		rule, ok := authz.Routes[tl.route]
		if !ok || (rule.Kind != authz.KindPerm && rule.Kind != authz.KindAnyUser) {
			t.Errorf("%s: route %q has no permission check", tl.name, tl.route)
		}
		if slices.Contains(names, tl.name) {
			t.Errorf("%s: name used twice", tl.name)
		}
		names = append(names, tl.name)
		if tl.readOnly && tl.destructive {
			t.Errorf("%s: read only and destructive", tl.name)
		}
	}
	// Routes that return secrets or change who holds what stay out.
	for _, never := range []string{"GET /tasks/secrets", "PUT /tasks/secrets", "POST /enroll/invites",
		"POST /enroll/devices/{id}/links", "POST /enroll/devices/{id}/rotate-secret",
		"POST /funnel/namespaces/{name}/keys", "POST /serve/services/{id}/keys",
		"POST /admin/grants", "PUT /admin/roles/{name}", "POST /admin/import"} {
		for _, tl := range tools {
			if tl.route == never {
				t.Errorf("%s must not be a tool (%s)", tl.name, never)
			}
		}
	}
}

func FuzzParseMessage(f *testing.F) {
	f.Add([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	f.Add([]byte(`[{"jsonrpc":"2.0"}]`))
	f.Add([]byte(`{"jsonrpc":"2.0","id":{},"method":"x"}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		req, err := parseMessage(b)
		if err == nil && (req.JSONRPC != "2.0" || (len(req.ID) > 0 && !validID(req.ID))) {
			t.Fatalf("accepted an invalid message: %q", b)
		}
	})
}
