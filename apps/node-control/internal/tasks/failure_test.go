package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

// Each row of the class table in the README (#323).
func TestFailureClassTable(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(f *engFixture)
		task   func(tk *Task)
		reason string
		state  RunState
		class  FailureClass
	}{
		{name: "node busy", reason: "node busy", state: RunFailed, class: ClassTransient,
			setup: func(f *engFixture) {
				f.runner.start = func(string, StartReq) (string, error) { return "", ErrRunnerBusy }
			}},
		{name: "timeout", reason: "timeout", state: RunFailed, class: ClassTransient,
			setup: func(f *engFixture) {
				f.runner.status = func(context.Context, string, int) (RunStatus, error) {
					f.clock.Advance(time.Hour)
					return RunStatus{State: "running"}, nil
				}
			}},
		{name: "runner unreachable", reason: "runner unreachable", state: RunLost, class: ClassTransient,
			setup: func(f *engFixture) {
				f.runner.status = func(context.Context, string, int) (RunStatus, error) {
					f.clock.Advance(time.Hour)
					return RunStatus{}, errors.New("dial")
				}
			}},
		{name: "lost", reason: "runner lost the run", state: RunLost, class: ClassTransient,
			setup: func(f *engFixture) {
				f.runner.status = func(context.Context, string, int) (RunStatus, error) { return RunStatus{}, ErrRunUnknown }
			}},
		{name: "no eligible node", reason: "no eligible node", state: RunFailed, class: ClassStructural,
			setup: func(f *engFixture) { f.nodes.nodes = nil }},
		{name: "ambiguous target", reason: "ambiguous target", state: RunFailed, class: ClassStructural,
			setup: func(f *engFixture) {
				dup := runnerNode("other", "100.64.0.9")
				dup.GivenName = "n1"
				f.nodes.nodes = append(f.nodes.nodes, dup)
			}},
		{name: "script not found", reason: "script not found: gone.sh", state: RunFailed, class: ClassStructural,
			task: func(tk *Task) { tk.Script = "gone.sh" }},
		{name: "secret not configured", reason: "secret not configured: NOPE", state: RunFailed, class: ClassStructural,
			task: func(tk *Task) { tk.Secrets = []string{"NOPE"} }},
		{name: "script error", reason: "boom", state: RunFailed, class: ClassUnknown,
			setup: func(f *engFixture) { f.runner.status = failStatus("boom") }},
		{name: "bad result", reason: "bad result: ", state: RunFailed, class: ClassUnknown,
			task: func(tk *Task) {
				tk.Alert = &Alert{When: Condition{Field: "missing", Op: "<", Value: 1}, Notify: Notify{Topic: "T"}}
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEngFixture(t, oneTask(c.task))
			if c.setup != nil {
				c.setup(f)
			}
			f.step()
			r := f.runs("job")[0]
			if r.State != c.state || !strings.HasPrefix(r.Reason, c.reason) || r.Class != c.class {
				t.Fatalf("run = state %s reason %q class %q, want %s %q %q", r.State, r.Reason, r.Class, c.state, c.reason, c.class)
			}
			if v := toRunView(r); v.Class != c.class {
				t.Fatalf("view class = %q", v.Class)
			}
		})
	}
}

func TestSuccessAndCancelHaveNoClass(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.step()
	if r := f.runs("job")[0]; r.State != RunSucceeded || r.Class != "" || r.Counts != nil {
		t.Fatalf("run = %+v", r)
	}
}

