package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

const mapPolicy = `// secret-comment
{
	"groups": {"group:admins": ["alice@example.com"]},
	"acls": [
		{"action": "accept", "src": ["group:admins"], "dst": ["tag:web:443"]},
	],
}`

func (h *harness) setPolicy(text string) {
	h.api.mu.Lock()
	defer h.api.mu.Unlock()
	h.api.text = text
}

func TestMapOverHTTP(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.setPolicy(mapPolicy)
	rec := h.do("GET", "/policy/api/map", adminAddr, "", false)
	if rec.Code != 200 {
		t.Fatalf("map: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	m := jsonBody(t, rec)
	if m["updated_at"] != "v1" || m["truncated"] != false {
		t.Errorf("updated_at %v truncated %v", m["updated_at"], m["truncated"])
	}
	edges, _ := m["edges"].([]any)
	if len(edges) != 1 {
		t.Fatalf("edges %v", m["edges"])
	}
	e := edges[0].(map[string]any)
	refs := e["refs"].([]any)
	ref := refs[0].(map[string]any)
	if e["from"] != "group:admins" || e["to"] != "tag:web" || e["ports"] != "443" || ref["line"] != float64(5) {
		t.Errorf("edge %v", e)
	}
	if strings.Contains(rec.Body.String(), "secret-comment") {
		t.Error("the answer has the policy text")
	}
}

func TestMapEmptyPolicyIsEmptyGraph(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.setPolicy("")
	rec := h.do("GET", "/policy/api/map", adminAddr, "", false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"nodes":[]`) {
		t.Errorf("empty policy: %d %s", rec.Code, rec.Body)
	}
}

func TestMapSyntaxError422WithoutPolicyText(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.setPolicy(`{"acls": [ secret-text`)
	rec := h.do("GET", "/policy/api/map", adminAddr, "", false)
	if rec.Code != 422 || strings.Contains(rec.Body.String(), "secret-text") {
		t.Errorf("syntax error: %d %s", rec.Code, rec.Body)
	}
}

func TestMapTooLarge413(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_MAX_BYTES"] = "1024"
	h := newHarness(t, env)
	h.setPolicy(`{"acls": [], "x": "` + strings.Repeat("a", 2000) + `"}`)
	if rec := h.do("GET", "/policy/api/map", adminAddr, "", false); rec.Code != 413 {
		t.Errorf("too large: %d %s", rec.Code, rec.Body)
	}
}

func TestMapHeadscaleDown502(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.mu.Lock()
	h.api.getErr = errors.New("connection refused")
	h.api.mu.Unlock()
	if rec := h.do("GET", "/policy/api/map", adminAddr, "", false); rec.Code != 502 {
		t.Errorf("headscale down: %d %s", rec.Code, rec.Body)
	}
}

func TestMapNeverLogsPolicyText(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := newHarness(t, baseEnv(t))
	for _, text := range []string{mapPolicy, `{"acls": [ secret-text`, `{"acls":[{"action":"x","src":["secret-sel"]}]}`} {
		h.setPolicy(text)
		h.do("GET", "/policy/api/map", adminAddr, "", false)
	}
	for _, s := range []string{"secret-comment", "secret-text", "secret-sel"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("logs contain %q", s)
		}
	}
}

func mapPreviewBody(text string) string {
	b, _ := json.Marshal(map[string]string{"policy": text})
	return string(b)
}

func TestMapPreviewShowsNewPathsOfAGroupMember(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.setPolicy(mapPolicy)
	draft := strings.Replace(mapPolicy, `["alice@example.com"]`, `["alice@example.com", "bob@example.com"]`, 1)
	rec := h.post("/policy/api/map/preview", mapPreviewBody(draft))
	if rec.Code != 200 {
		t.Fatalf("map preview: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	m := jsonBody(t, rec)
	d := m["diff"].(map[string]any)
	if d["new_paths"] != float64(1) || d["removed_paths"] != float64(0) || len(d["added"].([]any)) != 0 {
		t.Errorf("diff %v", d)
	}
	members := d["members_added"].([]any)
	if len(members) != 1 || members[0].(map[string]any)["member"] != "bob@example.com" {
		t.Errorf("members_added %v", members)
	}
	if m["updated_at"] != "v1" || m["live_truncated"] != false || len(m["edges"].([]any)) != 1 {
		t.Errorf("answer %v", m)
	}
	if strings.Contains(rec.Body.String(), "secret-comment") {
		t.Error("the answer has the policy text")
	}
	h.api.mu.Lock()
	live := h.api.text
	h.api.mu.Unlock()
	if live != mapPolicy {
		t.Error("the map preview changed the policy")
	}
}

func TestMapPreviewShowsAnInternetRule(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.setPolicy(mapPolicy)
	draft := strings.Replace(mapPolicy, `"tag:web:443"`, `"autogroup:internet:*"`, 1)
	m := jsonBody(t, h.post("/policy/api/map/preview", mapPreviewBody(draft)))
	d := m["diff"].(map[string]any)
	added, removed := d["added"].([]any), d["removed"].([]any)
	if len(added) != 1 || added[0].(map[string]any)["to"] != "autogroup:internet" || len(removed) != 1 {
		t.Fatalf("added %v removed %v", added, removed)
	}
	if d["new_paths"] != float64(1) || d["removed_paths"] != float64(1) {
		t.Errorf("paths %v %v", d["new_paths"], d["removed_paths"])
	}
	nodes := d["removed_nodes"].([]any)
	if len(nodes) != 1 || nodes[0].(map[string]any)["id"] != "tag:web" {
		t.Errorf("removed_nodes %v", nodes)
	}
}

func TestMapPreviewRefusals(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_MAX_BYTES"] = "1024"
	h := newHarness(t, env)
	h.setPolicy(mapPolicy)
	big := `{"acls": [], "x": "` + strings.Repeat("a", 1100) + `"}`
	for _, tc := range []struct {
		body string
		code int
	}{
		{mapPreviewBody(big), 413},
		{`{"policy": "` + strings.Repeat("a", 4000) + `"}`, 413},
		{`{}`, 422},
		{mapPreviewBody("  "), 422},
		{mapPreviewBody(`{"acls": [ secret-text`), 422},
	} {
		if rec := h.post("/policy/api/map/preview", tc.body); rec.Code != tc.code || strings.Contains(rec.Body.String(), "secret-text") {
			t.Errorf("%.40s: %d %s", tc.body, rec.Code, rec.Body)
		}
	}
	h.setPolicy(big)
	if rec := h.post("/policy/api/map/preview", mapPreviewBody(mapPolicy)); rec.Code != 413 {
		t.Errorf("live too large: %d %s", rec.Code, rec.Body)
	}
	h.setPolicy(`{"acls": [ secret-text`)
	if rec := h.post("/policy/api/map/preview", mapPreviewBody(mapPolicy)); rec.Code != 422 || strings.Contains(rec.Body.String(), "secret-text") {
		t.Errorf("live syntax error: %d %s", rec.Code, rec.Body)
	}
	h.api.mu.Lock()
	h.api.getErr = errors.New("connection refused")
	h.api.mu.Unlock()
	if rec := h.post("/policy/api/map/preview", mapPreviewBody(mapPolicy)); rec.Code != 502 {
		t.Errorf("headscale down: %d %s", rec.Code, rec.Body)
	}
}

func TestMapPreviewNeverLogsPolicyText(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	h := newHarness(t, baseEnv(t))
	h.setPolicy(mapPolicy)
	for _, text := range []string{mapPolicy, `{"acls": [ secret-text`, `{"acls":[{"action":"x","src":["secret-sel"]}]}`} {
		h.post("/policy/api/map/preview", mapPreviewBody(text))
	}
	h.api.mu.Lock()
	h.api.getErr = errors.New("connection refused")
	h.api.mu.Unlock()
	h.post("/policy/api/map/preview", mapPreviewBody(mapPolicy))
	for _, s := range []string{"secret-comment", "secret-text", "secret-sel"} {
		if strings.Contains(logs.String(), s) {
			t.Errorf("logs contain %q", s)
		}
	}
}
