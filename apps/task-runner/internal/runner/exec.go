package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// globalFlags go before the subcommand. Rootless podman for a service user
// without a login session only works with the cgroupfs manager; the default
// systemd manager fails. A package-level slice so tests can assert it.
var globalFlags = []string{"--cgroup-manager=cgroupfs"}

const (
	maxResultBytes = 4096
	logTailBytes   = 4096
	minSecretLen   = 4
	errTailBytes   = 500
)

// scrubber replaces secret values with "***". Values shorter than minSecretLen
// are ignored (they would mangle ordinary text). Each value is matched in its
// raw form and in its JSON-escaped forms (a script may print the secrets
// object), longest first so a value that contains another is replaced whole.
type scrubber struct {
	r        *strings.Replacer
	variants []string
	maxLen_  int
}

func jsonEscaped(v string, html bool) string {
	var b strings.Builder
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(html)
	_ = enc.Encode(v)
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(b.String()), `"`), `"`)
}

func newScrubber(secrets map[string]string) *scrubber {
	seen := map[string]bool{}
	var vals []string
	add := func(v string) {
		if v != "" && !seen[v] {
			seen[v] = true
			vals = append(vals, v)
		}
	}
	for _, v := range secrets {
		if len(v) >= minSecretLen {
			add(v)
			add(jsonEscaped(v, true))
			add(jsonEscaped(v, false))
		}
	}
	sort.Slice(vals, func(i, j int) bool { return len(vals[i]) > len(vals[j]) })
	s := &scrubber{}
	if len(vals) == 0 {
		return s
	}
	pairs := make([]string, 0, 2*len(vals))
	for _, v := range vals {
		pairs = append(pairs, v, "***")
	}
	s.r = strings.NewReplacer(pairs...)
	s.variants = vals
	s.maxLen_ = len(vals[0])
	return s
}

func (s *scrubber) maxLen() int { return s.maxLen_ }

func (s *scrubber) scrub(in string) string {
	if s == nil || s.r == nil {
		return in
	}
	return s.r.Replace(in)
}

// minFragment is the shortest secret fragment trimFragment removes; shorter
// ones would eat ordinary text and tell an observer next to nothing.
const minFragment = 3

// trimFragment drops leading bytes that are a proper suffix of a secret, until
// none is left: when the ring evicted the start of a secret, scrubbing cannot
// see it and the remainder would otherwise show. Run it on raw, unscrubbed text.
func (s *scrubber) trimFragment(in string) string {
	if s == nil || s.r == nil {
		return in
	}
	for {
		cut := 0
		for _, v := range s.variants {
			for k := len(v) - 1; k >= minFragment && k > cut; k-- {
				if strings.HasPrefix(in, v[len(v)-k:]) {
					cut = k
				}
			}
		}
		if cut == 0 {
			return in
		}
		in = in[cut:]
	}
}

// ring keeps the last bytes of combined stdout/stderr. It holds raw bytes,
// slightly more than the tail needs (the longest secret), and is scrubbed only
// when read, so a secret split across several writes is still caught.
type ring struct {
	mu  sync.Mutex
	buf []byte
	cap int
	// overflowed: bytes were evicted, so the start of buf may cut a secret.
	overflowed bool
}

func newRing(extra int) *ring { return &ring{cap: logTailBytes + extra} }

func (r *ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.buf = append(r.buf, p...)
	if len(r.buf) > r.cap {
		r.overflowed = true
		r.buf = append([]byte(nil), r.buf[len(r.buf)-r.cap:]...)
	}
	return len(p), nil
}

// tail returns the scrubbed last logTailBytes bytes.
func (r *ring) tail(s *scrubber) string {
	r.mu.Lock()
	raw, overflowed := string(r.buf), r.overflowed
	r.mu.Unlock()
	// Trim the raw bytes, before scrubbing: once a nested or overlapping secret
	// has been replaced, the leftover of the evicted one no longer looks like a
	// suffix of anything.
	if overflowed {
		raw = s.trimFragment(raw)
	}
	out := s.scrub(raw)
	if len(out) > logTailBytes {
		out = out[len(out)-logTailBytes:]
	}
	return strings.ToValidUTF8(out, "")
}

// childEnv is the environment of the container client: ours minus RUNNER_*
// (the bearer token must not reach it), plus the two values `-e NAME` forwards.
func childEnv(secretsJSON, paramsJSON string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "RUNNER_") || strings.HasPrefix(kv, "TASK_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TASK_SECRETS_JSON="+secretsJSON, "TASK_PARAMS_JSON="+paramsJSON)
}

// containerArgs builds the `run` command. --timeout is podman's own backstop
// for the container (the runner enforces the real deadline and kills first);
// it lets the container die even if the runner or its client is gone.
func (s *Server) containerArgs(id, dir string, timeout time.Duration) []string {
	args := append([]string(nil), globalFlags...)
	return append(args,
		"run", "--rm", "--name", "task-"+id,
		"--timeout", strconv.Itoa(int((timeout+s.cfg.KillGrace+time.Second-1)/time.Second)),
		"--read-only", "--cap-drop=ALL", "--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(s.cfg.PidsLimit), "--memory", s.cfg.Memory,
		"--tmpfs", "/tmp",
		"-v", filepath.Join(dir, "script")+":/run/task:ro",
		"-v", filepath.Join(dir, "result")+":/run/result",
		"-e", "TASK_SECRETS_JSON", "-e", "TASK_PARAMS_JSON",
		s.cfg.Image, "node", "/app/host.js")
}

