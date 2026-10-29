package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type sessView struct {
	Session *struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		StartedAt string `json:"started_at"`
		ExpiresAt string `json:"expires_at"`
		Port      int    `json:"port"`
		Reason    string `json:"reason"`
	} `json:"session"`
}

func sessHarness(t *testing.T, unit time.Duration) *harness {
	return newHarness(t, func(c *Config) {
		c.SessionBind = "100.64.0.9"
		c.sessionUnit = unit
	})
}

func (h *harness) startSession(body any) string {
	h.t.Helper()
	code, out := h.do("POST", "/v1/sessions", body, testToken)
	if code != 201 {
		h.t.Fatalf("start session: %d %s", code, out)
	}
	var r struct {
		ID        string `json:"id"`
		Port      int    `json:"port"`
		ExpiresAt string `json:"expires_at"`
	}
	if err := json.Unmarshal(out, &r); err != nil || r.ID == "" || r.Port != 6080 {
		h.t.Fatalf("bad start body %s", out)
	}
	if _, err := time.Parse(time.RFC3339, r.ExpiresAt); err != nil || !strings.HasSuffix(r.ExpiresAt, "Z") {
		h.t.Fatalf("expires_at %q is not RFC 3339 UTC", r.ExpiresAt)
	}
	return r.ID
}

func (h *harness) sessionView() sessView {
	h.t.Helper()
	code, out := h.do("GET", "/v1/sessions", nil, testToken)
	var v sessView
	if code != 200 || json.Unmarshal(out, &v) != nil {
		h.t.Fatalf("get session: %d %s", code, out)
	}
	return v
}

func (h *harness) waitSessionEnded() sessView {
	h.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if v := h.sessionView(); v.Session != nil && v.Session.State == "ended" {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatal("session did not end")
	return sessView{}
}

func TestSessionStartPublishesOnBindOnly(t *testing.T) {
	h := sessHarness(t, time.Second)
	id := h.startSession(map[string]any{})
	for i := 0; i < 250 && !strings.Contains(h.readFake("env"), "SESSION_"); i++ { // fake writes argv before env
		time.Sleep(20 * time.Millisecond)
	}
	argv := strings.Split(strings.TrimSpace(h.readFake("argv")), "\n")
	found := false
	for i, a := range argv {
		if a == "-p" && i+1 < len(argv) {
			found = true
			if argv[i+1] != "100.64.0.9:6080:6080" {
				t.Errorf("-p %q", argv[i+1])
			}
		}
		if strings.HasPrefix(a, ":") || strings.HasPrefix(a, "0.0.0.0") {
			t.Errorf("unbound publish arg %q", a)
		}
	}
	if !found {
		t.Fatalf("no -p in %v", argv)
	}
	joined := " " + strings.Join(argv, " ") + " "
	for _, want := range []string{" --init ", " --read-only ", " --cap-drop=ALL ", " --shm-size=512m ", " task-" + id + " ", " sh /app/session.sh ", " --tmpfs /home/pwuser:rw "} {
		if !strings.Contains(joined, want) {
			t.Errorf("argv lacks %q: %v", want, argv)
		}
	}
	env := h.readFake("env")
	for _, want := range []string{"SESSION_IDLE_SECONDS=900\n", "SESSION_NOVNC_PORT=6080\n"} {
		if !strings.Contains(env, want) {
			t.Errorf("child env lacks %q: %q", want, env)
		}
	}
	v := h.sessionView()
	if v.Session == nil || v.Session.ID != id || v.Session.State != "running" || v.Session.Port != 6080 || v.Session.Reason != "" {
		t.Fatalf("view %+v", v.Session)
	}
	if code, _ := h.do("DELETE", "/v1/sessions/"+id, nil, testToken); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	h.waitIdle()
}

func TestSessionBadBindRejectedAtStartup(t *testing.T) {
	for _, bind := range []string{"0.0.0.0", "::", "host.example", "300.1.1.1"} {
		if _, err := New(Config{Token: "t", Image: "i", StateDir: t.TempDir(), SessionBind: bind}); err == nil {
			t.Errorf("bind %q accepted", bind)
		}
	}
	if _, err := New(Config{Token: "t", Image: "i", StateDir: t.TempDir(), SessionBind: "fd7a::1"}); err != nil {
		t.Errorf("ipv6 bind: %v", err)
	}
}

func TestSessionDisabledWithoutBind(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("POST", "/v1/sessions", map[string]any{}, testToken)
	if code != 404 || len(body) != 0 {
		t.Fatalf("%d %q", code, body)
	}
	if h.readFake("argv") != "" {
		t.Fatal("a container was started")
	}
}

func TestSessionStatusDisabledWithoutBind(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do("GET", "/v1/sessions", nil, testToken); code != 404 || len(body) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestSessionStopDisabledWithoutBind(t *testing.T) {
	h := newHarness(t)
	if code, body := h.do("DELETE", "/v1/sessions/abc", nil, testToken); code != 404 || len(body) != 0 {
		t.Fatalf("%d %q", code, body)
	}
}

func TestSessionSharesSlotWithRuns(t *testing.T) {
	h := sessHarness(t, time.Second)
	id := h.startSession(nil)
	if code, _ := h.do("POST", "/v1/runs", map[string]any{"script": "# mode: ok\n{}"}, testToken); code != 409 {
		t.Fatalf("run during session: %d", code)
	}
	_, out := h.do("GET", "/v1/health", nil, testToken)
	if !strings.Contains(string(out), `"busy":true`) {
		t.Fatalf("health %s", out)
	}
	h.do("DELETE", "/v1/sessions/"+id, nil, testToken)
	h.waitIdle()

	rid := h.start(map[string]any{"script": "# mode: sleep\n"})
	if code, _ := h.do("POST", "/v1/sessions", map[string]any{}, testToken); code != 409 {
		t.Fatalf("session during run: %d", code)
	}
	h.do("DELETE", "/v1/runs/"+rid, nil, testToken)
	h.waitIdle()
}

func TestSessionStartRaceSingle201(t *testing.T) {
	h := sessHarness(t, time.Second)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i], _ = h.do("POST", "/v1/sessions", map[string]any{}, testToken)
		}()
	}
	wg.Wait()
	if !(codes[0] == 201 && codes[1] == 409) && !(codes[0] == 409 && codes[1] == 201) {
		t.Fatalf("codes %v", codes)
	}
}

