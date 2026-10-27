package tasks

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// sweepGhosts stores one running ghost per address and runs the sweep.
func sweepGhosts(t *testing.T, f *engFixture, addrs ...string) {
	t.Helper()
	for i, a := range addrs {
		g := Run{ID: "g" + string(rune('a'+i)), Task: "job", Node: "n1", Attempt: 1, State: RunRunning,
			Started: t0, RunnerID: "orphan-" + a, RunnerAddr: a}
		if err := f.store.AppendRun(g); err != nil {
			t.Fatal(err)
		}
	}
	f.e.sweep(context.Background())
	for _, r := range f.runs("job") {
		if r.State != RunLost || r.Reason != reasonRestarted {
			t.Fatalf("ghost not finalised: %+v", r)
		}
	}
}

func TestSweepCancelsOnlyAtTaggedNodeAddresses(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	untagged := runnerNode("n3", "100.64.0.3")
	untagged.Tags = []string{"tag:other"}
	f.nodes.nodes = append(f.nodes.nodes, untagged)
	var buf bytes.Buffer
	f.e.log = slog.New(slog.NewTextHandler(&buf, nil))

	// .1 is a tagged node, .3 now belongs to an untagged node, .9 to no node.
	sweepGhosts(t, f, "100.64.0.1", "100.64.0.3", "100.64.0.9")

	f.runner.mu.Lock()
	addrs := append([]string(nil), f.runner.cAddrs...)
	f.runner.mu.Unlock()
	if len(addrs) != 1 || addrs[0] != "100.64.0.1" {
		t.Fatalf("cancels sent to %v, want only the tagged node", addrs)
	}
	if n := f.nodes.calls.Load(); n != 1 {
		t.Fatalf("node list fetched %d times, want once", n)
	}
	out := buf.String()
	if strings.Count(out, "not cancelled") != 1 {
		t.Fatalf("want one skip line, got:\n%s", out)
	}
	if strings.Contains(out, "100.64.0.3") || strings.Contains(out, "100.64.0.9") {
		t.Fatalf("skip line names an address:\n%s", out)
	}
}

func TestSweepNodeListErrorSkipsCancels(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	f.nodes.err = errors.New("headscale down")
	sweepGhosts(t, f, "100.64.0.1", "100.64.0.2")
	if n := f.runner.cancelCount(); n != 0 {
		t.Fatalf("cancels = %d, want none when the node list is unavailable", n)
	}
}

func TestSweepNoNodeListWithoutCancellableGhosts(t *testing.T) {
	f := newEngFixture(t, oneTask(nil))
	if err := f.store.AppendRun(Run{ID: "q", Task: "job", Attempt: 1, State: RunQueued, Started: t0}); err != nil {
		t.Fatal(err)
	}
	f.e.sweep(context.Background())
	if n := f.nodes.calls.Load(); n != 0 {
		t.Fatalf("node list fetched %d times with nothing to cancel", n)
	}
	if r := f.runs("job")[0]; r.State != RunLost {
		t.Fatalf("queued ghost = %+v", r)
	}
}
