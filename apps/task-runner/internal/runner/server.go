// Package runner is a small generic script runner: it executes one throwaway
// rootless container per run and exposes start/poll/cancel over HTTP.
package runner

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is reported by /v1/health.
const Version = "0.1.0"

const (
	maxBody        = 1 << 20
	maxEnvJSON     = 100_000 // Linux caps a single environment string at 128 KiB
	defaultTimeout = 180
	maxTimeout     = 1800
	keepFinished   = 20
	keepFor        = time.Hour
)

// Config is the runner's configuration; zero values get the documented defaults.
type Config struct {
	Token        string // required bearer token
	Image        string // required container image, pinned by digest
	StateDir     string
	ContainerBin string
	Memory       string
	PidsLimit    int
	KillGrace    time.Duration // wait after `kill <name>` before killing the process group
	Logger       *slog.Logger
}

type run struct {
	id        string
	state     string
	result    map[string]any
	errText   string
	started   time.Time
	finished  time.Time
	ring      *ring
	scr       *scrubber
	tail      string // final scrubbed log tail, set when the run ends
	cancelCh  chan struct{}
	cancelled bool
	done      chan struct{}
}

// Server holds the run table. Runs live in memory only: after a restart every
// id is unknown (404).
type Server struct {
	cfg    Config
	log    *slog.Logger
	tokenH [32]byte

	mu     sync.Mutex
	runs   map[string]*run
	active *run
}

// New validates cfg, applies defaults and removes stale run dirs left by a
// previous process.
func New(cfg Config) (*Server, error) {
	if cfg.Token == "" || cfg.Image == "" {
		return nil, errors.New("token and image are required")
	}
	if cfg.StateDir == "" {
		cfg.StateDir = "/var/lib/task-runner"
	}
	if cfg.ContainerBin == "" {
		cfg.ContainerBin = "podman"
	}
	if cfg.Memory == "" {
		cfg.Memory = "1g"
	}
	if cfg.PidsLimit <= 0 {
		cfg.PidsLimit = 512
	}
	if cfg.KillGrace <= 0 {
		cfg.KillGrace = 3 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, err
	}
	stale, _ := filepath.Glob(filepath.Join(cfg.StateDir, "run-*"))
	for _, d := range stale {
		_ = os.RemoveAll(d)
	}
	return &Server{cfg: cfg, log: cfg.Logger, tokenH: sha256.Sum256([]byte(cfg.Token)), runs: map[string]*run{}}, nil
}

// authorized compares digests so neither content nor length of the token
// leaks through timing.
func (s *Server) authorized(r *http.Request) bool {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return false
	}
	h := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(h[:], s.tokenH[:]) == 1
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Failures are bare status codes on purpose: nothing for a scanner to read.
	if !s.authorized(r) {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	p := r.URL.Path
	switch {
	case p == "/v1/health" && r.Method == http.MethodGet:
		s.health(w)
	case p == "/v1/runs" && r.Method == http.MethodPost:
		s.start(w, r)
	case strings.HasPrefix(p, "/v1/runs/") && r.Method == http.MethodGet:
		s.status(w, strings.TrimPrefix(p, "/v1/runs/"))
	case strings.HasPrefix(p, "/v1/runs/") && r.Method == http.MethodDelete:
		s.cancel(w, strings.TrimPrefix(p, "/v1/runs/"))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) health(w http.ResponseWriter) {
	s.mu.Lock()
	busy := s.active != nil
	s.mu.Unlock()
	writeJSON(w, 200, map[string]any{"version": Version, "busy": busy, "mem_available_mb": memAvailableMB()})
}

// memAvailableMB reads MemAvailable from /proc/meminfo, -1 when unavailable.
func memAvailableMB() int {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == "MemAvailable:" {
			if kb, err := strconv.Atoi(f[1]); err == nil {
				return kb / 1024
			}
		}
	}
	return -1
}

type startRequest struct {
	Script         string          `json:"script"`
	Params         json.RawMessage `json:"params"`
	Secrets        json.RawMessage `json:"secrets"`
	TimeoutSeconds *int            `json:"timeout_seconds"`
}

