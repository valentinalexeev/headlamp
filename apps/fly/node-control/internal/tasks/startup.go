package tasks

import (
	"context"
	"net/netip"
	"time"
)

// sweep finalises runs a previous process left as running or queued (it was
// killed mid-attempt): each is cancelled on its node when the record says
// where it lives, then recorded as lost. It never notifies and never touches
// TaskState, so the persisted Attempt/AttemptAt retry the same attempt number.
//
// A Cancel carries the runner token, so it goes only to a stored address that
// still belongs to a node with the runner tag (the same filter as an attempt):
// the address may have moved to another node since the record was written. If
// the node list cannot be fetched no Cancel is sent at all; the records are
// finalised either way.
func (e *Engine) sweep(ctx context.Context) {
	cfg := e.d.Store.Config()
	if cfg == nil {
		return
	}
	total, cancel := context.WithTimeout(context.WithoutCancel(ctx), sweepCancelTotal)
	defer cancel()
	var tagged map[string]bool // nil until needed; empty when the list failed
	skipped := false
	for task := range cfg.Tasks {
		for _, r := range e.d.Store.Runs(task) {
			if r.State != RunRunning && r.State != RunQueued {
				continue
			}
			e.mu.Lock()
			cur, mine := e.active[task]
			mine = mine && cur.ID == r.ID // started by a Trigger before Run
			e.mu.Unlock()
			if mine {
				continue
			}
			if r.RunnerID != "" && r.RunnerAddr != "" && total.Err() == nil {
				if tagged == nil {
					tagged = e.taggedAddrs(total)
				}
				if a, err := netip.ParseAddr(r.RunnerAddr); err == nil && tagged[a.String()] {
					cctx, cc := context.WithTimeout(total, sweepCancelEach)
					_ = e.d.Runner.Cancel(cctx, r.RunnerAddr, r.RunnerID)
					cc()
				} else if !skipped {
					skipped = true
					e.log.Info("ghost runs not cancelled on their node: the stored address is not a node with the runner tag now (or the node list is unavailable); they are recorded as lost",
						"tag", e.d.RunnerTag)
				}
			}
			r.State, r.Reason, r.Ended = RunLost, reasonRestarted, e.d.Now()
			if err := e.d.Store.AppendRun(r); err != nil {
				e.log.Warn("task run record not stored", "task", task, "run", r.ID, "err", err)
			}
			e.log.Info("attempt_result", "task", task, "run", r.ID, "node", r.Node, "attempt", r.Attempt,
				"state", string(r.State), "reason", r.Reason)
		}
	}
}

// taggedAddrs lists the tailnet addresses of the nodes that carry the runner
// tag. An error yields an empty set, so nothing is cancelled (fail closed).
func (e *Engine) taggedAddrs(ctx context.Context) map[string]bool {
	out := map[string]bool{}
	nodes, err := e.d.Nodes.Nodes(ctx)
	if err != nil {
		return out
	}
	for _, n := range WithTag(nodes, e.d.RunnerTag) {
		for _, s := range n.IPAddresses {
			if a, err := netip.ParseAddr(s); err == nil {
				out[a.String()] = true
			}
		}
	}
	return out
}

// spreadStart gives every task its own random startup delay, so tasks that
// are overdue at a restart do not all start at once (two of them may resolve
// to the same node).
func (e *Engine) spreadStart() {
	if e.startSpread <= 0 {
		return
	}
	cfg := e.d.Store.Config()
	if cfg == nil {
		return
	}
	now := e.d.Now()
	nb := map[string]time.Time{}
	for _, task := range sortedTasks(cfg) {
		nb[task] = now.Add(time.Duration(e.d.Rand() * float64(e.startSpread)))
	}
	e.mu.Lock()
	e.notBefore = nb
	e.mu.Unlock()
}

// due is when the task may next start: its persisted due time, held back by
// the startup delay.
func (e *Engine) due(task string, st TaskState) time.Time {
	at := dueAt(st)
	e.mu.Lock()
	nb, ok := e.notBefore[task]
	e.mu.Unlock()
	if ok && nb.After(at) {
		return nb
	}
	return at
}
