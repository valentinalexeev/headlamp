package tasks

import (
	"time"
)

// The API never serialises Run or TaskState directly: these views name every
// field explicitly and leave out where a run lives on its node.

type runView struct {
	ID            string         `json:"id"`
	Task          string         `json:"task"`
	Node          string         `json:"node"`
	Attempt       int            `json:"attempt"`
	State         RunState       `json:"state"`
	Reason        string         `json:"reason"`
	Started       *string        `json:"started"`
	Ended         *string        `json:"ended"`
	DurationMS    *int64         `json:"duration_ms"` // null until the run ended
	Result        map[string]any `json:"result"`
	ScriptSHA     string         `json:"script_sha"`
	NotifyOutcome string         `json:"notify_outcome"`
}

type taskView struct {
	ID          string         `json:"id"`
	Script      string         `json:"script"`
	Target      Target         `json:"target"`
	Every       string         `json:"every"`
	Running     bool           `json:"running"`
	RunningRun  *runView       `json:"running_run"`
	LastSuccess *string        `json:"last_success"`
	LastResult  map[string]any `json:"last_result"`
	Streak      int            `json:"streak"`
	Attempt     int            `json:"attempt"`
	NextRun     *string        `json:"next_run"`
	Stale       bool           `json:"stale"`
}

// timeJSON is RFC 3339 in UTC, or null for the zero time.
func timeJSON(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func toRunView(r Run) runView {
	v := runView{
		ID: r.ID, Task: r.Task, Node: r.Node, Attempt: r.Attempt, State: r.State, Reason: r.Reason,
		Started: timeJSON(r.Started), Ended: timeJSON(r.Ended), Result: r.Result,
		ScriptSHA: r.ScriptSHA, NotifyOutcome: r.NotifyOutcome,
	}
	if !r.Started.IsZero() && !r.Ended.IsZero() {
		d := r.Ended.Sub(r.Started).Milliseconds()
		v.DurationMS = &d
	}
	return v
}

// staleGrace keeps a task that is about to start from flashing stale.
const staleGrace = time.Minute

// isStale reports that a task is overdue for a success:
//   - It has succeeded before: its last success is older than 2 x every.
//   - It never succeeded: its first scheduled time has passed. That is the case
//     when it already has a finished run (it ran and did not succeed), or when
//     its NextRun is set and more than a minute in the past with no run in
//     flight (it should have started and has not). A new task that never ran
//     (zero NextRun, no history) is not stale.
func isStale(now time.Time, every time.Duration, st TaskState, running, ranBefore bool) bool {
	if !st.LastSuccess.IsZero() {
		return now.Sub(st.LastSuccess) > 2*every
	}
	if ranBefore {
		return true
	}
	return !st.NextRun.IsZero() && !running && now.Sub(st.NextRun) > staleGrace
}

// buildTaskView assembles one task from the config, its state, its history
// (newest first) and the attempt executing now, if any.
func buildTaskView(now time.Time, id string, t Task, st TaskState, runs []Run, active *Run) taskView {
	v := taskView{
		ID: id, Script: t.Script, Target: t.Target, Every: time.Duration(t.Schedule.Every).String(),
		Running: active != nil, LastSuccess: timeJSON(st.LastSuccess), Streak: st.Streak, Attempt: st.Attempt,
	}
	if active != nil {
		rv := toRunView(*active)
		v.RunningRun = &rv
	}
	if st.Attempt > 0 {
		v.NextRun = timeJSON(st.AttemptAt)
	} else {
		v.NextRun = timeJSON(st.NextRun)
	}
	ranBefore := false
	for _, r := range runs {
		if r.State == RunSucceeded && v.LastResult == nil {
			v.LastResult = r.Result
			if v.LastResult == nil {
				v.LastResult = map[string]any{}
			}
		}
		if r.State != RunRunning && r.State != RunQueued {
			ranBefore = true
		}
	}
	v.Stale = isStale(now, time.Duration(t.Schedule.Every), st, active != nil, ranBefore)
	return v
}
