package runner

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

const testToken = "tok-for-tests"

// fakeBin is a stand-in for podman. It records argv and the TASK_* env, finds
// the mounted script/result dirs from the -v args, and behaves according to
// the first line of the script ("# mode: X"), so tests drive it through the
// same interface the real container gets.
const fakeBin = `#!/bin/sh
d="$FAKE_DIR"
prev=""
for a in "$@"; do
  if [ "$a" = rm ]; then echo "rm $*" >> "$d/rms"; exit 0; fi
  if [ "$prev" = kill ]; then
    echo "kill $a" >> "$d/kills"
    if [ ! -f "$d/ignorekill" ] && [ -f "$d/pid" ]; then kill "$(cat "$d/pid")"; fi
    exit 0
  fi
  prev="$a"
done
printf '%s\n' "$@" > "$d/argv"
env | grep '^TASK_' > "$d/env"
for a in "$@"; do
  case "$a" in
    *:/run/task:ro) script="${a%:/run/task:ro}/script.js" ;;
    *:/run/result) res="${a%:/run/result}" ;;
  esac
done
echo "$script" > "$d/scriptpath"
mode=$(head -n 1 "$script")
case "$mode" in
  "# mode: ok") tail -n +2 "$script" > "$res/result.json" ;;
  "# mode: big") { printf '{"a":"'; head -c 5000 /dev/zero | tr '\0' x; printf '"}'; } > "$res/result.json" ;;
  "# mode: nofile") : ;;
  "# mode: fifo") mkfifo "$res/result.json" ;;
  "# mode: symlink") ln -s /etc/hostname "$res/result.json" ;;
  "# mode: cat") cat "$d/stream" >&2; exit 1 ;;
  "# mode: catsleep") cat "$d/stream" >&2; echo $$ > "$d/pid"; exec sleep 30 ;;
  "# mode: fail") echo "boom: something broke" >&2; exit 3 ;;
  "# mode: leak")
    echo "env was $TASK_SECRETS_JSON" >&2
    printf 'split hunter2-' >&2; sleep 0.2; printf 'secret end\n' >&2
    echo "failed with hunter2-secret" >&2
    exit 4 ;;
  "# mode: ignorekill") touch "$d/ignorekill"; echo $$ > "$d/pid"; exec sleep 30 ;;
  "# mode: sleep") echo $$ > "$d/pid"; exec sleep 30 ;;
esac
exit 0
`

type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

