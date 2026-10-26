package tasks

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"
)

// The shared Redis is billed per command, so the store reads every field once
// at open, serves everything from memory afterwards and only ever writes (one
// HSET per changed field). It talks through ReadState/WriteState only, which
// keeps the command set at HGET and HSET.
//
// Hash fields (each value sealed by the wrapper, bound to its field name):
//
//	config               the Config as JSON
//	script:<name>        script text
//	secret:<name>        secret value
//	secrets-index        JSON list of secret names
//	state:<task>         TaskState as JSON
//	runs:<task>          JSON array of Run, oldest first, capped at maxRuns

const (
	fieldConfig  = "config"
	fieldSecrets = "secrets-index"
	maxRuns      = 200
)

type RunState string

const (
	RunQueued    RunState = "queued"
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunLost      RunState = "lost"
	RunCancelled RunState = "cancelled"
)

type Run struct {
	ID, Task, Node string
	Attempt        int
	State          RunState
	Reason         string
	Started, Ended time.Time
	Result         map[string]any
	ScriptSHA      string
	NotifyOutcome  string
}

type TaskState struct {
	NextRun, LastSuccess time.Time
	Streak, Attempt      int       // Attempt > 0: a retry sequence is in progress
	AttemptAt            time.Time // when the next attempt is due
	AlertUntil           time.Time // cooldown expiry timestamps
	FailNotifyUntil      time.Time
}

// Store is safe for concurrent use. A write that fails keeps the in-memory
// value (the scheduler must keep working with Redis down) and is queued, so
// the next write retries every field that is still behind.
type Store struct {
	kv  ipn.StateStore
	log *slog.Logger

	mu       sync.Mutex
	config   *Config
	scripts  map[string]string
	secrets  map[string]string
	states   map[string]TaskState
	runs     map[string][]Run  // oldest first
	pending  map[string][]byte // fields not yet written, latest value
	order    []string          // pending fields, in first-failure order
	degraded bool
}

// OpenStore reads config, scripts, secrets, state and history once.
func OpenStore(kv ipn.StateStore, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{
		kv: kv, log: log,
		scripts: map[string]string{}, secrets: map[string]string{},
		states: map[string]TaskState{}, runs: map[string][]Run{},
		pending: map[string][]byte{},
	}

	if b, ok, err := s.read(fieldConfig); err != nil {
		return nil, err
	} else if ok {
		if s.config, err = ParseConfig(b); err != nil {
			return nil, fmt.Errorf("stored task config: %w", err)
		}
	}

	var names []string
	if b, ok, err := s.read(fieldSecrets); err != nil {
		return nil, err
	} else if ok {
		if err := json.Unmarshal(b, &names); err != nil {
			return nil, fmt.Errorf("stored %s: %w", fieldSecrets, err)
		}
	}
	for _, n := range names {
		b, ok, err := s.read("secret:" + n)
		if err != nil {
			return nil, err
		}
		if ok {
			s.secrets[n] = string(b)
		}
	}

	if s.config != nil {
		for id, t := range s.config.Tasks {
			if _, done := s.scripts[t.Script]; !done {
				if b, ok, err := s.read("script:" + t.Script); err != nil {
					return nil, err
				} else if ok {
					s.scripts[t.Script] = string(b)
				}
			}
			if b, ok, err := s.read("state:" + id); err != nil {
				return nil, err
			} else if ok {
				var st TaskState
				if err := json.Unmarshal(b, &st); err != nil {
					return nil, fmt.Errorf("stored state of task %s: %w", id, err)
				}
				s.states[id] = st
			}
			if b, ok, err := s.read("runs:" + id); err != nil {
				return nil, err
			} else if ok {
				var rs []Run
				if err := json.Unmarshal(b, &rs); err != nil {
					return nil, fmt.Errorf("stored history of task %s: %w", id, err)
				}
				s.runs[id] = rs
			}
		}
	}
	return s, nil
}

