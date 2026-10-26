package tasks

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	mrand "math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/scrub"
)

var (
	// ErrAlreadyRunning: an attempt of the task is executing right now.
	ErrAlreadyRunning = errors.New("task is already running")
	// ErrUnknownTask: the task is not in the current config.
	ErrUnknownTask = errors.New("unknown task")

	errStopped = errors.New("task engine stopped")
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
	maxSleep      = 5 * time.Minute
	shutdownWait  = 30 * time.Second
	cancelTimeout = 5 * time.Second
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
	d           Deps
	log         *slog.Logger
	startSpread time.Duration // random delay before the first tick; tests set 0
	wake        chan struct{}
	wg          sync.WaitGroup

	mu     sync.Mutex
	base   context.Context // parent of sequences started by Trigger
	closed bool
	active map[string]*Run // task -> record of the attempt executing now
	busy   map[string]int  // node name -> attempts executing on it
	manual map[string]bool // task -> its current sequence was started by Trigger
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
	return &Engine{
		d: d, log: log, startSpread: defaultStartSpread,
		wake:   make(chan struct{}, 1),
		base:   context.Background(),
		active: map[string]*Run{}, busy: map[string]int{}, manual: map[string]bool{},
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
	e.mu.Lock()
	e.base = ctx
	e.mu.Unlock()
	defer e.shutdown()

	// Overdue tasks found at startup run within seconds, spread a little so a
	// restart does not hit every node at the same instant.
	if e.startSpread > 0 {
		t := time.NewTimer(time.Duration(e.d.Rand() * float64(e.startSpread)))
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
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
	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	t := time.NewTimer(shutdownWait)
	defer t.Stop()
	select {
	case <-done:
	case <-t.C:
		e.log.Warn("task attempts still running at shutdown")
	}
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
	e.mu.Lock()
	ctx := e.base
	e.mu.Unlock()
	e.launch(ctx, task, id, 1, true)
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
	names := make([]string, 0, len(cfg.Tasks))
	for n := range cfg.Tasks {
		names = append(names, n)
	}
	sort.Strings(names)

	now := e.d.Now()
	var next time.Time
	later := func(at time.Time) {
		if next.IsZero() || at.Before(next) {
			next = at
		}
	}
	for _, task := range names {
		if at := dueAt(e.d.Store.State(task)); now.Before(at) {
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
		if at := dueAt(st); now.Before(at) {
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
		return errStopped
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
	go func() {
		defer e.wg.Done()
		defer func() {
			e.mu.Lock()
			delete(e.active, task)
			e.mu.Unlock()
			e.Reload()
		}()
		e.attempt(ctx, task, id, n, manual)
	}()
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

// outcome is how one attempt ended, before scrubbing.
type outcome struct {
	state    RunState
	reason   string
	result   map[string]any
	shutdown bool // interrupted by shutdown: neither a success nor a failure
}

func failed(reason string) outcome { return outcome{state: RunFailed, reason: reason} }
func lost(reason string) outcome   { return outcome{state: RunLost, reason: reason} }

// attempt runs attempt n of the task and records its consequences.
func (e *Engine) attempt(ctx context.Context, task, id string, n int, manual bool) {
	cfg := e.d.Store.Config()
	if cfg == nil {
		return
	}
	t, ok := cfg.Tasks[task]
	if !ok {
		return // removed by a config push since it became due
	}
	run := Run{ID: id, Task: task, Attempt: n, State: RunQueued, Started: e.d.Now()}
	e.setActive(run)
	e.log.Info("run_started", "task", task, "run", id, "attempt", n)

	var secretValues []string
	out := e.execute(ctx, task, t, &run, &secretValues)
	e.finish(ctx, task, t, run, out, secretValues, manual)
}

// execute prepares the attempt, picks a node and drives the run to an end. It
// fills in run.Node and run.ScriptSHA and the secret values to scrub.
func (e *Engine) execute(ctx context.Context, task string, t Task, run *Run, values *[]string) outcome {
	script, ok := e.d.Store.Script(t.Script)
	if !ok {
		return failed("script not found: " + t.Script)
	}
	secrets, err := e.d.Store.Secrets(t.Secrets)
	if err != nil {
		return failed("secret not configured: " + strings.Join(e.missingSecrets(t.Secrets), ", "))
	}
	for _, v := range secrets {
		*values = append(*values, v)
	}
	sum := sha256.Sum256([]byte(script))
	run.ScriptSHA = hex.EncodeToString(sum[:])

	nodes, err := e.d.Nodes.Nodes(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return outcome{shutdown: true}
		}
		return failed("cannot list nodes")
	}
	cand, err := Select(t.Target, nodes, e.healthProbe(ctx), e.busySnapshot(), lastOK(e.d.Store.Runs(task)))
	if err != nil {
		if ctx.Err() != nil {
			return outcome{shutdown: true}
		}
		return failed("no eligible node")
	}
	node := cand.Node.Name
	e.addBusy(node, 1)
	defer e.addBusy(node, -1)

	run.Node, run.State = node, RunRunning
	e.setActive(*run)
	// Upserted now so the UI shows the attempt while it runs.
	if err := e.d.Store.AppendRun(*run); err != nil {
		e.log.Warn("task run record not stored", "task", task, "run", run.ID, "err", err)
	}
	e.log.Info("node_chosen", "task", task, "run", run.ID, "node", node, "attempt", run.Attempt)

	started := e.d.Now()
	rid, err := e.d.Runner.Start(ctx, cand.Addr, StartReq{
		Script: script, Params: t.Params, Secrets: secrets,
		TimeoutSeconds: int(time.Duration(t.Timeout).Seconds()),
	})
	if err != nil {
		switch {
		case ctx.Err() != nil:
			return outcome{shutdown: true}
		case errors.Is(err, ErrRunnerBusy):
			return failed("node busy")
		}
		return failed(err.Error())
	}
	return e.poll(ctx, t, cand.Addr, rid, started)
}

// poll follows the run until it ends. Deadlines compare Deps.Now so tests can
// move the clock; the poll interval itself is real time.
func (e *Engine) poll(ctx context.Context, t Task, addr, rid string, started time.Time) outcome {
	timeout := time.Duration(t.Timeout)
	cancelAt := started.Add(timeout + cancelMargin)
	lostAt := started.Add(timeout + lostMargin)
	tk := time.NewTicker(e.d.PollEvery)
	defer tk.Stop()
	for {
		select {
		case <-ctx.Done():
			e.cancelRun(ctx, addr, rid)
			return outcome{shutdown: true}
		case <-tk.C:
		}
		st, err := e.d.Runner.Status(ctx, addr, rid)
		now := e.d.Now()
		if errors.Is(err, ErrRunUnknown) {
			return lost("runner lost the run")
		}
		if err != nil {
			if ctx.Err() != nil {
				e.cancelRun(ctx, addr, rid)
				return outcome{shutdown: true}
			}
			// Tolerated: the node may be re-establishing its tailnet link.
			if !now.Before(lostAt) {
				return lost("runner unreachable")
			}
			continue
		}
		switch st.State {
		case "succeeded":
			return outcome{state: RunSucceeded, result: st.Result}
		case "failed":
			if st.Error == "" {
				return failed("script failed")
			}
			return failed(st.Error)
		case "cancelled":
			return failed("cancelled on the node")
		}
		if !now.Before(cancelAt) {
			e.cancelRun(ctx, addr, rid)
			return failed("timeout")
		}
	}
}

// cancelRun is best effort and must work while ctx is already cancelled.
func (e *Engine) cancelRun(ctx context.Context, addr, rid string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cancelTimeout)
	defer cancel()
	_ = e.d.Runner.Cancel(cctx, addr, rid)
}

// healthProbe asks each node's runner at most once per attempt.
func (e *Engine) healthProbe(ctx context.Context) func(headscale.Node) bool {
	memo := map[string]bool{}
	return func(n headscale.Node) bool {
		addr, _ := firstIPv4(n)
		if v, ok := memo[addr]; ok {
			return v
		}
		ok, err := e.d.Runner.Health(ctx, addr)
		memo[addr] = err == nil && ok
		return memo[addr]
	}
}

func lastOK(runs []Run) string {
	for _, r := range runs { // newest first
		if r.State == RunSucceeded {
			return r.Node
		}
	}
	return ""
}

// missingSecrets names the listed secrets that are not stored. Only names are
// returned: they are safe to show, values never are.
func (e *Engine) missingSecrets(names []string) []string {
	known := map[string]bool{}
	for _, n := range e.d.Store.SecretNames() {
		known[n] = true
	}
	var out []string
	for _, n := range names {
		if !known[n] {
			out = append(out, n)
		}
	}
	if len(out) == 0 { // stored in between; still name what was asked for
		return names
	}
	return out
}

// finish scrubs and stores the attempt and applies its effect on the task
// state. State changes go through UpdateState so the notifier's cooldown
// fields, written concurrently, are never overwritten.
func (e *Engine) finish(ctx context.Context, task string, t Task, run Run, out outcome, values []string, manual bool) {
	now := e.d.Now()
	run.Ended = now
	run.Result = scrubResult(out.result, values)
	run.State, run.Reason = out.state, ""
	if out.reason != "" {
		run.Reason = scrub.Scrub(out.reason, values)
	}
	if run.State == RunSucceeded {
		// A result the alert cannot evaluate is a failure, not a silent pass;
		// the error names the field, never its value.
		if err := CheckResult(t, run.Result); err != nil {
			run.State, run.Reason = RunFailed, scrub.Scrub("bad result: "+err.Error(), values)
		}
	}
	if out.shutdown {
		run.State, run.Reason = RunFailed, "shutdown"
	}
	e.setActive(run)
	e.log.Info("attempt_result", "task", task, "run", run.ID, "node", run.Node, "attempt", run.Attempt,
		"state", string(run.State), "dur_ms", now.Sub(run.Started).Milliseconds(), "reason", run.Reason)

	switch {
	case out.shutdown:
		// Not counted: Attempt/AttemptAt stay, so the next start retries the
		// same attempt number, and nobody is told about a deploy.
	case run.State == RunSucceeded:
		e.updateState(task, func(s *TaskState) {
			s.LastSuccess, s.Streak, s.Attempt, s.AttemptAt = now, 0, 0, time.Time{}
			if !manual {
				s.NextRun = e.nextRun(now, t.Schedule)
			}
		})
		e.endSequence(task)
		run.NotifyOutcome = notifyText(e.d.Notifier.Success(ctx, task, t, run.Result))
	case run.Attempt < t.Retry.Attempts:
		at := now.Add(Backoff(t.Retry, run.Attempt))
		e.updateState(task, func(s *TaskState) { s.Attempt, s.AttemptAt = run.Attempt, at })
		e.log.Info("retry_scheduled", "task", task, "run", run.ID, "node", run.Node, "attempt", run.Attempt,
			"state", string(run.State), "reason", run.Reason)
	default:
		e.updateState(task, func(s *TaskState) {
			s.Streak++
			s.Attempt, s.AttemptAt = 0, time.Time{}
			if !manual {
				s.NextRun = e.nextRun(now, t.Schedule)
			}
		})
		e.endSequence(task)
		run.NotifyOutcome = notifyText(e.d.Notifier.Failure(ctx, task, t, run.Reason))
	}
	if err := e.d.Store.AppendRun(run); err != nil {
		e.log.Warn("task run record not stored", "task", task, "run", run.ID, "err", err)
	}
}

func (e *Engine) updateState(task string, fn func(*TaskState)) {
	// The in-memory state is updated even when the write fails, so the
	// schedule keeps going with Redis down; the store retries the write.
	if err := e.d.Store.UpdateState(task, fn); err != nil {
		e.log.Warn("task state not stored", "task", task, "err", err)
	}
}

// nextRun is now + every ± jitter (uniform), never sooner than every/2 so a
// large jitter cannot make a task run back to back.
func (e *Engine) nextRun(now time.Time, s Schedule) time.Time {
	every := time.Duration(s.Every)
	d := every + time.Duration((e.d.Rand()*2-1)*float64(s.Jitter))
	return now.Add(max(d, every/2))
}

func notifyText(r NotifyResult) string {
	var parts []string
	if r.Alert != "" {
		parts = append(parts, "alert="+r.Alert)
	}
	if r.Ping != "" {
		parts = append(parts, "ping="+r.Ping)
	}
	return strings.Join(parts, " ")
}

// scrubResult copies the result with secret values replaced inside string
// leaves (and keys). Numbers and structure are kept as they are, and nothing is
// truncated: the store bounds the whole result instead.
func scrubResult(m map[string]any, secrets []string) map[string]any {
	if m == nil {
		return nil
	}
	// Longest first, so a secret containing another is replaced whole.
	secrets = append([]string(nil), secrets...)
	sort.Slice(secrets, func(i, j int) bool { return len(secrets[i]) > len(secrets[j]) })
	return scrubValue(m, secrets).(map[string]any)
}

func scrubValue(v any, secrets []string) any {
	switch x := v.(type) {
	case string:
		for _, s := range secrets {
			if s != "" {
				x = strings.ReplaceAll(x, s, "***")
			}
		}
		return x
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, vv := range x {
			out[scrubValue(k, secrets).(string)] = scrubValue(vv, secrets)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, vv := range x {
			out[i] = scrubValue(vv, secrets)
		}
		return out
	}
	return v
}

func newRunID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
