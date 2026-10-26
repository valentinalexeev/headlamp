package tasks

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

// The runner token and the task's secrets go to whichever node is probed and
// started: only nodes with the runner tag and the configured given name may
// ever see them.

func TestEngineNeverProbesUntaggedOrImpostorNodes(t *testing.T) {
	f := newEngFixture(t, oneTask(nil)) // target: node "n1"
	untagged := headscale.Node{Name: "n1", GivenName: "n1", Online: true, IPAddresses: []string{"100.64.0.50"}}
	impostor := runnerNode("n1", "100.64.0.66") // tagged, same reported name
	impostor.GivenName = "laptop"
	real := runnerNode("real-host", "100.64.0.1")
	real.GivenName = "n1"
	f.nodes.nodes = []headscale.Node{untagged, impostor, real}

	f.step()
	rs := f.runs("job")
	if len(rs) != 1 || rs[0].State != RunSucceeded {
		t.Fatalf("runs = %+v", rs)
	}
	f.runner.mu.Lock()
	probed, addrs := f.runner.probed, f.runner.addrs
	f.runner.mu.Unlock()
	if !slices.Equal(probed, []string{"100.64.0.1"}) || !slices.Equal(addrs, []string{"100.64.0.1"}) {
		t.Fatalf("probed %v, started on %v; want only the tagged node with the given name", probed, addrs)
	}
}

func TestEngineUntaggedNodeNeverProbedEvenIfGivenNameMatches(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.nodes.nodes = []headscale.Node{{Name: "n1", GivenName: "n1", Online: true, Tags: []string{"tag:other"},
		IPAddresses: []string{"100.64.0.1"}}}
	f.step()
	if rs := f.runs("job"); len(rs) != 1 || rs[0].State != RunFailed || rs[0].Reason != "no eligible node" {
		t.Fatalf("runs = %+v", rs)
	}
	if f.runner.health != 0 || f.runner.startCount() != 0 {
		t.Fatalf("health %d, starts %d: an untagged node was contacted", f.runner.health, f.runner.startCount())
	}
}

func TestEngineTagTargetOnlyProbesRunnerTaggedNodes(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) { tk.Target = Target{Tags: []string{"tag:region-xx"}} }))
	a := runnerNode("a", "100.64.0.1")
	a.Tags = append(a.Tags, "tag:region-xx")
	b := headscale.Node{Name: "b", GivenName: "b", Online: true, Tags: []string{"tag:region-xx"}, IPAddresses: []string{"100.64.0.2"}}
	f.nodes.nodes = []headscale.Node{b, a}
	f.step()
	f.runner.mu.Lock()
	probed := f.runner.probed
	f.runner.mu.Unlock()
	if !slices.Equal(probed, []string{"100.64.0.1"}) {
		t.Fatalf("probed %v, want only the runner-tagged node", probed)
	}
}

func TestEngineCustomRunnerTag(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.e = NewEngine(Deps{Store: f.store, Nodes: f.nodes, Runner: f.runner, Notifier: f.notifier,
		Now: f.clock.Now, Rand: f.rand, PollEvery: f.e.d.PollEvery, Log: quiet, RunnerTag: "tag:custom"})
	f.e.startSpread = 0
	n := runnerNode("n1", "100.64.0.9")
	n.Tags = []string{"tag:custom"}
	f.nodes.nodes = append(f.nodes.nodes, n) // the default-tagged n1 is ignored now
	f.step()
	f.runner.mu.Lock()
	probed := f.runner.probed
	f.runner.mu.Unlock()
	if !slices.Equal(probed, []string{"100.64.0.9"}) {
		t.Fatalf("probed %v", probed)
	}
}

