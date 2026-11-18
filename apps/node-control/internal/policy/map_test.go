package policy

import (
	"bytes"
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
