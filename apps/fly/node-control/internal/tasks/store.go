package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	maxReasonBytes = 512
	maxResultBytes = 8 << 10
	maxRunsField   = 256 << 10 // whole runs:<task> field, oldest runs trimmed first
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
	// Where the run lives on the node, so a restarted engine can cancel a
	// run its predecessor left behind.
	RunnerID   string `json:"runner_id,omitempty"`
	RunnerAddr string `json:"runner_addr,omitempty"`
}

type TaskState struct {
	NextRun, LastSuccess time.Time
	Streak, Attempt      int       // Attempt > 0: a retry sequence is in progress
	AttemptAt            time.Time // when the next attempt is due
	AlertUntil           time.Time // cooldown expiry timestamps
	FailNotifyUntil      time.Time
}

// Store is safe for concurrent use. A write that fails keeps the in-memory
// value (the scheduler must keep working with Redis down) and stays pending;
// every later write retries each pending field on its own, so one field that
// never succeeds cannot hold back the others.
//
// Locking: mu guards the cache and the pending set and is never held across
// Redis IO. wmu serialises the Redis writes; each flush takes its snapshot
// after acquiring wmu, so the latest value of a field always wins.
type Store struct {
	kv  ipn.StateStore
	log *slog.Logger

	wmu sync.Mutex // held for the duration of Redis writes only

	mu      sync.Mutex
	config  *Config
	scripts map[string]string
	secrets map[string]string
	states  map[string]TaskState
	runs    map[string][]Run // oldest first
	pending map[string]pendingWrite
	order   []string         // pending fields, first-queued first
	stuck   map[string]error // fields whose last write attempt failed

	flushBudget time.Duration // tests shrink it
}

type pendingWrite struct {
	val []byte
	ver uint64
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
		pending: map[string]pendingWrite{}, stuck: map[string]error{}, flushBudget: defaultFlushBudget,
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

// pendingVer orders writes of one field.
var pendingVer atomic.Uint64

// queue marks field as needing a write. The caller holds s.mu and has already
// updated the cache.
func (s *Store) queue(field string, val []byte) {
	ver := pendingVer.Add(1)
	if _, queued := s.pending[field]; !queued {
		s.order = append(s.order, field)
	}
	s.pending[field] = pendingWrite{val: val, ver: ver}
}

func (s *Store) queueJSON(field string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.queue(field, b)
	return nil
}

// defaultFlushBudget bounds one flush. Each Redis call can take up to 10 s when
// Redis is unreachable, so without a budget a writer would wait that long per
// pending field.
const defaultFlushBudget = 15 * time.Second

// isTransportErr tells "Redis cannot be reached or answered in time" from a
// per-field rejection by the server. The shared client flattens go-redis errors
// into text, so besides the error chain the text is checked too.
func isTransportErr(err error) bool {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) ||
		errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &ne) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, m := range []string{"timeout", "deadline exceeded", "connection refused", "connection reset",
		"broken pipe", "no such host", "dial tcp", "eof", "network is unreachable", "closed network connection"} {
		if strings.Contains(msg, m) {
			return true
		}
	}
	return false
}

// flush writes the pending fields, each independently, within the flush
// budget; a transport failure ends the flush at once (the other fields would
// fail the same way). Fields not attempted stay pending for the next flush. It
// is called without s.mu held. Errors never carry the Redis URL or key
// material, and values are never logged. The returned error covers only the
// fields in own (the ones the caller queued); everything else a flush could
// not write shows in Degraded/DegradedFields.
func (s *Store) flush(own ...string) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	deadline := time.Now().Add(s.flushBudget)

	type item struct {
		field string
		pw    pendingWrite
	}
	s.mu.Lock()
	todo := make([]item, 0, len(s.order))
	for _, f := range s.order {
		todo = append(todo, item{f, s.pending[f]})
	}
	s.mu.Unlock()

	var abort error
	for _, it := range todo {
		if abort == nil && !time.Now().Before(deadline) {
			abort = errors.New("flush budget spent")
		}
		if abort != nil {
			break
		}
		err := s.kv.WriteState(ipn.StateKey(it.field), it.pw.val)
		s.mu.Lock()
		if err != nil {
			if _, was := s.stuck[it.field]; !was {
				s.log.Warn("task store write failed, serving from memory", "field", it.field, "err", err)
			}
			s.stuck[it.field] = err
			if isTransportErr(err) {
				abort = err
			}
		} else {
			delete(s.stuck, it.field)
			if cur, ok := s.pending[it.field]; ok && cur.ver == it.pw.ver {
				delete(s.pending, it.field)
				for i, f := range s.order {
					if f == it.field {
						s.order = append(s.order[:i], s.order[i+1:]...)
						break
					}
				}
			}
		}
		s.mu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, f := range own {
		if _, queued := s.pending[f]; !queued {
			continue
		}
		if err, ok := s.stuck[f]; ok {
			errs = append(errs, fmt.Errorf("write %s: %w", f, err))
		} else {
			errs = append(errs, fmt.Errorf("write %s: not written yet: %v", f, abort))
		}
	}
	return errors.Join(errs...)
}