// outcome of one run, before it is published into the run record.
type outcome struct {
	state  string
	result map[string]any
	err    string
}

// execute runs one container to completion. The temp dir is created and
// removed here; the caller publishes the outcome only after cleanup, so a
// finished state implies the files are gone.
func (s *Server) execute(r *run, script, secretsJSON, paramsJSON string, timeout time.Duration) outcome {
	dir := filepath.Join(s.cfg.StateDir, "run-"+r.id)
	defer func() {
		if err := os.RemoveAll(dir); err != nil {
			s.log.Error("remove run dir failed", "run", r.id, "err", r.scr.scrub(err.Error()))
		}
	}()
	// The script and result dirs are bind-mounted; in rootless mode the
	// container's root maps to this user, but an image that drops to another
	// uid must still read the script and write the result, hence 0755/0644 and
	// a 0777 result dir. The 0700 parent keeps every other host user out.
	if err := os.Mkdir(dir, 0o700); err != nil {
		return outcome{state: "failed", err: "prepare run dir: " + err.Error()}
	}
	scriptDir, resultDir := filepath.Join(dir, "script"), filepath.Join(dir, "result")
	for _, d := range []string{scriptDir, resultDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return outcome{state: "failed", err: "prepare run dir: " + err.Error()}
		}
	}
	if err := os.Chmod(resultDir, 0o777); err != nil {
		return outcome{state: "failed", err: "prepare run dir: " + err.Error()}
	}
	if err := os.WriteFile(filepath.Join(scriptDir, "script.js"), []byte(script), 0o644); err != nil {
		return outcome{state: "failed", err: "prepare run dir: " + err.Error()}
	}

	cmd := exec.Command(s.cfg.ContainerBin, s.containerArgs(r.id, dir, timeout)...)
	cmd.Env = childEnv(secretsJSON, paramsJSON)
	cmd.Stdout, cmd.Stderr = r.ring, r.ring
	// Own process group: a hung or fake client may ignore `kill`, and the
	// group kill is what reaps it.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		return outcome{state: "failed", err: r.scr.scrub("start container: " + err.Error())}
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitErr error
	stopped := ""
	select {
	case waitErr = <-done:
	case <-timer.C:
		stopped = "timeout"
	case <-r.cancelCh:
		stopped = "cancel"
	}
	if stopped != "" {
		s.killByName(r.id)
		select {
		case waitErr = <-done:
		case <-time.After(s.cfg.KillGrace):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			waitErr = <-done
		}
		// The container is a child of conmon, not of the client, so reaping the
		// client proves nothing: remove it by name once the client is gone.
		s.removeByName(r.id)
	}

	switch {
	case stopped == "cancel":
		return outcome{state: "cancelled"}
	case stopped == "timeout":
		return outcome{state: "failed", err: fmt.Sprintf("timeout after %s, container killed", timeout)}
	case waitErr != nil:
		msg := r.scr.scrub(waitErr.Error())
		if t := lastLines(r.ring.tail(r.scr), errTailBytes); t != "" {
			msg += ": " + t
		}
		return outcome{state: "failed", err: msg}
	}
	res, err := readResult(filepath.Join(resultDir, "result.json"))
	if err != nil {
		return outcome{state: "failed", err: err.Error()}
	}
	return outcome{state: "succeeded", result: res}
}

// rmRetryDelay separates the two rm attempts after a timeout or cancel (the
// first can race a container that is still being created).
var rmRetryDelay = 300 * time.Millisecond

func (s *Server) removeByName(id string) {
	args := append(append([]string(nil), globalFlags...), "rm", "-f", "--time", "0", "task-"+id)
	for i := 0; i < 2; i++ {
		if i > 0 {
			time.Sleep(rmRetryDelay)
		}
		s.runBounded(args)
	}
}

// killByName asks the container engine to kill the named container; best
// effort and short, the process-group kill backs it up.
func (s *Server) killByName(id string) {
	s.runBounded(append(append([]string(nil), globalFlags...), "kill", "task-"+id))
}

// runBounded runs the container binary with args, best effort, capped at 5 s
// (the process group is killed beyond that).
func (s *Server) runBounded(args []string) {
	cmd := exec.Command(s.cfg.ContainerBin, args...)
	cmd.Env = childEnv("", "")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.Start() != nil {
		return
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		<-done
	}
}

// readResult reads the script's result file: a regular file of at most
// maxResultBytes holding one JSON object. The container wrote it, so a symlink
// pointing at a host file must not be followed (O_NOFOLLOW, no check-then-open). O_NONBLOCK keeps a FIFO planted
// by the script from blocking open() forever and holding the run gate.
func readResult(path string) (map[string]any, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, errors.New("result file missing: the script produced no result.json")
		}
		return nil, errors.New("result file unreadable or not a regular file")
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() {
		return nil, errors.New("result file is not a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxResultBytes+1))
	if err != nil {
		return nil, errors.New("result file unreadable")
	}
	if len(raw) > maxResultBytes {
		return nil, fmt.Errorf("result too large (limit %d bytes)", maxResultBytes)
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, errors.New("result is not valid JSON")
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("result is not a JSON object")
	}
	return obj, nil
}

// lastLines returns the tail of s, at most max bytes, cut at a line start
// when possible and trimmed.
func lastLines(s string, max int) string {
	s = strings.TrimSpace(s)
	if len(s) > max {
		s = s[len(s)-max:]
		if i := strings.IndexByte(s, '\n'); i >= 0 && i < len(s)-1 {
			s = s[i+1:]
		}
	}
	return strings.ToValidUTF8(strings.TrimSpace(s), "")
}