func (s *Server) start(w http.ResponseWriter, r *http.Request) {
	var req startRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	timeout := defaultTimeout
	if req.TimeoutSeconds != nil {
		timeout = *req.TimeoutSeconds
	}
	secrets := map[string]string{}
	params := map[string]any{}
	if req.Script == "" || timeout < 1 || timeout > maxTimeout ||
		!decodeOptional(req.Secrets, &secrets) || !decodeOptional(req.Params, &params) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	secretsJSON, _ := json.Marshal(secrets)
	paramsJSON, _ := json.Marshal(params)
	if len(secretsJSON) > maxEnvJSON || len(paramsJSON) > maxEnvJSON {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}

	scr := newScrubber(secrets)
	run := &run{
		id: newID(), state: "running", started: time.Now(), scr: scr,
		ring: newRing(scr.maxLen()), cancelCh: make(chan struct{}), done: make(chan struct{}),
	}
	s.mu.Lock()
	s.prune()
	if s.active != nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusConflict)
		return
	}
	s.active = run
	s.runs[run.id] = run
	s.mu.Unlock()

	s.log.Info("run started", "run", run.id, "timeout_s", timeout)
	go func() {
		out := s.execute(run, req.Script, string(secretsJSON), string(paramsJSON), time.Duration(timeout)*time.Second)
		s.mu.Lock()
		if !run.cancelled { // a cancel already published its state
			run.state, run.result, run.errText = out.state, out.result, out.err
		}
		run.tail = run.ring.tail(run.scr)
		run.scr = nil
		run.finished = time.Now()
		s.active = nil
		state, dur := run.state, run.finished.Sub(run.started)
		s.mu.Unlock()
		close(run.done)
		s.log.Info("run finished", "run", run.id, "state", state, "duration_ms", dur.Milliseconds())
	}()
	writeJSON(w, 201, map[string]string{"id": run.id})
}

// decodeOptional unmarshals raw into dst (a map); absent or null is fine,
// anything of another JSON type is not.
func decodeOptional(raw json.RawMessage, dst any) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return true
	}
	return json.Unmarshal(raw, dst) == nil
}

func newID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// prune forgets finished runs older than keepFor and all but the newest
// keepFinished. Caller holds s.mu.
func (s *Server) prune() {
	var fin []*run
	for id, r := range s.runs {
		if r.finished.IsZero() {
			continue
		}
		if time.Since(r.finished) > keepFor {
			delete(s.runs, id)
			continue
		}
		fin = append(fin, r)
	}
	if len(fin) <= keepFinished {
		return
	}
	sort.Slice(fin, func(i, j int) bool { return fin[i].finished.After(fin[j].finished) })
	for _, r := range fin[keepFinished:] {
		delete(s.runs, r.id)
	}
}

func (s *Server) status(w http.ResponseWriter, id string) {
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	state, result, errText, tail, scr := r.state, r.result, r.errText, r.tail, r.scr
	ring := r.ring
	s.mu.Unlock()
	if scr != nil { // still running (or just cancelled): scrub the live ring
		tail = ring.tail(scr)
	}
	writeJSON(w, 200, map[string]any{"state": state, "result": result, "error": errText, "log_tail": tail})
}

func (s *Server) cancel(w http.ResponseWriter, id string) {
	s.mu.Lock()
	r := s.runs[id]
	if r == nil {
		s.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
		return
	}
	s.cancelLocked(r)
	s.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

// cancelLocked marks a running run cancelled at once; the slot stays taken
// until execute has reaped the container (409 meanwhile).
func (s *Server) cancelLocked(r *run) {
	if r.state == "running" && !r.cancelled {
		r.cancelled = true
		r.state = "cancelled"
		close(r.cancelCh)
	}
}

// Shutdown cancels the active run and waits for it to be reaped.
func (s *Server) Shutdown(ctx context.Context) {
	s.mu.Lock()
	r := s.active
	if r != nil {
		s.cancelLocked(r)
	}
	s.mu.Unlock()
	if r != nil {
		select {
		case <-r.done:
		case <-ctx.Done():
		}
	}
}
