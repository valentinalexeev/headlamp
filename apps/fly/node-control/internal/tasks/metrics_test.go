package tasks

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func metricsText(m *Metrics) string {
	var b bytes.Buffer
	m.WriteText(&b)
	return b.String()
}

func TestMetricsTextGolden(t *testing.T) {
	m := NewMetrics()
	weird := "we\"ird\\task\nx"
	// Recorded out of order: the output must not depend on it.
	m.Observe(weird, "n1", RunLost, 30250*time.Millisecond)
	m.Observe("job", "n2", RunSucceeded, 500*time.Millisecond)
	m.Observe("job", "n1", RunSucceeded, 2*time.Second)
	m.Observe("job", "n1", RunFailed, 1500*time.Millisecond)
	m.NotifySent("scheduler", "ping", true)
	m.NotifySent("job", "ping", false)
	m.NotifySent("job", "alert", true)
	m.NotifySent("job", "ping", false)
	m.Snapshot("retrying", TaskState{Streak: 1, LastSuccess: t0.Add(-time.Hour), NextRun: t0.Add(time.Hour),
		Attempt: 2, AttemptAt: t0.Add(5 * time.Minute)})
	m.Snapshot("job", TaskState{LastSuccess: t0, NextRun: t0.Add(time.Hour)})
	m.Snapshot("never", TaskState{Streak: 3, NextRun: t0.Add(60500 * time.Millisecond)}) // never succeeded
	m.Snapshot("idle", TaskState{})                                                      // no next run either

	want, err := os.ReadFile("testdata/metrics.golden")
	if err != nil {
		t.Fatal(err)
	}
	got := metricsText(m)
	if got != string(want) {
		t.Fatalf("metrics text differs from golden.\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if again := metricsText(m); again != got {
		t.Fatal("output is not deterministic")
	}
}

func TestMetricsSnapshotAndForget(t *testing.T) {
	m := NewMetrics()
	m.Observe("a", "n1", RunSucceeded, time.Second)
	m.NotifySent("a", "alert", false)
	m.Snapshot("a", TaskState{LastSuccess: t0, Streak: 2, NextRun: t0.Add(time.Hour)})
	m.Snapshot("b", TaskState{Streak: 1})
	out := metricsText(m)
	for _, s := range []string{`task_failure_streak{task="a"} 2`, `task_failure_streak{task="b"} 1`,
		`task_last_success_timestamp_seconds{task="a"}`, `task_next_run_timestamp_seconds{task="a"}`} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
	// A later snapshot replaces the earlier one.
	m.Snapshot("a", TaskState{Streak: 0})
	out = metricsText(m)
	if !strings.Contains(out, `task_failure_streak{task="a"} 0`) ||
		strings.Contains(out, `task_last_success_timestamp_seconds{task="a"}`) ||
		strings.Contains(out, `task_next_run_timestamp_seconds{task="a"}`) {
		t.Fatalf("snapshot not replaced:\n%s", out)
	}

	m.Forget("a")
	out = metricsText(m)
	if strings.Contains(out, `task_failure_streak{task="a"}`) {
		t.Fatalf("gauge of a forgotten task still present:\n%s", out)
	}
	if !strings.Contains(out, `task_failure_streak{task="b"} 1`) {
		t.Fatalf("other task lost:\n%s", out)
	}
	// Counters are cumulative and survive Forget.
	if !strings.Contains(out, `task_runs_total{task="a",node="n1",state="succeeded"} 1`) ||
		!strings.Contains(out, `task_notify_failures_total{task="a"} 1`) {
		t.Fatalf("counters dropped:\n%s", out)
	}
}

func TestMetricsNilIsNoop(t *testing.T) {
	var m *Metrics
	m.Observe("a", "n", RunFailed, time.Second)
	m.NotifySent("a", "ping", true)
	m.Snapshot("a", TaskState{})
	m.Forget("a")
	m.WriteText(io.Discard)
}

// --- server ------------------------------------------------------------------

func startMetricsServer(t *testing.T, m *Metrics) (string, context.CancelFunc, <-chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	stopped := make(chan struct{})
	go func() { done <- serveMetrics(ctx, ln, m); close(stopped) }()
	t.Cleanup(func() { cancel(); <-stopped })
	return "http://" + ln.Addr().String(), cancel, done
}

func TestMetricsServerOnlyServesMetricsPath(t *testing.T) {
	m := NewMetrics()
	m.Observe("job", "n1", RunSucceeded, time.Second)
	base, _, _ := startMetricsServer(t, m)

	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; version=0.0.4; charset=utf-8" {
		t.Fatalf("GET /metrics: %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if !strings.Contains(string(body), `task_runs_total{task="job",node="n1",state="succeeded"} 1`) {
		t.Fatalf("body:\n%s", body)
	}

	for _, p := range []string{"/", "/metrics/", "/tasks", "/metrics.json", "/debug/pprof/"} {
		resp, err := http.Get(base + p)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 404 || len(b) != 0 {
			t.Errorf("GET %s: %d %q", p, resp.StatusCode, b)
		}
	}
	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req, _ := http.NewRequest(method, base+"/metrics", strings.NewReader("x"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 405 {
			t.Errorf("%s /metrics: %d", method, resp.StatusCode)
		}
	}
}

func TestMetricsServerStopsOnContextCancel(t *testing.T) {
	_, cancel, done := startMetricsServer(t, NewMetrics())
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned %v after cancel", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("server did not stop")
	}

	// ServeMetrics itself: returns on cancel, and at once on a listen error.
	ctx, cancel2 := context.WithCancel(context.Background())
	ret := make(chan error, 1)
	go func() { ret <- ServeMetrics(ctx, "127.0.0.1:0", NewMetrics()) }()
	time.Sleep(20 * time.Millisecond)
	cancel2()
	select {
	case err := <-ret:
		if err != nil {
			t.Fatalf("ServeMetrics: %v", err)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("ServeMetrics did not stop")
	}

	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	if err := ServeMetrics(context.Background(), busy.Addr().String(), NewMetrics()); err == nil {
		t.Fatal("listen on a busy port must fail")
	}
}

// --- engine and notifier wiring ------------------------------------------------

func TestEngineReportsMetricsForAttempts(t *testing.T) {
	f := newEngFixture(t, oneTask(func(tk *Task) {
		tk.Retry = Retry{Attempts: 2, Backoff: []Duration{Duration(time.Minute)}}
	}))
	m := NewMetrics()
	f.e.d.Metrics = m
	f.runner.status = func(_ context.Context, _ string, call int) (RunStatus, error) {
		f.clock.Advance(1500 * time.Millisecond)
		if call == 1 {
			return RunStatus{State: "failed", Error: "boom"}, nil
		}
		return RunStatus{State: "succeeded", Result: map[string]any{"ok": 1.0}}, nil
	}

	f.step()
	out := metricsText(m)
	if !strings.Contains(out, `task_runs_total{task="job",node="n1",state="failed"} 1`) {
		t.Fatalf("failed attempt not counted:\n%s", out)
	}
	retryAt := float64(f.store.State("job").AttemptAt.UnixMilli()) / 1000
	if !strings.Contains(out, `task_next_run_timestamp_seconds{task="job"} `+formatFloat(retryAt)) {
		t.Fatalf("next run should be the retry time:\n%s", out)
	}

	f.clock.Set(f.store.State("job").AttemptAt)
	f.step()
	out = metricsText(m)
	for _, s := range []string{
		`task_runs_total{task="job",node="n1",state="failed"} 1`,
		`task_runs_total{task="job",node="n1",state="succeeded"} 1`,
		`task_run_duration_seconds_count{task="job"} 2`,
		`task_run_duration_seconds_sum{task="job"} 3`,
		`task_failure_streak{task="job"} 0`,
		`task_last_success_timestamp_seconds{task="job"} ` + formatFloat(float64(f.store.State("job").LastSuccess.UnixMilli())/1000),
		`task_next_run_timestamp_seconds{task="job"} ` + formatFloat(float64(f.store.State("job").NextRun.UnixMilli())/1000),
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
}

func TestEngineMetricsStartupSnapshotAndReloadForget(t *testing.T) {
	tasks := map[string]Task{"job": baseTask(), "other": baseTask()}
	f := newEngFixture(t, tasks)
	future := t0.Add(24 * time.Hour)
	for _, id := range []string{"job", "other"} {
		if err := f.store.SaveState(id, TaskState{NextRun: future, Streak: 4}); err != nil {
			t.Fatal(err)
		}
	}
	m := NewMetrics()
	f.e.d.Metrics = m
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); f.e.Run(ctx) }()
	defer func() { cancel(); <-done }()
	waitFor(t, "startup snapshot", func() bool {
		out := metricsText(m)
		return strings.Contains(out, `task_failure_streak{task="job"} 4`) &&
			strings.Contains(out, `task_failure_streak{task="other"} 4`)
	})

	if err := f.store.PutConfig(&Config{Tasks: map[string]Task{"job": baseTask()}}, map[string]string{"job.sh": jobScript}); err != nil {
		t.Fatal(err)
	}
	f.e.Reload()
	waitFor(t, "removed task forgotten", func() bool {
		out := metricsText(m)
		return !strings.Contains(out, `task="other"`) && strings.Contains(out, `task_failure_streak{task="job"} 4`)
	})
}

func TestNotifierObserverFeedsMetrics(t *testing.T) {
	f := newFixture(t)
	m := NewMetrics()
	f.n.SetObserver(m.NotifySent)

	f.success(500) // ping ok, alert ok
	f.ntfyCode = []int{500}
	f.n.Failure(context.Background(), "tk", alertTask(), "x") // ping ok, failure fails
	if out := f.n.PingSecret(context.Background(), "hc"); out != "sent" {
		t.Fatalf("PingSecret = %q", out)
	}
	if out := f.n.PingSecret(context.Background(), "absent"); out != failedNoSecret {
		t.Fatalf("PingSecret missing = %q", out)
	}
	out := metricsText(m)
	for _, s := range []string{
		`task_alert_sent_total{task="tk",kind="alert",ok="true"} 1`,
		`task_alert_sent_total{task="tk",kind="failure",ok="false"} 1`,
		`task_alert_sent_total{task="tk",kind="ping",ok="true"} 2`,
		`task_alert_sent_total{task="scheduler",kind="ping",ok="true"} 1`,
		`task_alert_sent_total{task="scheduler",kind="ping",ok="false"} 1`,
		`task_notify_failures_total{task="tk"} 1`,
		`task_notify_failures_total{task="scheduler"} 1`,
	} {
		if !strings.Contains(out, s) {
			t.Errorf("missing %q in\n%s", s, out)
		}
	}
	if strings.Contains(out, f.pingSrv.URL) || strings.Contains(out, testTopic) {
		t.Fatalf("metrics leak a URL or topic:\n%s", out)
	}
}
