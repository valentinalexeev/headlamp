package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

const (
	adminAddr  = "100.64.0.1:5555"
	otherAddr  = "100.64.0.2:5555"
	taggedAddr = "100.64.0.3:5555"
	errAddr    = "100.64.0.4:5555"
)

const wantCSP = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

func fakeTailnet() *authz.FakeTailnet {
	return authz.NewFakeTailnet(map[string]authz.Who{
		"100.64.0.1": {Login: "alice@"},
		"100.64.0.2": {Login: "bob@"},
		"100.64.0.3": {Login: "alice@", Tags: []string{"tag:server"}},
		"100.64.0.4": {Err: errors.New("whois down")},
	})
}

func baseEnv(t *testing.T) map[string]string {
	return map[string]string{
		"POLICY_DB":              filepath.Join(t.TempDir(), "policy.db"),
		"POLICY_ADMIN_USERS":     "alice",
		"ENROLL_ALLOWED_HOSTS":   "node-control.example.com,*.example.net",
		"HEADSCALE_LOGIN_SERVER": "https://hs.example.com",
		"HEADSCALE_API_KEY":      "secret-key-value",
	}
}

type harness struct {
	mux *http.ServeMux
	mod core.Module
	api *fakeAPI
}

func register(t *testing.T, env map[string]string, api API, log *slog.Logger) (*http.ServeMux, core.Module, error) {
	t.Helper()
	mux := http.NewServeMux()
	if log == nil {
		log = slog.Default()
	}
	m := New(core.MapEnv(env), api)
	err := m.Register(&core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: fakeTailnet(), Log: log})
	return mux, m, err
}

func newHarness(t *testing.T, env map[string]string) *harness {
	t.Helper()
	api := newFakeAPI(`{"acls":[]}`)
	mux, m, err := register(t, env, api, nil)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	t.Cleanup(func() { m.(core.Stopper).Stop(context.Background()) })
	return &harness{mux: mux, mod: m, api: api}
}

func (h *harness) do(method, path, addr, body string, json bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = addr
	if json {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	h.mux.ServeHTTP(rec, req)
	return rec
}

func (h *harness) post(path, body string) *httptest.ResponseRecorder {
	return h.do("POST", path, adminAddr, body, true)
}

func (h *harness) confirm(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	p, _ := h.state(t)["pending"].(map[string]any)
	id, _ := p["id"].(float64)
	return h.post("/policy/confirm", `{"change_id":`+itoa(int64(id))+`}`)
}

func (h *harness) state(t *testing.T) map[string]any {
	t.Helper()
	rec := h.do("GET", "/policy/state", adminAddr, "", false)
	if rec.Code != 200 {
		t.Fatalf("state: %d %s", rec.Code, rec.Body)
	}
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func jsonBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body %q: %v", rec.Body, err)
	}
	return m
}

func applyBody(policy, base string) string {
	b, _ := json.Marshal(map[string]string{"policy": policy, "base_updated_at": base})
	return string(b)
}

func (h *harness) apply(t *testing.T, policy string) *httptest.ResponseRecorder {
	t.Helper()
	return h.post("/policy/apply", applyBody(policy, h.state(t)["updated_at"].(string)))
}

var allRoutes = []struct{ method, path string }{
	{"GET", "/policy"},
	{"GET", "/policy/static/policy.js"},
	{"GET", "/policy/static/policy.css"},
	{"GET", "/policy/state"},
	{"POST", "/policy/preview"},
	{"POST", "/policy/apply"},
	{"POST", "/policy/confirm"},
	{"POST", "/policy/revert/1"},
}