type harness struct {
	logs  *lockedBuf
	t     *testing.T
	s     *Server
	ts    *httptest.Server
	fake  string
	state string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	root := t.TempDir()
	fakeDir := filepath.Join(root, "fake")
	if err := os.Mkdir(fakeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "fakepodman")
	if err := os.WriteFile(bin, []byte(fakeBin), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_DIR", fakeDir)
	stateDir := filepath.Join(root, "state")
	logs := &lockedBuf{}
	s, err := New(Config{
		Token:        testToken,
		Image:        "example.com/img@sha256:abc",
		StateDir:     stateDir,
		ContainerBin: bin,
		KillGrace:    300 * time.Millisecond,
		Logger:       slog.New(slog.NewTextHandler(logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s)
	t.Cleanup(func() {
		ts.Close()
		ctx, cancel := contextWithTimeout(5 * time.Second)
		defer cancel()
		s.Shutdown(ctx)
	})
	return &harness{logs: logs, t: t, s: s, ts: ts, fake: fakeDir, state: stateDir}
}

func (h *harness) do(method, path string, body any, token string) (int, []byte) {
	h.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		raw, _ := json.Marshal(b)
		rd = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func (h *harness) start(body map[string]any) string {
	h.t.Helper()
	code, out := h.do("POST", "/v1/runs", body, testToken)
	if code != 201 {
		h.t.Fatalf("start: %d %s", code, out)
	}
	var r struct{ ID string }
	_ = json.Unmarshal(out, &r)
	return r.ID
}

type runView struct {
	State   string         `json:"state"`
	Result  map[string]any `json:"result"`
	Error   string         `json:"error"`
	LogTail string         `json:"log_tail"`
}

func (h *harness) get(id string) (int, runView) {
	h.t.Helper()
	code, out := h.do("GET", "/v1/runs/"+id, nil, testToken)
	var v runView
	_ = json.Unmarshal(out, &v)
	return code, v
}

func (h *harness) wait(id, want string) runView {
	h.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, v := h.get(id)
		if v.State == want {
			return v
		}
		time.Sleep(20 * time.Millisecond)
	}
	_, v := h.get(id)
	h.t.Fatalf("run %s: state %q, want %q (error %q)", id, v.State, want, v.Error)
	return v
}

func (h *harness) waitIdle() {
	h.t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		_, out := h.do("GET", "/v1/health", nil, testToken)
		var hv struct{ Busy bool }
		_ = json.Unmarshal(out, &hv)
		if !hv.Busy {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	h.t.Fatal("runner still busy")
}

func (h *harness) readFake(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.fake, name))
	return string(b)
}

func TestRunnerAuthEmpty401(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct{ method, path, token string }{
		{"GET", "/v1/health", ""},
		{"GET", "/v1/health", "wrong"},
		{"POST", "/v1/runs", ""},
		{"GET", "/v1/runs/abc", "nope"},
		{"DELETE", "/v1/runs/abc", ""},
		{"GET", "/nothing", ""},
	} {
		code, body := h.do(tc.method, tc.path, nil, tc.token)
		if code != 401 || len(body) != 0 {
			t.Errorf("%s %s token=%q: %d %q, want empty 401", tc.method, tc.path, tc.token, code, body)
		}
	}
	code, body := h.do("GET", "/nothing", nil, testToken)
	if code != 404 || len(body) != 0 {
		t.Errorf("unknown route: %d %q, want empty 404", code, body)
	}
}

func TestRunnerHealth(t *testing.T) {
	h := newHarness(t)
	code, out := h.do("GET", "/v1/health", nil, testToken)
	if code != 200 {
		t.Fatalf("health: %d", code)
	}
	var hv map[string]any
	_ = json.Unmarshal(out, &hv)
	if hv["version"] == "" || hv["busy"] != false {
		t.Errorf("health: %v", hv)
	}
	if _, ok := hv["mem_available_mb"].(float64); !ok {
		t.Errorf("mem_available_mb missing: %v", hv)
	}
}

func TestRunnerSucceeds(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{
		"script":          "# mode: ok\n{\"balance\":42,\"currency\":\"xx\"}",
		"params":          map[string]any{"account": "a1"},
		"secrets":         map[string]string{"PASSWORD": "hunter2-secret"},
		"timeout_seconds": 30,
	})
	v := h.wait(id, "succeeded")
	want := map[string]any{"balance": float64(42), "currency": "xx"}
	if !reflect.DeepEqual(v.Result, want) {
		t.Errorf("result %v, want %v", v.Result, want)
	}
	argv := h.readFake("argv")
	lines := strings.Split(strings.TrimSpace(argv), "\n")
	if lines[0] != "--cgroup-manager=cgroupfs" || lines[1] != "run" {
		t.Errorf("argv must start with the global cgroup flag then run: %v", lines[:2])
	}
	for _, want := range []string{"--rm", "--name\ntask-" + id, "--read-only", "--cap-drop=ALL", "--security-opt\nno-new-privileges",
		"--pids-limit\n512", "--memory\n1g", "--tmpfs\n/tmp", "example.com/img@sha256:abc\nnode\n/app/host.js"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q:\n%s", want, argv)
		}
	}
	env := h.readFake("env")
	if !strings.Contains(env, "TASK_PARAMS_JSON={\"account\":\"a1\"}") || !strings.Contains(env, "hunter2-secret") {
		t.Errorf("child env lacks params/secrets: %q", env)
	}
}

func TestRunnerSecondRunConflicts(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: sleep"})
	code, _ := h.do("POST", "/v1/runs", map[string]any{"script": "# mode: ok\n{}"}, testToken)
	if code != 409 {
		t.Errorf("second run: %d, want 409", code)
	}
	h.do("DELETE", "/v1/runs/"+id, nil, testToken)
	h.waitIdle()
}

func TestRunnerTimeoutKillsContainerByName(t *testing.T) {
	for _, mode := range []string{"sleep", "ignorekill"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			id := h.start(map[string]any{"script": "# mode: " + mode, "timeout_seconds": 1})
			v := h.wait(id, "failed")
			if !strings.Contains(v.Error, "timeout") {
				t.Errorf("error %q should mention timeout", v.Error)
			}
			if !strings.Contains(h.readFake("kills"), "kill task-"+id) {
				t.Errorf("fake did not see kill task-%s: %q", id, h.readFake("kills"))
			}
			if rms := h.readFake("rms"); !strings.Contains(rms, "rm -f --time 0 task-"+id) {
				t.Errorf("fake did not see rm -f task-%s: %q", id, rms)
			}
			if argv := h.readFake("argv"); !strings.Contains(argv, "--timeout\n2\n") { // 1 s + 300 ms grace, rounded up
				t.Errorf("run argv lacks --timeout 2:\n%s", argv)
			}
			h.waitIdle()
		})
	}
}

