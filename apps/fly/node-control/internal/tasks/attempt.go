package tasks

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"time"

	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/scrub"
)

// outcome is how one attempt ended, before scrubbing.
type outcome struct {
	state    RunState
	reason   string
	result   map[string]any
	shutdown bool // interrupted by shutdown: neither a success nor a failure
	operator bool // cancelled by the operator: ends the sequence quietly
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
	if out.shutdown && errors.Is(context.Cause(ctx), errCancelledByOperator) {
		out = outcome{state: RunCancelled, reason: errCancelledByOperator.Error(), operator: true}
	}
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
	// Longest first, once, for every scrub of this attempt: a secret that is
	// a substring of another must not leave the longer one's tail behind.
	sort.Slice(*values, func(i, j int) bool { return len((*values)[i]) > len((*values)[j]) })
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
	// Recorded before polling so a successor process can cancel the run if
	// this one dies (see sweep).
	run.RunnerID, run.RunnerAddr = rid, cand.Addr
	e.setActive(*run)
	if err := e.d.Store.AppendRun(*run); err != nil {
		e.log.Warn("task run record not stored", "task", task, "run", run.ID, "err", err)
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
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), e.cancelBudget)
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
		run.State, run.Reason = RunFailed, reasonShutdown
	}
	e.setActive(run)
	e.log.Info("attempt_result", "task", task, "run", run.ID, "node", run.Node, "attempt", run.Attempt,
		"state", string(run.State), "dur_ms", now.Sub(run.Started).Milliseconds(), "reason", run.Reason)
	e.d.Metrics.Observe(task, run.Node, run.State, now.Sub(run.Started))

	nctx, ncancel := context.WithTimeout(context.WithoutCancel(ctx), notifyTimeout)
	defer ncancel()
	// A manual sequence never moves a future NextRun, but a stale or zero one
	// is advanced: left alone it would start a scheduled sequence right after.
	reschedule := func(s *TaskState) {
		if !manual || !s.NextRun.After(now) {
			s.NextRun = e.nextRun(now, t.Schedule)
		}
	}
	switch {
	case out.shutdown:
		// Not counted: Attempt/AttemptAt stay, so the next start retries the
		// same attempt number, and nobody is told about a deploy.
	case out.operator:
		// The operator ended the sequence: no retry, no streak, no message.
		e.updateState(task, func(s *TaskState) {
			s.Attempt, s.AttemptAt = 0, time.Time{}
			reschedule(s)
		})
		e.endSequence(task)
	case run.State == RunSucceeded:
		e.updateState(task, func(s *TaskState) {
			s.LastSuccess, s.Streak, s.Attempt, s.AttemptAt = now, 0, 0, time.Time{}
			reschedule(s)
		})
		e.endSequence(task)
		run.NotifyOutcome = notifyText(e.d.Notifier.Success(nctx, task, t, run.Result))
	case run.Attempt < t.Retry.Attempts:
		at := now.Add(Backoff(t.Retry, run.Attempt))
		e.updateState(task, func(s *TaskState) { s.Attempt, s.AttemptAt = run.Attempt, at })
		e.log.Info("retry_scheduled", "task", task, "run", run.ID, "node", run.Node, "attempt", run.Attempt,
			"state", string(run.State), "reason", run.Reason)
	default:
		e.updateState(task, func(s *TaskState) {
			s.Streak++
			s.Attempt, s.AttemptAt = 0, time.Time{}
			reschedule(s)
		})
		e.endSequence(task)
		run.NotifyOutcome = notifyText(e.d.Notifier.Failure(nctx, task, t, run.Reason))
	}
	if err := e.d.Store.AppendRun(run); err != nil {
		e.log.Warn("task run record not stored", "task", task, "run", run.ID, "err", err)
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

// scrubResult copies the result with secret values (sorted longest first by
// execute) replaced inside string leaves and keys. Numbers and structure are
// kept as they are, and nothing is truncated: the store bounds the whole
// result instead.
func scrubResult(m map[string]any, secrets []string) map[string]any {
	if m == nil {
		return nil
	}
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
