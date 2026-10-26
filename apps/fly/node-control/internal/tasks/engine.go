package tasks

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"sort"
	"sync"
	"time"

	"example.com/node-control/internal/headscale"
)

var (
	// ErrAlreadyRunning: an attempt of the task is executing right now.
	ErrAlreadyRunning = errors.New("task is already running")
	// ErrUnknownTask: the task is not in the current config.
	ErrUnknownTask = errors.New("unknown task")
	// ErrStopped: the engine has shut down and accepts no new work.
	ErrStopped = errors.New("task engine stopped")
	// ErrRunNotFound: no attempt with that run id is executing in this engine.
	ErrRunNotFound = errors.New("run is not executing")

	// errCancelledByOperator is the cause of an attempt context cancelled by
	// Cancel, which tells it apart from a shutdown.
	errCancelledByOperator = errors.New("cancelled by operator")
)

// Reasons the engine writes on runs cut short by node-control itself; the
// API's stale rule (realFailure) compares against them.
const (
	reasonShutdown  = "shutdown"
	reasonRestarted = "node-control restarted"
)

const (
	defaultPollEvery   = 5 * time.Second
	defaultStartSpread = 10 * time.Second

	// A run still reported as running this long past its timeout is cancelled:
	// the runner enforces the timeout itself, so this only catches a runner that
	// failed to.
	cancelMargin = 10 * time.Second
	// A runner that cannot be reached this long past the timeout is given up on.
	lostMargin = 30 * time.Second

	// The loop re-reads the schedule at least this often, so a wall-clock jump
	// (the timer runs on the monotonic clock) cannot delay a run for long. A
	// tick only reads memory: no Redis, no node listing.
	maxSleep = 5 * time.Minute

	// Shutdown must fit the platform's kill timeout (25 s): wait for attempts
	// at most this long, and give each node Cancel this much of it.
	defaultShutdownWait = 12 * time.Second
	defaultCancelBudget = 3 * time.Second

	// The startup sweep cancels runs a previous process left behind.
	sweepCancelEach  = 5 * time.Second
	sweepCancelTotal = 30 * time.Second

	// Notifications outlive a shutdown: the state change they report (for
	// example Streak++) is already stored.
	notifyTimeout = 60 * time.Second
)

// Deps are the engine's collaborators. Now and Rand default to the wall clock
// and math/rand; tests replace them to make schedules deterministic.
type Deps struct {
	Store     *Store
	Nodes     headscale.NodeSource
	Runner    Runner
	Notifier  Notifier
	Now       func() time.Time
	Rand      func() float64 // [0,1)
	PollEvery time.Duration  // Status poll interval; default 5 s
	Log       *slog.Logger
}

// Engine schedules task attempts and drives them on the nodes.
//
// Nothing sleeps inside a sequence: after a failed attempt with attempts left
// the engine persists Attempt/AttemptAt and returns, and the loop wakes at
// AttemptAt. Every due time therefore lives in the store, which is what lets a
// restarted process resume a backoff exactly where it was.
//
// Locking: mu guards the in-memory maps only and is never held across a call
// into the store, the runner or the notifier (store writes can block for
// seconds while Redis is down).
type Engine struct {
	d    Deps
	log  *slog.Logger
	wake chan struct{}
	wg   sync.WaitGroup

	// Tests shrink these.
	startSpread  time.Duration // per-task random delay of startup runs; tests set 0
	shutdownWait time.Duration
	cancelBudget time.Duration

	// base is the parent of sequences started by Trigger; Run's shutdown
	// cancels it, also for a Trigger that came before Run.
	base       context.Context
	cancelBase context.CancelFunc

	mu        sync.Mutex
	closed    bool
	active    map[string]*Run                    // task -> record of the attempt executing now
	busy      map[string]int                     // node name -> attempts executing on it
	manual    map[string]bool                    // task -> its current sequence was started by Trigger
	stops     map[string]context.CancelCauseFunc // task -> cancels the executing attempt
	notBefore map[string]time.Time               // task -> startup delay end
}

func NewEngine(d Deps) *Engine {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Rand == nil {
		d.Rand = mrand.Float64
	}
	if d.PollEvery <= 0 {
		d.PollEvery = defaultPollEvery
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	base, cancel := context.WithCancel(context.Background())
	return &Engine{
		d: d, log: log, wake: make(chan struct{}, 1),
		startSpread: defaultStartSpread, shutdownWait: defaultShutdownWait, cancelBudget: defaultCancelBudget,
		base: base, cancelBase: cancel,
		active: map[string]*Run{}, busy: map[string]int{}, manual: map[string]bool{},
		stops: map[string]context.CancelCauseFunc{}, notBefore: map[string]time.Time{},
	}
}

// Backoff is the wait after failed attempt number attempt (1-based). Beyond the
// list the last value repeats; an empty list means no wait.
func Backoff(r Retry, attempt int) time.Duration {
	if len(r.Backoff) == 0 {
		return 0
	}
	i := min(max(attempt-1, 0), len(r.Backoff)-1)
	return time.Duration(r.Backoff[i])
}

// Run drives the schedule until ctx is done, then waits (bounded) for the
// executing attempts, which see ctx cancelled and record themselves as
// interrupted. An Engine runs once: after Run returns it accepts no new work.
func (e *Engine) Run(ctx context.Context) {
	defer e.shutdown()
	stop := context.AfterFunc(ctx, e.cancelBase)
	defer stop()

	e.sweep(ctx)
	e.spreadStart()
	for {
		next := e.tick(ctx)
		d := maxSleep
		if !next.IsZero() {
			d = min(max(next.Sub(e.d.Now()), 0), maxSleep)
		}
		t := time.NewTimer(d)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-e.wake:
		case <-t.C:
		}
		t.Stop()
	}
}