func TestRunnerCancel(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: sleep"})
	deadline := time.Now().Add(3 * time.Second)
	for h.readFake("pid") == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if code, _ := h.do("DELETE", "/v1/runs/"+id, nil, testToken); code != 204 {
		t.Fatalf("delete: %d", code)
	}
	h.wait(id, "cancelled")
	h.waitIdle()
	if !strings.Contains(h.readFake("kills"), "kill task-"+id) {
		t.Errorf("no kill by name: %q", h.readFake("kills"))
	}
	rms := h.readFake("rms")
	if !strings.Contains(rms, "rm --cgroup-manager=cgroupfs rm -f --time 0 task-"+id) && !strings.Contains(rms, "rm -f --time 0 task-"+id) {
		t.Errorf("no rm -f by name after cancel: %q", rms)
	}
	if code, _ := h.do("DELETE", "/v1/runs/"+id, nil, testToken); code != 204 {
		t.Errorf("repeat delete: %d", code)
	}
	if code, _ := h.do("DELETE", "/v1/runs/0123456789abcdef", nil, testToken); code != 404 {
		t.Errorf("delete unknown: %d", code)
	}
}

func TestRunnerResultTooLarge(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: big"})
	v := h.wait(id, "failed")
	if !strings.Contains(v.Error, "too large") || v.Result != nil {
		t.Errorf("got error %q result %v", v.Error, v.Result)
	}
}

func TestRunnerResultNotJSON(t *testing.T) {
	h := newHarness(t)
	for name, body := range map[string]string{
		"invalid": "# mode: ok\nnot json {",
		"array":   "# mode: ok\n[1,2]",
		"missing": "# mode: nofile",
	} {
		id := h.start(map[string]any{"script": body})
		v := h.wait(id, "failed")
		if !strings.Contains(v.Error, "result") {
			t.Errorf("%s: error %q should explain the result problem", name, v.Error)
		}
		h.waitIdle()
	}
}

func TestRunnerNonZeroExit(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: fail"})
	v := h.wait(id, "failed")
	if !strings.Contains(v.Error, "exit status 3") || !strings.Contains(v.Error, "boom: something broke") {
		t.Errorf("error %q", v.Error)
	}
}