// Each filter count of "no eligible node".
func TestNoEligibleNodeCounts(t *testing.T) {
	tag := []string{DefaultRunnerTag}
	tagX := []string{DefaultRunnerTag, "tag:x"}
	cases := []struct {
		name    string
		target  Target
		nodes   []headscale.Node
		healthy map[string]bool
		want    NodeCounts
		detail  string
	}{
		{"no tagged node", Target{Tags: []string{"tag:x"}}, nil, nil,
			NodeCounts{}, "tagged 0"},
		{"none matching", Target{Tags: []string{"tag:x"}},
			[]headscale.Node{node("a", true, tag, "100.64.0.1"), node("b", true, tag, "100.64.0.2")}, nil,
			NodeCounts{Tagged: 2}, "tagged 2, matching 0"},
		{"none online", Target{Tags: []string{"tag:x"}, Online: true},
			[]headscale.Node{node("a", false, tagX, "100.64.0.1"), node("b", true, tag, "100.64.0.2")}, nil,
			NodeCounts{Tagged: 2, Matching: 1}, "tagged 2, matching 1, online 0"},
		{"no address", Target{Tags: []string{"tag:x"}},
			[]headscale.Node{node("a", false, tagX, "fd7a::1")}, nil,
			NodeCounts{Tagged: 1, Matching: 1, Online: 1}, "tagged 1, matching 1, online 1, with address 0"},
		{"none healthy", Target{Node: "a"},
			[]headscale.Node{node("a", true, tag, "100.64.0.1")}, map[string]bool{"100.64.0.1": false},
			NodeCounts{Tagged: 1, Matching: 1, Online: 1, Address: 1}, "tagged 1, matching 1, online 1, with address 1, healthy 0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newEngFixture(t, oneTask(func(tk *Task) { tk.Target = c.target }))
			f.nodes.nodes = c.nodes
			f.runner.healthy = c.healthy
			f.step()
			r := f.runs("job")[0]
			if r.Class != ClassStructural || r.Counts == nil || *r.Counts != c.want {
				t.Fatalf("run = %+v counts %+v, want %+v", r, r.Counts, c.want)
			}
			if got := toRunView(r).Detail; got != c.detail {
				t.Fatalf("detail = %q, want %q", got, c.detail)
			}
			if got := runReasonText(r); got != "no eligible node ("+c.detail+")" {
				t.Fatalf("reason text = %q", got)
			}
			if in := f.notifier.infos; len(in) != 1 || in[0].Detail != c.detail || in[0].Class != ClassStructural {
				t.Fatalf("notifier got %+v", in)
			}
		})
	}
}

func retry3(tk *Task) {
	tk.Retry = Retry{Attempts: 3, Backoff: []Duration{Duration(2 * time.Minute), Duration(10 * time.Minute)}}
}

// Structural: one message within the first attempt, and no retry.
func TestStructuralFailureNotifiesAtOnceAndSkipsRetries(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) { retry3(tk); tk.Target = Target{Tags: []string{"tag:x"}} }))
	f.nodes.nodes = nil
	f.step()
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failures = %d, want 1 after the first attempt", fl)
	}
	if info := f.notifier.infos[0]; info.Class != ClassStructural || info.Detail != "tagged 0" || info.Streak != 1 {
		t.Fatalf("info = %+v", info)
	}
	st := f.store.State("job")
	if st.Attempt != 0 || !st.AttemptAt.IsZero() || st.Streak != 1 || !st.NextRun.Equal(t0.Add(time.Hour)) {
		t.Fatalf("state = %+v (no retry wanted)", st)
	}
	if len(f.runs("job")) != 1 {
		t.Fatal("more than one attempt")
	}
}

// Transient: retries go on, and the message waits for the last attempt.
func TestNodeBusyRetriesAndNotifiesOnlyAfterLastAttempt(t *testing.T) {
	f := newEngFixture(t, oneTask(retry3))
	f.runner.start = func(string, StartReq) (string, error) { return "", ErrRunnerBusy }
	f.step()
	f.clock.Advance(2 * time.Minute)
	f.step()
	if _, fl := f.notifier.counts(); fl != 0 {
		t.Fatalf("failures = %d before the last attempt", fl)
	}
	if st := f.store.State("job"); st.Attempt != 2 {
		t.Fatalf("state = %+v", st)
	}
	f.clock.Advance(10 * time.Minute)
	f.step()
	if _, fl := f.notifier.counts(); fl != 1 {
		t.Fatalf("failures = %d after the last attempt", fl)
	}
	if info := f.notifier.infos[0]; info.Class != ClassTransient || info.Reason != "node busy" {
		t.Fatalf("info = %+v", info)
	}
}

func TestFailureInfoCarriesStreak(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.runner.status = failStatus("boom")
	f.step()
	f.clock.Advance(time.Hour)
	f.step()
	if in := f.notifier.infos; len(in) != 2 || in[0].Streak != 1 || in[1].Streak != 2 || in[1].Class != ClassUnknown {
		t.Fatalf("infos = %+v", in)
	}
}

func lastNtfy(t *testing.T, f *fixture) map[string]any {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(f.ntfy[len(f.ntfy)-1].body, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestFailureMessageHasClassAndDetail(t *testing.T) {
	f := newFixture(t)
	tk := alertTask()
	tk.OnFailure.Notify.Message = "{task} {class}: {reason} [{detail}]"
	r := f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "no eligible node", Class: ClassStructural, Detail: "tagged 0", Streak: 1})
	if r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	if m := lastNtfy(t, f); m["message"] != "tk structural: no eligible node [tagged 0]" {
		t.Fatalf("message = %v", m["message"])
	}
	// No class given: unknown. No detail: empty.
	f.now = f.now.Add(7 * time.Hour)
	f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "boom"})
	if m := lastNtfy(t, f); m["message"] != "tk unknown: boom []" {
		t.Fatalf("message = %v", m["message"])
	}
}