func TestDisabledWithoutPolicyDB(t *testing.T) {
	env := baseEnv(t)
	delete(env, "POLICY_DB")
	mux, m, err := register(t, env, newFakeAPI("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := m.(core.Stopper).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := &harness{mux: mux}
	for _, r := range allRoutes {
		if rec := h.do(r.method, r.path, adminAddr, "", true); rec.Code != 404 {
			t.Errorf("%s %s: %d, want 404", r.method, r.path, rec.Code)
		}
	}
}

func TestMissingAdminUsersStopsStartup(t *testing.T) {
	for _, v := range []string{"", " , ,"} {
		env := baseEnv(t)
		env["POLICY_ADMIN_USERS"] = v
		_, _, err := register(t, env, newFakeAPI("x"), nil)
		if err == nil || !strings.Contains(err.Error(), "POLICY_ADMIN_USERS") {
			t.Errorf("%q: err = %v", v, err)
		}
	}
}

func TestMissingHeadscaleSettingsStopsStartup(t *testing.T) {
	for _, k := range []string{"HEADSCALE_LOGIN_SERVER", "HEADSCALE_API_KEY"} {
		env := baseEnv(t)
		delete(env, k)
		_, _, err := register(t, env, newFakeAPI("x"), nil)
		if err == nil || !strings.Contains(err.Error(), k) {
			t.Errorf("%s: err = %v", k, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret-key-value") {
			t.Errorf("%s: error leaks the key", k)
		}
	}
}

func TestBadNumericEnvStopsStartup(t *testing.T) {
	cases := map[string][]string{
		"POLICY_REVERT_SECONDS": {"abc", "29", "901", "-5"},
		"POLICY_MAX_BYTES":      {"abc", "1023", "4194305"},
	}
	for k, vals := range cases {
		for _, v := range vals {
			env := baseEnv(t)
			env[k] = v
			_, _, err := register(t, env, newFakeAPI("x"), nil)
			if err == nil || !strings.Contains(err.Error(), k) {
				t.Errorf("%s=%s: err = %v", k, v, err)
			}
		}
	}
	env := baseEnv(t)
	env["POLICY_REVERT_SECONDS"], env["POLICY_MAX_BYTES"] = "30", "1024"
	if _, _, err := register(t, env, newFakeAPI("x"), nil); err != nil {
		t.Errorf("lower bounds should be accepted: %v", err)
	}
}

func TestEveryRoute403ForNonAdminTaggedAndWhoIsError(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, addr := range []string{otherAddr, taggedAddr, errAddr, "garbage"} {
		for _, r := range allRoutes {
			rec := h.do(r.method, r.path, addr, "", true)
			if rec.Code != 403 || rec.Body.Len() != 0 {
				t.Errorf("%s %s from %s: %d %q", r.method, r.path, addr, rec.Code, rec.Body)
			}
		}
	}
	if h.api.calls() != 0 {
		t.Errorf("headscale called %d times for refused requests", h.api.calls())
	}
}

func TestPostRequiresJSON(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, p := range []string{"/policy/preview", "/policy/apply", "/policy/confirm", "/policy/revert/1"} {
		rec := h.do("POST", p, adminAddr, "policy=x", false)
		if rec.Code != 422 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if h.api.setCount() != 0 {
		t.Error("a change was sent")
	}
}

func TestApplyConfirmFlowOverHTTP(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	rec := h.apply(t, `{"acls":[1]}`)
	if rec.Code != 200 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	b := jsonBody(t, rec)
	if b["change_id"] == nil {
		t.Errorf("no change_id: %v", b)
	}
	if _, err := time.Parse(time.RFC3339, b["deadline"].(string)); err != nil {
		t.Errorf("deadline: %v", err)
	}
	if h.api.current() != `{"acls":[1]}` {
		t.Errorf("headscale has %q", h.api.current())
	}
	if h.state(t)["pending"] == nil {
		t.Error("expected a pending change")
	}
	if rec := h.confirm(t); rec.Code != 204 {
		t.Fatalf("confirm: %d %s", rec.Code, rec.Body)
	}
	st := h.state(t)
	if st["pending"] != nil {
		t.Error("still pending after confirm")
	}
	if st["policy"] != `{"acls":[1]}` {
		t.Errorf("policy %v", st["policy"])
	}
}

func TestPreviewOverHTTP(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	rec := h.post("/policy/preview", `{"policy":"{\"acls\":[1]}"}`)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := jsonBody(t, rec)
	if b["valid"] != true || !strings.Contains(b["diff"].(string), "acls") {
		t.Errorf("preview %v", b)
	}
	h.api.checkErr = &headscale.APIError{Status: 400, Message: "bad acl"}
	b = jsonBody(t, h.post("/policy/preview", `{"policy":"x"}`))
	if b["valid"] != false || b["error"] != "bad acl" {
		t.Errorf("invalid preview %v", b)
	}
	if h.api.setCount() != 0 {
		t.Error("preview changed the policy")
	}
}

func TestApplyStaleBase409(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	rec := h.post("/policy/apply", applyBody("new", "v0"))
	if rec.Code != 409 || !strings.Contains(jsonBody(t, rec)["detail"].(string), "changed since") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h.api.setCount() != 0 {
		t.Error("stale apply reached headscale")
	}
}

func TestApplyWhilePending409(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	if rec := h.apply(t, "one"); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	rec := h.apply(t, "two")
	if rec.Code != 409 || !strings.Contains(jsonBody(t, rec)["detail"].(string), "pending") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestApplyTooLarge413(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_MAX_BYTES"] = "1024"
	h := newHarness(t, env)
	rec := h.post("/policy/apply", applyBody(strings.Repeat("a", 2*1024+8192+10), "v1"))
	if rec.Code != 413 || jsonBody(t, rec)["detail"] == nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h.api.calls() != 0 {
		t.Error("headscale called")
	}
}

func TestApplyOversizePolicy413(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_MAX_BYTES"] = "1024"
	h := newHarness(t, env)
	rec := h.post("/policy/apply", applyBody(strings.Repeat("a", 1025), "v1"))
	if rec.Code != 413 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h.api.setCount() != 0 {
		t.Error("oversize policy was sent")
	}
}

func TestApplyInvalidJSONBody422(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, body := range []string{"", "{", "[]", `{"policy": 5}`} {
		rec := h.post("/policy/apply", body)
		if rec.Code != 422 || jsonBody(t, rec)["detail"] == nil {
			t.Errorf("%q: %d %s", body, rec.Code, rec.Body)
		}
	}
	if rec := h.post("/policy/apply", `{"policy":"x","base_updated_at":"v1","extra":1}`); rec.Code != 200 {
		t.Errorf("unknown fields must be ignored: %d %s", rec.Code, rec.Body)
	}
}

func TestApplyRejectedByHeadscale422WithMessage(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.fail(&headscale.APIError{Status: 400, Message: "unknown group"})
	rec := h.apply(t, "bad")
	if rec.Code != 422 || jsonBody(t, rec)["detail"] != "unknown group" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h.state(t)["pending"] != nil {
		t.Error("rejected change left pending")
	}
}

func TestApplyUncertain202(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.fail(errors.New("connection reset"))
	h.api.breakGet = true
	base := h.state(t)["updated_at"].(string)
	rec := h.post("/policy/apply", applyBody("new", base))
	if rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b := jsonBody(t, rec)
	if b["uncertain"] != true || b["change_id"] == nil || b["deadline"] == nil || b["detail"] == nil {
		t.Errorf("body %v", b)
	}
}

func TestApplyUnreachable502(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.getErr = errors.New("dial tcp: refused")
	rec := h.post("/policy/apply", applyBody("x", "v1"))
	if rec.Code != 502 || jsonBody(t, rec)["detail"] != "headscale is unreachable or failed" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "refused") {
		t.Error("raw error leaked")
	}
}

func TestConfirmNothingPending409(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	if rec := h.post("/policy/confirm", `{"change_id":1}`); rec.Code != 409 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestConfirmRequiresChangeID(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.apply(t, "x")
	for _, b := range []string{"{}", `{"change_id":0}`, `{"change_id":-1}`, `{"change_id":"a"}`, ""} {
		if rec := h.post("/policy/confirm", b); rec.Code != 422 {
			t.Errorf("%q: %d", b, rec.Code)
		}
	}
	if h.state(t)["pending"] == nil {
		t.Error("confirmed without an id")
	}
}

func TestConfirmWrongIDAndLiveMismatchAre409(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.apply(t, "x")
	if rec := h.post("/policy/confirm", `{"change_id":99}`); rec.Code != 409 || !strings.Contains(rec.Body.String(), "not the pending") {
		t.Errorf("wrong id: %d %s", rec.Code, rec.Body)
	}
	h.api.mu.Lock()
	h.api.text = "other"
	h.api.mu.Unlock()
	if rec := h.confirm(t); rec.Code != 409 || !strings.Contains(rec.Body.String(), "live policy") {
		t.Errorf("mismatch: %d %s", rec.Code, rec.Body)
	}
}

func TestConfirmAfterUncertainAndNotApplied409(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.fail(errors.New("connection reset"))
	base := h.state(t)["updated_at"].(string)
	if rec := h.post("/policy/apply", applyBody("new", base)); rec.Code != 202 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := h.confirm(t); rec.Code != 409 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestApplyEmptyUpdatedAtRefusedOverHTTP(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.api.noVer = true
	rec := h.post("/policy/apply", applyBody("x", ""))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "bootstrap") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestStateShowsAttemptsAndOverdue(t *testing.T) {
	env := baseEnv(t)
	st, err := OpenStore(env["POLICY_DB"], nil)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.Begin("alice", "edit", "orig", "changed", time.Now().Add(-time.Minute))
	st.MarkReverted(id, false)
	st.MarkReverted(id, false)
	st.Close()
	h := newHarness(t, env)
	p := h.state(t)["pending"].(map[string]any)
	if p["attempts"].(float64) != 2 || p["overdue"] != true {
		t.Errorf("pending %v", p)
	}
	h2 := newHarness(t, baseEnv(t))
	h2.apply(t, "x")
	p = h2.state(t)["pending"].(map[string]any)
	if p["attempts"].(float64) != 0 || p["overdue"] != false {
		t.Errorf("fresh pending %v", p)
	}
}

func TestEmptyAllowedHostsStopsStartup(t *testing.T) {
	env := baseEnv(t)
	delete(env, "ENROLL_ALLOWED_HOSTS")
	_, _, err := register(t, env, newFakeAPI("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "ENROLL_ALLOWED_HOSTS") {
		t.Fatalf("err = %v", err)
	}
}

func TestWildcardAllowedHostsStopsStartup(t *testing.T) {
	env := baseEnv(t)
	env["ENROLL_ALLOWED_HOSTS"] = "node-control.example.com, *"
	_, _, err := register(t, env, newFakeAPI("x"), nil)
	if err == nil || !strings.Contains(err.Error(), "ENROLL_ALLOWED_HOSTS") {
		t.Fatalf("err = %v", err)
	}
}

func TestSuffixWildcardAccepted(t *testing.T) {
	env := baseEnv(t)
	env["ENROLL_ALLOWED_HOSTS"] = "*.example.com"
	if _, m, err := register(t, env, newFakeAPI("x"), nil); err != nil {
		t.Fatal(err)
	} else {
		m.(core.Stopper).Stop(context.Background())
	}
}

func TestBodyLimitAllowsEscapedPolicyAndRejectsBeyond(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_MAX_BYTES"] = "10000"
	h := newHarness(t, env)
	text := strings.Repeat("\"\n", 4000) // 8000 bytes, about 16000 once escaped
	if rec := h.apply(t, text); rec.Code != 200 {
		t.Fatalf("escaped policy: %d %s", rec.Code, rec.Body)
	}
	h.confirm(t)
	if rec := h.apply(t, strings.Repeat("a", 10001)); rec.Code != 413 {
		t.Fatalf("maxBytes+1: %d", rec.Code)
	}
	if rec := h.post("/policy/apply", applyBody(strings.Repeat("a", 2*10000+8192+10), "v1")); rec.Code != 413 {
		t.Fatalf("over the body limit: %d", rec.Code)
	}
}

func TestRevertFlow(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.apply(t, "second")
	h.confirm(t)
	id := int64(jsonBody(t, h.apply(t, "third"))["change_id"].(float64))
	h.confirm(t)
	rec := h.post("/policy/revert/"+itoa(id), "{}")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if h.api.current() != "second" {
		t.Errorf("policy %q", h.api.current())
	}
	if rec := h.post("/policy/revert/abc", "{}"); rec.Code != 422 {
		t.Errorf("bad id: %d", rec.Code)
	}
	h.confirm(t)
	if rec := h.post("/policy/revert/9999", "{}"); rec.Code != 404 {
		t.Errorf("unknown id: %d %s", rec.Code, rec.Body)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestStateNeverContainsPolicyTextInHistory(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	h.apply(t, "SECRET-POLICY-TEXT-ONE")
	h.confirm(t)
	st := h.state(t)
	raw, _ := json.Marshal(map[string]any{"history": st["history"], "pending": st["pending"], "last_edit": st["last_edit"]})
	if bytes.Contains(raw, []byte("SECRET-POLICY-TEXT")) || bytes.Contains(raw, []byte(`{\"acls`)) {
		t.Errorf("policy text in %s", raw)
	}
	hist := st["history"].([]any)
	if len(hist) != 1 {
		t.Fatalf("history %v", hist)
	}
	e := hist[0].(map[string]any)
	for _, k := range []string{"id", "at", "actor", "reason", "state", "deadline"} {
		if _, ok := e[k]; !ok {
			t.Errorf("history entry lacks %s: %v", k, e)
		}
	}
	if e["actor"] != "alice" || st["last_edit"] == nil {
		t.Errorf("entry %v last_edit %v", e, st["last_edit"])
	}
}

func TestStateShowsPendingSecondsLeft(t *testing.T) {
	env := baseEnv(t)
	env["POLICY_REVERT_SECONDS"] = "300"
	h := newHarness(t, env)
	if st := h.state(t); st["pending"] != nil || st["last_edit"] != nil {
		t.Errorf("fresh state %v", st)
	}
	h.apply(t, "x")
	p := h.state(t)["pending"].(map[string]any)
	left := p["seconds_left"].(float64)
	if left < 290 || left > 300 {
		t.Errorf("seconds_left %v", left)
	}
	if p["actor"] != "alice" || p["id"] == nil || p["reason"] == nil {
		t.Errorf("pending %v", p)
	}
	if _, err := time.Parse(time.RFC3339, p["deadline"].(string)); err != nil {
		t.Error(err)
	}
}

func TestPageAndAssetsHaveSecurityHeaders(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, p := range []string{"/policy", "/policy/static/policy.js", "/policy/static/policy.css"} {
		rec := h.do("GET", p, adminAddr, "", false)
		if rec.Code != 200 || rec.Body.Len() == 0 {
			t.Fatalf("%s: %d", p, rec.Code)
		}
		want := map[string]string{
			"Content-Security-Policy": wantCSP,
			"X-Frame-Options":         "DENY",
			"X-Content-Type-Options":  "nosniff",
			"Cache-Control":           "no-store",
		}
		for k, v := range want {
			if got := rec.Header().Get(k); got != v {
				t.Errorf("%s %s = %q", p, k, got)
			}
		}
	}
	if ct := h.do("GET", "/policy/static/policy.js", adminAddr, "", false).Header().Get("Content-Type"); !strings.Contains(ct, "javascript") {
		t.Errorf("js content type %q", ct)
	}
	if ct := h.do("GET", "/policy/static/policy.css", adminAddr, "", false).Header().Get("Content-Type"); !strings.Contains(ct, "css") {
		t.Errorf("css content type %q", ct)
	}
}

func TestPageHasNoInlineScriptOrExternalReferences(t *testing.T) {
	h := newHarness(t, baseEnv(t))
	for _, p := range []string{"/policy", "/policy/static/policy.js", "/policy/static/policy.css"} {
		body := strings.ToLower(h.do("GET", p, adminAddr, "", false).Body.String())
		for _, bad := range []string{"http://", "https://", "style=", "<style", "innerhtml"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q", p, bad)
			}
		}
		if p == "/policy" {
			for _, part := range strings.Split(body, "<script")[1:] {
				tag, _, _ := strings.Cut(part, ">")
				if !strings.Contains(tag, "src=") {
					t.Errorf("inline script tag: <script%s>", tag)
				}
			}
		}
	}
}

func TestStartRunsWatcherRevertsPastDeadline(t *testing.T) {
	env := baseEnv(t)
	st, err := OpenStore(env["POLICY_DB"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Begin("alice", "edit", "orig", "changed", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	st.Close()
	api := newFakeAPI("changed")
	_, m, err := register(t, env, api, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	begin := time.Now()
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if time.Since(begin) > time.Second {
		t.Error("Start did not return promptly")
	}
	deadline := time.Now().Add(3 * time.Second)
	for api.current() != "orig" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if api.current() != "orig" {
		t.Error("the past-deadline change was not reverted")
	}
	cancel()
	m.(core.Stopper).Stop(context.Background())
}

func TestStopWaitsForWatcherAndClosesStore(t *testing.T) {
	env := baseEnv(t)
	api := newFakeAPI("x")
	_, m, err := register(t, env, api, nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	if err := m.(core.Stopper).Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	pm := m.(*Module)
	select {
	case <-pm.watchDone:
	default:
		t.Error("watcher still running after Stop")
	}
	if _, err := pm.store.History(1); err == nil {
		t.Error("store still open after Stop")
	}
	// Stop bounded by its context when the watcher is never cancelled.
	_, m2, _ := register(t, baseEnv(t), api, nil)
	m2.Start(context.Background())
	sctx, scancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer scancel()
	begin := time.Now()
	m2.(core.Stopper).Stop(sctx)
	if time.Since(begin) > 2*time.Second {
		t.Error("Stop was not bounded by its context")
	}
}

func TestStopWithoutStartAndInactive(t *testing.T) {
	_, m, err := register(t, baseEnv(t), newFakeAPI("x"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.(core.Stopper).Stop(context.Background()); err != nil {
		t.Error(err)
	}
	mod := New(core.MapEnv(nil), newFakeAPI("x"))
	if err := mod.(core.Stopper).Stop(context.Background()); err != nil {
		t.Error(err)
	}
	if err := mod.Start(context.Background()); err != nil {
		t.Error(err)
	}
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func TestLogsNeverContainPolicyText(t *testing.T) {
	buf := &syncBuf{}
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(old)
	api := newFakeAPI("PLAIN-OLD-TEXT")
	mux, m, err := register(t, baseEnv(t), api, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	defer m.(core.Stopper).Stop(context.Background())
	h := &harness{mux: mux, api: api}
	h.apply(t, "TOP-SECRET-NEW-TEXT")
	h.post("/policy/preview", `{"policy":"TOP-SECRET-PREVIEW"}`)
	api.checkErr = &headscale.APIError{Status: 400, Message: "boom"}
	h.post("/policy/preview", `{"policy":"TOP-SECRET-PREVIEW"}`)
	h.confirm(t)
	api.fail(&headscale.APIError{Status: 400, Message: "rejected"})
	h.apply(t, "TOP-SECRET-REJECTED")
	api.fail(errors.New("net down"))
	h.apply(t, "TOP-SECRET-ERRORED")
	h.post("/policy/apply", `{"policy": "TOP-SECRET-BAD`)
	h.do("GET", "/policy/state", otherAddr, "", false)
	out := buf.String()
	for _, s := range []string{"TOP-SECRET", "PLAIN-OLD-TEXT", "secret-key-value"} {
		if strings.Contains(out, s) {
			t.Errorf("logs contain %q:\n%s", s, out)
		}
	}
	_ = io.Discard
}