func TestRunnerSecretsNeverInArgvOrLogOrError(t *testing.T) {
	h := newHarness(t)
	const secret = "hunter2-secret"
	id := h.start(map[string]any{
		"script":  "# mode: leak",
		"secrets": map[string]string{"PASSWORD": secret},
	})
	v := h.wait(id, "failed")
	for name, text := range map[string]string{"error": v.Error, "log_tail": v.LogTail} {
		if strings.Contains(text, secret) {
			t.Errorf("%s leaks the secret: %q", name, text)
		}
		if !strings.Contains(text, "***") {
			t.Errorf("%s lacks the placeholder: %q", name, text)
		}
	}
	if logs := h.logs.String(); strings.Contains(logs, secret) || !strings.Contains(logs, "run started") {
		t.Errorf("logs must exist and never hold the secret:\n%s", logs)
	}
	argv := h.readFake("argv")
	if strings.Contains(argv, secret) {
		t.Errorf("argv contains the secret:\n%s", argv)
	}
	if !strings.Contains(argv, "-e\nTASK_SECRETS_JSON\n") || strings.Contains(argv, "TASK_SECRETS_JSON=") {
		t.Errorf("secrets must be passed as bare -e NAME:\n%s", argv)
	}
	if !strings.Contains(argv, "-e\nTASK_PARAMS_JSON\n") || strings.Contains(argv, "TASK_PARAMS_JSON=") {
		t.Errorf("params must be passed as bare -e NAME:\n%s", argv)
	}
}

func TestRunnerUnknownIDIs404(t *testing.T) {
	h := newHarness(t)
	code, body := h.do("GET", "/v1/runs/deadbeefdeadbeef", nil, testToken)
	if code != 404 {
		t.Errorf("unknown id: %d %q", code, body)
	}
}

func TestRunnerTempFilesRemoved(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: ok\n{}"})
	h.wait(id, "succeeded")
	left, _ := filepath.Glob(filepath.Join(h.state, "run-*"))
	if len(left) != 0 {
		t.Errorf("temp dirs left: %v", left)
	}
	id = h.start(map[string]any{"script": "# mode: fail"})
	h.wait(id, "failed")
	left, _ = filepath.Glob(filepath.Join(h.state, "run-*"))
	if len(left) != 0 {
		t.Errorf("temp dirs left after failure: %v", left)
	}
	for name, body := range map[string]map[string]any{
		"timeout": {"script": "# mode: sleep", "timeout_seconds": 1},
		"cancel":  {"script": "# mode: sleep"},
	} {
		id = h.start(body)
		if name == "cancel" {
			for h.readFake("pid") == "" {
				time.Sleep(10 * time.Millisecond)
			}
			h.do("DELETE", "/v1/runs/"+id, nil, testToken)
		}
		h.waitIdle()
		if left, _ = filepath.Glob(filepath.Join(h.state, "run-*")); len(left) != 0 {
			t.Errorf("temp dirs left after %s: %v", name, left)
		}
	}
	sp := strings.TrimSpace(h.readFake("scriptpath"))
	if _, err := os.Stat(sp); err == nil {
		t.Errorf("script file %s still exists", sp)
	}
}

func TestRunnerStaleDirsRemovedAtStart(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "run-0123456789abcdef", "script")
	if err := os.MkdirAll(stale, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{Token: "t", Image: "i", StateDir: dir}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(stale)); !os.IsNotExist(err) {
		t.Errorf("stale dir survived: %v", err)
	}
}

func TestRunnerRejectsBadRequests(t *testing.T) {
	h := newHarness(t)
	big := strings.Repeat("x", 1<<20+10)
	for name, body := range map[string]any{
		"no script":        map[string]any{"params": map[string]any{}},
		"empty script":     map[string]any{"script": ""},
		"timeout zero":     map[string]any{"script": "x", "timeout_seconds": 0},
		"timeout too big":  map[string]any{"script": "x", "timeout_seconds": 1801},
		"timeout negative": map[string]any{"script": "x", "timeout_seconds": -5},
		"secrets array":    map[string]any{"script": "x", "secrets": []string{"a"}},
		"secret non-str":   map[string]any{"script": "x", "secrets": map[string]any{"A": 1}},
		"params string":    map[string]any{"script": "x", "params": "nope"},
		"not json":         "{{{",
		"trailing data":    "{\"script\":\"x\"} {}",
		"params array":     map[string]any{"script": "x", "params": []int{1}},
		"too large":        map[string]any{"script": big},
	} {
		if code, out := h.do("POST", "/v1/runs", body, testToken); code != 422 {
			t.Errorf("%s: %d %s, want 422", name, code, out)
		}
	}
	h.waitIdle()
}