func TestSessionTTLEndsAndRemovesContainer(t *testing.T) {
	h := sessHarness(t, 10*time.Millisecond) // ttl 60 -> 600 ms
	id := h.startSession(map[string]any{"ttl_seconds": 60})
	v := h.waitSessionEnded()
	if v.Session.Reason != "ttl" || v.Session.ID != id {
		t.Fatalf("view %+v", v.Session)
	}
	h.waitIdle()
	if !strings.Contains(h.readFake("kills"), "task-"+id) {
		t.Errorf("no kill by name: %q", h.readFake("kills"))
	}
	if rms := h.readFake("rms"); !strings.Contains(rms, "rm") || !strings.Contains(rms, "-f") || !strings.Contains(rms, "task-"+id) {
		t.Errorf("no rm -f by name: %q", rms)
	}
}

func TestSessionIdleExitIsReasonIdle(t *testing.T) {
	h := sessHarness(t, time.Second)
	if err := writeFake(h, "sessexit", "0"); err != nil {
		t.Fatal(err)
	}
	h.startSession(map[string]any{"idle_seconds": 60})
	if v := h.waitSessionEnded(); v.Session.Reason != "idle" {
		t.Fatalf("reason %q", v.Session.Reason)
	}
	h.waitIdle()
}

func TestSessionNonZeroExitIsFailed(t *testing.T) {
	h := sessHarness(t, time.Second)
	if err := writeFake(h, "sessexit", "3"); err != nil {
		t.Fatal(err)
	}
	h.startSession(nil)
	if v := h.waitSessionEnded(); v.Session.Reason != "failed" {
		t.Fatalf("reason %q", v.Session.Reason)
	}
	h.waitIdle()
}

func TestSessionDeleteIdempotent(t *testing.T) {
	h := sessHarness(t, time.Second)
	id := h.startSession(nil)
	if code, _ := h.do("DELETE", "/v1/sessions/nope", nil, testToken); code != 404 {
		t.Fatalf("unknown: %d", code)
	}
	for i := 0; i < 2; i++ {
		if code, _ := h.do("DELETE", "/v1/sessions/"+id, nil, testToken); code != 204 {
			t.Fatalf("delete %d: %d", i, code)
		}
	}
	h.waitIdle()
	if v := h.sessionView(); v.Session == nil || v.Session.State != "ended" || v.Session.Reason != "stopped" {
		t.Fatalf("view %+v", v.Session)
	}
	if code, _ := h.do("DELETE", "/v1/sessions/"+id, nil, testToken); code != 204 {
		t.Fatalf("delete after end: %d", code)
	}
	if !strings.Contains(h.readFake("rms"), "task-"+id) {
		t.Error("container not removed by name")
	}
}

func TestSessionGetEmpty(t *testing.T) {
	h := sessHarness(t, time.Second)
	if _, out := h.do("GET", "/v1/sessions", nil, testToken); strings.TrimSpace(string(out)) != `{"session":null}` {
		t.Fatalf("%s", out)
	}
}

func TestSessionShutdownRemovesContainer(t *testing.T) {
	h := sessHarness(t, time.Second)
	id := h.startSession(nil)
	ctx, cancel := contextWithTimeout(5 * time.Second)
	defer cancel()
	h.s.Shutdown(ctx)
	h.s.mu.Lock()
	busy := h.s.activeSession != nil
	h.s.mu.Unlock()
	if busy {
		t.Fatal("slot still held after Shutdown")
	}
	if !strings.Contains(h.readFake("rms"), "task-"+id) {
		t.Errorf("container not removed: %q", h.readFake("rms"))
	}
}

func TestSessionRejectsBadRequests(t *testing.T) {
	h := sessHarness(t, time.Second)
	for _, body := range []any{
		map[string]any{"ttl_seconds": 59}, map[string]any{"ttl_seconds": 14401},
		map[string]any{"idle_seconds": 59}, map[string]any{"idle_seconds": 3601},
		`{"ttl_seconds":60} {}`, `{`, `{"ttl_seconds":"x"}`,
	} {
		if code, _ := h.do("POST", "/v1/sessions", body, testToken); code != 422 {
			t.Errorf("%v: %d, want 422", body, code)
		}
	}
	if h.readFake("argv") != "" {
		t.Fatal("a container was started")
	}
	h.startSession(map[string]any{"ttl_seconds": 14400, "idle_seconds": 3600})
}

func TestSessionNeedsToken(t *testing.T) {
	h := sessHarness(t, time.Second)
	for _, tc := range []struct{ method, path, token string }{
		{"POST", "/v1/sessions", ""}, {"GET", "/v1/sessions", "wrong"}, {"DELETE", "/v1/sessions/x", ""},
	} {
		if code, body := h.do(tc.method, tc.path, nil, tc.token); code != 401 || len(body) != 0 {
			t.Errorf("%s %s: %d %q", tc.method, tc.path, code, body)
		}
	}
}

func writeFake(h *harness, name, content string) error {
	return os.WriteFile(filepath.Join(h.fake, name), []byte(content), 0o644)
}