func TestEngineAmbiguousTargetFailsAndLogsOnce(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	var logs bytes.Buffer
	f.e.log = slog.New(slog.NewTextHandler(&logs, nil))
	other := runnerNode("other", "100.64.0.7")
	other.GivenName = "n1"
	f.nodes.nodes = append(f.nodes.nodes, other)
	f.step()
	f.clock.Set(f.store.State("job").NextRun)
	f.step()
	rs := f.runs("job")
	if len(rs) != 2 || rs[0].Reason != "ambiguous target" || rs[1].Reason != "ambiguous target" {
		t.Fatalf("runs = %+v", rs)
	}
	if f.runner.health != 0 {
		t.Fatalf("an ambiguous target probed %d nodes", f.runner.health)
	}
	if n := strings.Count(logs.String(), "ambiguous task target"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, logs.String())
	}
}

func TestEngineLogsRunnerUnauthorizedOnce(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	var logs bytes.Buffer
	f.e.log = slog.New(slog.NewTextHandler(&logs, nil))
	f.runner.hErr = map[string]error{"100.64.0.1": ErrRunnerUnauthorized}
	f.step()
	f.clock.Set(f.store.State("job").NextRun)
	f.step()
	out := logs.String()
	if n := strings.Count(out, "runner rejected the token"); n != 1 {
		t.Fatalf("logged %d times:\n%s", n, out)
	}
	if !strings.Contains(out, "node=n1") || strings.Contains(out, runnerToken) {
		t.Fatalf("log line must name the node only:\n%s", out)
	}
	if rs := f.runs("job"); rs[0].Reason != "no eligible node" {
		t.Fatalf("runs = %+v", rs)
	}
}

func registerErr(t *testing.T, env map[string]string) error {
	t.Helper()
	m := New(core.MapEnv(env), testNodes())
	return m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet})
}

func TestTasksRequireHostGuard(t *testing.T) {
	for name, v := range map[string]string{"unset": "", "blank": " , ", "wildcard": "node-control.example.com,*"} {
		t.Run(name, func(t *testing.T) {
			env := modEnv("redis://127.0.0.1:1")
			env["ENROLL_ALLOWED_HOSTS"] = v
			err := registerErr(t, env)
			if err == nil || !strings.Contains(err.Error(), "ENROLL_ALLOWED_HOSTS") {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestRunnerTagEnv(t *testing.T) {
	m := New(core.MapEnv(modEnv("redis://127.0.0.1:1")), testNodes()).(*Module)
	env := modEnv("redis://127.0.0.1:1")
	if err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet}); err != nil {
		t.Fatal(err)
	}
	if m.tag != DefaultRunnerTag || DefaultRunnerTag != "tag:task-runner" {
		t.Fatalf("default tag = %q", m.tag)
	}
	env["TASKS_RUNNER_TAG"] = "tag:runners-2"
	m = New(core.MapEnv(env), testNodes()).(*Module)
	if err := m.Register(&core.Routes{Admin: http.NewServeMux(), Env: core.MapEnv(env), Tailnet: gateTailnet(), Log: quiet}); err != nil {
		t.Fatal(err)
	}
	if m.tag != "tag:runners-2" {
		t.Fatalf("tag = %q", m.tag)
	}
	for _, bad := range []string{"task-runner", "tag:", "tag:Upper", "tag:-x", "tag:a b", "tag:" + strings.Repeat("a", 64), "tag:a,tag:b"} {
		env := modEnv("redis://127.0.0.1:1")
		env["TASKS_RUNNER_TAG"] = bad
		if err := registerErr(t, env); err == nil || !strings.Contains(err.Error(), "TASKS_RUNNER_TAG") {
			t.Errorf("%q: err = %v", bad, err)
		}
	}
}

func TestModulePassesRunnerTagToEngine(t *testing.T) {
	h := newModHarnessEnv(t, nil, func(e map[string]string) { e["TASKS_RUNNER_TAG"] = "tag:custom" })
	h.waitOpen()
	_, e := h.m.current()
	if e.d.RunnerTag != "tag:custom" {
		t.Fatalf("engine tag = %q", e.d.RunnerTag)
	}
}