// read treats a missing field as absent, not as an error.
func (s *Store) read(field string) ([]byte, bool, error) {
	b, err := s.kv.ReadState(ipn.StateKey(field))
	if errors.Is(err, ipn.ErrStateNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", field, err)
	}
	return b, true, nil
}

// put records field as dirty and flushes everything dirty. The caller holds
// s.mu and has already updated the cache. Errors from the kv wrapper never
// carry the Redis URL or key material, and values are never logged.
func (s *Store) put(field string, val []byte) error {
	if _, queued := s.pending[field]; !queued {
		s.order = append(s.order, field)
	}
	s.pending[field] = val
	for len(s.order) > 0 {
		f := s.order[0]
		if err := s.kv.WriteState(ipn.StateKey(f), s.pending[f]); err != nil {
			if !s.degraded {
				s.log.Warn("task store write failed, serving from memory", "field", f, "err", err)
			}
			s.degraded = true
			return fmt.Errorf("write %s: %w", f, err)
		}
		delete(s.pending, f)
		s.order = s.order[1:]
	}
	s.degraded = false
	return nil
}

func (s *Store) putJSON(field string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.put(field, b)
}

// Config is nil until a config was pushed.
func (s *Store) Config() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// PutConfig stores the config and its scripts. Scripts are reloaded at open
// only when a task references them.
func (s *Store) PutConfig(cfg *Config, scripts map[string]string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config = cfg
	var first error
	keep := func(err error) {
		if err != nil && first == nil {
			first = err
		}
	}
	names := make([]string, 0, len(scripts))
	for n := range scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s.scripts[n] = scripts[n]
		keep(s.put("script:"+n, []byte(scripts[n])))
	}
	keep(s.putJSON(fieldConfig, cfg))
	return first
}

func (s *Store) Script(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.scripts[name]
	return t, ok
}

func (s *Store) PutSecret(name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, known := s.secrets[name]
	s.secrets[name] = value
	first := s.put("secret:"+name, []byte(value))
	if !known {
		if err := s.putJSON(fieldSecrets, s.namesLocked()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Store) namesLocked() []string {
	names := make([]string, 0, len(s.secrets))
	for n := range s.secrets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SecretNames never returns values.
func (s *Store) SecretNames() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.namesLocked()
}

// Secrets returns the named values; the error lists the missing names only.
func (s *Store) Secrets(names []string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(names))
	var missing []string
	for _, n := range names {
		if v, ok := s.secrets[n]; ok {
			out[n] = v
		} else {
			missing = append(missing, n)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing secrets: %s", strings.Join(missing, ", "))
	}
	return out, nil
}

func (s *Store) State(task string) TaskState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.states[task]
}

func (s *Store) SaveState(task string, st TaskState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saveStateLocked(task, st)
}

// UpdateState applies fn to the current state under the store lock, so two
// writers touching different fields cannot overwrite each other. The in-memory
// state is updated even when the write fails.
func (s *Store) UpdateState(task string, fn func(*TaskState)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.states[task]
	fn(&st)
	return s.saveStateLocked(task, st)
}

func (s *Store) saveStateLocked(task string, st TaskState) error {
	s.states[task] = st
	return s.putJSON("state:"+task, st)
}

// AppendRun keeps the newest maxRuns runs of the task.
func (s *Store) AppendRun(r Run) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := append(s.runs[r.Task], r)
	if len(rs) > maxRuns {
		rs = append([]Run(nil), rs[len(rs)-maxRuns:]...)
	}
	s.runs[r.Task] = rs
	return s.putJSON("runs:"+r.Task, rs)
}

// Runs returns a copy, newest first.
func (s *Store) Runs(task string) []Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.runs[task]
	out := make([]Run, len(rs))
	for i, r := range rs {
		out[len(rs)-1-i] = r
	}
	return out
}

// Degraded reports that the last write failed.
func (s *Store) Degraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.degraded
}