func (e *Engine) shutdown() {
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	e.cancelBase()
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	t := time.NewTimer(e.shutdownWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		e.log.Warn("task attempts still running at shutdown")
	}
}

func sortedTasks(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Tasks))
	for n := range cfg.Tasks {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Reload makes the loop recompute its wake time, after a config push or when
// an attempt ends.
func (e *Engine) Reload() {
	select {
	case e.wake <- struct{}{}:
	default:
	}
}

// Running returns the record of the attempt executing now, for the API and UI.
func (e *Engine) Running(task string) (Run, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	r, ok := e.active[task]
	if !ok {
		return Run{}, false
	}
	return *r, true
}

// Trigger starts an attempt now. A pending retry is dropped and the sequence
// restarts at attempt 1. NextRun is never touched by a manual sequence.
func (e *Engine) Trigger(task string) (string, error) {
	cfg := e.d.Store.Config()
	if cfg == nil {
		return "", ErrUnknownTask
	}
	if _, ok := cfg.Tasks[task]; !ok {
		return "", ErrUnknownTask
	}
	id := newRunID()
	if err := e.claim(task, id, true); err != nil {
		return "", err
	}
	if st := e.d.Store.State(task); st.Attempt > 0 {
		e.updateState(task, func(s *TaskState) { s.Attempt, s.AttemptAt = 0, time.Time{} })
	}
	e.launch(e.base, task, id, 1, true)
	return id, nil
}

func dueAt(st TaskState) time.Time {
	if st.Attempt > 0 {
		return st.AttemptAt
	}
	return st.NextRun // zero: never ran, due at once
}

// tick launches every task due at Now() and returns the earliest future due
// time among the tasks it did not launch (zero when there is none). Executing
// tasks are skipped: their end wakes the loop.
func (e *Engine) tick(ctx context.Context) time.Time {
	cfg := e.d.Store.Config()
	if cfg == nil {
		return time.Time{}
	}
	names := sortedTasks(cfg)
	now := e.d.Now()
	var next time.Time
	later := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, task := range names {
		if at := e.due(task, e.d.Store.State(task)); now.Before(at) {
			later(at)
			continue
		}
		id := newRunID()
		if e.claim(task, id, false) != nil {
			continue
		}
		// Read again under the claim: a sequence that ended between the first
		// read and the claim has moved the due time and must not fire twice.
		st := e.d.Store.State(task)
		if at := e.due(task, st); now.Before(at) {
			e.unclaim(task)
			later(at)
			continue
		}
		e.launch(ctx, task, id, st.Attempt+1, e.continues(task, st.Attempt))
	}
	return next
}

// wait blocks until every launched attempt has ended.
func (e *Engine) wait() { e.wg.Wait() }

// claim reserves the task for one attempt. The WaitGroup is incremented under
// mu together with the closed check, so nothing is added once shutdown waits.
func (e *Engine) claim(task, id string, manual bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return ErrStopped
	}
	if _, ok := e.active[task]; ok {
		return ErrAlreadyRunning
	}
	e.active[task] = &Run{ID: id, Task: task, State: RunQueued}
	if manual {
		e.manual[task] = true
	}
	e.wg.Add(1)
	return nil
}

func (e *Engine) unclaim(task string) {
	e.mu.Lock()
	delete(e.active, task)
	e.mu.Unlock()
	e.wg.Done()
}

// continues reports whether the attempt about to start belongs to a manual
// sequence. Attempt 0 starts a fresh scheduled sequence. The flag is memory
// only: after a restart a pending manual retry finishes as a scheduled one.
func (e *Engine) continues(task string, attempt int) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if attempt == 0 {
		delete(e.manual, task)
		return false
	}
	return e.manual[task]
}

func (e *Engine) endSequence(task string) {
	e.mu.Lock()
	delete(e.manual, task)
	e.mu.Unlock()
}

func (e *Engine) launch(ctx context.Context, task, id string, n int, manual bool) {
	actx, stop := context.WithCancelCause(ctx)
	e.mu.Lock()
	e.stops[task] = stop
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.active, task)
			delete(e.stops, task)
			e.mu.Unlock()
			stop(nil)
			e.Reload()
		}()
		e.attempt(actx, task, id, n, manual)
	}()
}

// Cancel stops the executing attempt with that run id. The attempt cancels the
// run on its node (best effort), records it as cancelled and ends its sequence:
// no retry, no streak, no failure notification.
func (e *Engine) Cancel(runID string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for task, r := range e.active {
		if r.ID != runID {
			continue
		}
		stop, ok := e.stops[task]
		if !ok {
			break // claimed but not launched yet
		}
		stop(errCancelledByOperator)
		return nil
	}
	return ErrRunNotFound
}

func (e *Engine) setActive(r Run) {
	e.mu.Lock()
	if cur, ok := e.active[r.Task]; ok {
		*cur = r
	}
	e.mu.Unlock()
}

func (e *Engine) addBusy(node string, d int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.busy[node] += d; e.busy[node] <= 0 {
		delete(e.busy, node)
	}
}

func (e *Engine) busySnapshot() map[string]int {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]int, len(e.busy))
	for k, v := range e.busy {
		out[k] = v
	}
	return out
}

func (e *Engine) updateState(task string, fn func(*TaskState)) {
	// The in-memory state is updated even when the write fails, so the
	// schedule keeps going with Redis down; the store retries the write.
	if err := e.d.Store.UpdateState(task, fn); err != nil {
		e.log.Warn("task state not stored", "task", task, "err", err)
	}
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