// on_failure.streak 2 holds the message until the second failed sequence.
func TestFailureStreakHoldsMessage(t *testing.T) {
	f := newFixture(t)
	tk := alertTask()
	tk.OnFailure.Streak = 2
	if r := f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "x", Streak: 1}); r.Alert != "held" || r.Ping != "sent" {
		t.Fatalf("first: %+v", r)
	}
	if f.ntfyCount() != 0 {
		t.Fatal("sent on the first failed sequence")
	}
	if r := f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "x", Streak: 2}); r.Alert != "sent" {
		t.Fatalf("second: %+v", r)
	}
	if f.ntfyCount() != 1 {
		t.Fatalf("ntfy calls = %d", f.ntfyCount())
	}
}

// on_recovery sends once, on the first success after a failure message.
func TestRecoverySentOnceAfterFailureMessage(t *testing.T) {
	f := newFixture(t)
	tk := alertTask()
	tk.OnRecovery = &OnRecovery{Notify: Notify{Topic: "fail-topic", Title: "{task} recovered", Message: "ok"}}
	if out := f.n.Recovery(context.Background(), "tk", tk); out != "" || f.ntfyCount() != 0 {
		t.Fatalf("recovery without a failure message: %q", out)
	}
	f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "x", Streak: 1})
	if out := f.n.Recovery(context.Background(), "tk", tk); out != "sent" {
		t.Fatalf("recovery = %q", out)
	}
	if m := lastNtfy(t, f); m["title"] != "tk recovered" {
		t.Fatalf("title = %v", m["title"])
	}
	if out := f.n.Recovery(context.Background(), "tk", tk); out != "" || f.ntfyCount() != 2 {
		t.Fatalf("second recovery = %q, calls %d", out, f.ntfyCount())
	}
}

func TestRecoveryNotSentWhenFailureWasHeld(t *testing.T) {
	f := newFixture(t)
	tk := alertTask()
	tk.OnFailure.Streak = 2
	tk.OnRecovery = &OnRecovery{Notify: Notify{Topic: "fail-topic", Message: "ok"}}
	f.n.Failure(context.Background(), "tk", tk, FailureInfo{Reason: "x", Streak: 1})
	if out := f.n.Recovery(context.Background(), "tk", tk); out != "" || f.ntfyCount() != 0 {
		t.Fatalf("recovery = %q calls %d", out, f.ntfyCount())
	}
}

// End to end through the engine: streak 2 and on_recovery with the real notifier.
func TestEngineStreakAndRecoveryWithNotifier(t *testing.T) {
	nf := newFixture(t)
	tk := alertTask()
	tk.Alert = nil
	tk.Healthcheck = ""
	tk.Script, tk.Target, tk.Schedule, tk.Timeout, tk.Retry = "job.sh", Target{Node: "n1"}, Schedule{Every: Duration(time.Hour)}, Duration(time.Minute), Retry{Attempts: 1}
	tk.OnFailure.Streak = 2
	tk.OnRecovery = &OnRecovery{Notify: Notify{Topic: "fail-topic", Message: "{task} ok"}}
	if err := nf.store.PutConfig(&Config{Tasks: map[string]Task{"job": tk}}, map[string]string{"job.sh": jobScript}); err != nil {
		t.Fatal(err)
	}
	clock := &fakeClock{t: t0}
	nf.n.now = clock.Now
	runner := &fakeRunner{status: failStatus("boom")}
	e := NewEngine(Deps{Store: nf.store, Nodes: &fakeNodes{nodes: []headscale.Node{runnerNode("n1", "100.64.0.1")}},
		Runner: runner, Notifier: nf.n, Now: clock.Now, Rand: func() float64 { return 0.5 }, PollEvery: 2 * time.Millisecond, Log: quiet})
	step := func() { e.tick(context.Background()); e.wait() }
	step()
	if nf.ntfyCount() != 0 {
		t.Fatal("message on the first failed sequence")
	}
	clock.Advance(time.Hour)
	step()
	if nf.ntfyCount() != 1 {
		t.Fatalf("ntfy = %d after the second failed sequence", nf.ntfyCount())
	}
	runner.mu.Lock()
	runner.status = nil
	runner.mu.Unlock()
	clock.Advance(time.Hour)
	step()
	if nf.ntfyCount() != 2 || lastNtfy(t, nf)["message"] != "job ok" {
		t.Fatalf("ntfy = %d, want the recovery message", nf.ntfyCount())
	}
	if r := nf.store.Runs("job")[0]; !strings.Contains(r.NotifyOutcome, "recovery=sent") {
		t.Fatalf("notify outcome = %q", r.NotifyOutcome)
	}
	clock.Advance(time.Hour)
	step()
	if nf.ntfyCount() != 2 {
		t.Fatal("recovery sent twice")
	}
}