func TestRunnerParamsKeepBigIntegers(t *testing.T) {
	h := newHarness(t)
	id := h.start(map[string]any{"script": "# mode: ok\n{}"})
	h.wait(id, "succeeded")
	code, out := h.do("POST", "/v1/runs", "{\"script\":\"# mode: ok\\n{}\",\"params\":{ \"n\": 12345678901234567890 }}", testToken)
	if code != 201 {
		t.Fatalf("%d %s", code, out)
	}
	h.waitIdle()
	if env := h.readFake("env"); !strings.Contains(env, `TASK_PARAMS_JSON={"n":12345678901234567890}`) {
		t.Errorf("params not passed through verbatim: %q", env)
	}
}

func TestRunnerNonRegularResultFailsAndReleasesGate(t *testing.T) {
	for _, mode := range []string{"fifo", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t)
			id := h.start(map[string]any{"script": "# mode: " + mode})
			v := h.wait(id, "failed")
			if !strings.Contains(v.Error, "result") {
				t.Errorf("error %q", v.Error)
			}
			h.waitIdle()
			id2 := h.start(map[string]any{"script": "# mode: ok\n{}"})
			h.wait(id2, "succeeded")
		})
	}
}

func noFragments(t *testing.T, where, out string, secrets ...string) {
	t.Helper()
	for _, sec := range secrets {
		for k := 3; k < len(sec); k++ {
			for _, frag := range []string{sec[:k], sec[len(sec)-k:]} {
				if strings.Contains(out, frag) {
					t.Errorf("%s leaks %q of %q", where, frag, sec)
				}
			}
		}
	}
}

// streamStartingInside builds output whose last (4096+len(longest)) bytes
// start `off` bytes into head, then a trailing full occurrence of each secret.
func streamStartingInside(head string, off int, longest string, secrets ...string) string {
	tail := " " + strings.Join(secrets, " ")
	pad := 4096 + len(longest) + off - len(head) - len(tail)
	return head + strings.Repeat("-", pad) + tail
}

func TestRunnerNestedAndOverlappingSecretsNoFragmentInAnyTail(t *testing.T) {
	url, pw := "postgres://user:hunter2pw@host/db", "hunter2pw"
	a, b := "abcdefgh", "efghijkl"
	for name, tc := range map[string]struct {
		secrets map[string]string
		stream  string
		all     []string
	}{
		"nested":  {map[string]string{"URL": url, "PW": pw}, streamStartingInside(url, 11, url, url, pw), []string{url, pw}},
		"overlap": {map[string]string{"A": a, "B": b}, streamStartingInside("abcdefghijkl", 1, a, a, b), []string{a, b}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			if err := os.WriteFile(filepath.Join(h.fake, "stream"), []byte(tc.stream), 0o644); err != nil {
				t.Fatal(err)
			}
			// Live tail, while the run is still going.
			id := h.start(map[string]any{"script": "# mode: catsleep", "secrets": tc.secrets})
			var live runView
			for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if _, live = h.get(id); len(live.LogTail) > 3000 {
					break
				}
			}
			if len(live.LogTail) == 0 {
				t.Fatal("no live tail")
			}
			noFragments(t, "live tail", live.LogTail, tc.all...)
			h.do("DELETE", "/v1/runs/"+id, nil, testToken)
			h.waitIdle()
			// Final log_tail and error tail.
			id = h.start(map[string]any{"script": "# mode: cat", "secrets": tc.secrets})
			v := h.wait(id, "failed")
			noFragments(t, "log_tail", v.LogTail, tc.all...)
			noFragments(t, "error", v.Error, tc.all...)
		})
	}
}