// Config is nil until a config was pushed. The result is a shared snapshot:
// callers must treat it as read-only.
func (s *Store) Config() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// PutConfig stores the config and its scripts. Scripts are reloaded at open
// only when a task references them.
func (s *Store) PutConfig(cfg *Config, scripts map[string]string) error {
	if cfg == nil {
		return errors.New("nil task config")
	}
	s.mu.Lock()
	s.config = cfg
	names := make([]string, 0, len(scripts))
	for n := range scripts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		s.scripts[n] = scripts[n]
		s.queue("script:"+n, []byte(scripts[n]))
	}
	err := s.queueJSON(fieldConfig, cfg)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	own := []string{fieldConfig}
	for _, n := range names {
		own = append(own, "script:"+n)
	}
	return s.flush(own...)
}

func (s *Store) Script(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.scripts[name]
	return t, ok
}

func (s *Store) PutSecret(name, value string) error {
	s.mu.Lock()
	_, known := s.secrets[name]
	s.secrets[name] = value
	s.queue("secret:"+name, []byte(value))
	own := []string{"secret:" + name}
	var err error
	if !known {
		err = s.queueJSON(fieldSecrets, s.namesLocked())
		own = append(own, fieldSecrets)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.flush(own...)
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
	err := s.setStateLocked(task, st)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.flush("state:" + task)
}

// UpdateState applies fn to the current state under the store lock, so two
// writers touching different fields cannot overwrite each other. The in-memory
// state is updated even when the write fails. fn must not call store methods
// (the lock is held).
func (s *Store) UpdateState(task string, fn func(*TaskState)) error {
	s.mu.Lock()
	st := s.states[task]
	fn(&st)
	err := s.setStateLocked(task, st)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return s.flush("state:" + task)
}

func (s *Store) setStateLocked(task string, st TaskState) error {
	s.states[task] = st
	return s.queueJSON("state:"+task, st)
}

// bound keeps one run small: the history lives in a single Redis field.
func (r Run) bound() Run {
	if len(r.Reason) > maxReasonBytes {
		r.Reason = strings.ToValidUTF8(r.Reason[:maxReasonBytes], "")
	}
	if r.Result != nil {
		if b, err := json.Marshal(r.Result); err != nil || len(b) > maxResultBytes {
			r.Result = map[string]any{"truncated": true}
		}
	}
	return r
}

// AppendRun records r. A run whose ID is already known replaces that entry in
// place (so queued/running/finished updates keep their position); otherwise it
// is appended. The newest maxRuns runs are kept, and the stored field is held
// under maxRunsField by dropping the oldest runs. Run.Result is stored as a
// read-only snapshot: do not mutate the map afterwards.
func (s *Store) AppendRun(r Run) error {
	r = r.bound()
	s.mu.Lock()
	rs := s.runs[r.Task]
	replaced := false
	if r.ID != "" {
		for i := range rs {
			if rs[i].ID == r.ID {
				rs = append([]Run(nil), rs...)
				rs[i] = r
				replaced = true
				break
			}
		}
	}
	if !replaced {
		rs = append(rs[:len(rs):len(rs)], r)
	}
	if len(rs) > maxRuns {
		rs = rs[len(rs)-maxRuns:]
	}
	enc := make([][]byte, len(rs))
	total := 2
	for i, x := range rs {
		b, err := json.Marshal(x)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		enc[i] = b
		total += len(b) + 1
	}
	drop := 0
	for total > maxRunsField && drop < len(rs)-1 {
		total -= len(enc[drop]) + 1
		drop++
	}
	rs, enc = rs[drop:], enc[drop:]
	s.runs[r.Task] = rs
	s.queue("runs:"+r.Task, append(append([]byte("["), bytes.Join(enc, []byte(","))...), ']'))
	s.mu.Unlock()
	return s.flush("runs:" + r.Task)
}

// Runs returns a copy of the history, newest first.
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

// Degraded reports that a write is still failing: some field could not be
// stored at its last attempt. It clears once every pending field went through.
func (s *Store) Degraded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stuck) > 0
}

// DegradedFields names the fields whose last write failed (names only).
func (s *Store) DegradedFields() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.stuck))
	for f := range s.stuck {
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
