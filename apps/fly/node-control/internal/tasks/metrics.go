package tasks

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Metrics holds the task counters and gauges in memory and writes them in the
// Prometheus text format. It never reads Redis. Labels are task ids, node
// names, run states and notification kinds only: never a result value, an
// account name, a secret or a URL. A nil *Metrics is valid and records nothing.
type Metrics struct {
	mu sync.Mutex

	runs       map[[3]string]uint64 // task, node, state
	durSum     map[string]float64   // task -> seconds
	durCount   map[string]uint64
	sent       map[[3]string]uint64 // task, kind, ok
	notifyFail map[string]uint64

	// Gauges, replaced by Snapshot and dropped by Forget.
	streak      map[string]int
	lastSuccess map[string]time.Time
	nextRun     map[string]time.Time
}

func NewMetrics() *Metrics {
	return &Metrics{
		runs: map[[3]string]uint64{}, durSum: map[string]float64{}, durCount: map[string]uint64{},
		sent: map[[3]string]uint64{}, notifyFail: map[string]uint64{},
		streak: map[string]int{}, lastSuccess: map[string]time.Time{}, nextRun: map[string]time.Time{},
	}
}

// Observe counts one finished attempt.
func (m *Metrics) Observe(task, node string, state RunState, d time.Duration) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[[3]string{task, node, string(state)}]++
	m.durSum[task] += max(d, 0).Seconds()
	m.durCount[task]++
}

// NotifySent counts one attempted notification or ping; kind is alert,
// failure or ping. It has the signature HTTPNotifier.SetObserver takes.
func (m *Metrics) NotifySent(task, kind string, ok bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent[[3]string{task, kind, strconv.FormatBool(ok)}]++
	if !ok {
		m.notifyFail[task]++
	}
}

// Snapshot replaces the task's gauges from its state. The next run is the
// retry time while a retry sequence is in progress.
func (m *Metrics) Snapshot(task string, st TaskState) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.streak[task] = st.Streak
	setOrDelete(m.lastSuccess, task, st.LastSuccess)
	setOrDelete(m.nextRun, task, dueAt(st))
}

func setOrDelete(g map[string]time.Time, task string, t time.Time) {
	if t.IsZero() {
		delete(g, task)
	} else {
		g[task] = t
	}
}

// Forget drops the gauges of a task that left the config. Counters are
// cumulative and stay.
func (m *Metrics) Forget(task string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.streak, task)
	delete(m.lastSuccess, task)
	delete(m.nextRun, task)
}

// gaugeTasks lists the tasks that have gauges, so Reload can find removed ones.
func (m *Metrics) gaugeTasks() []string {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.streak))
	for t := range m.streak {
		out = append(out, t)
	}
	return out
}

// sample is one exposition line: a name suffix, label values and the value.
type sample struct {
	suffix string
	labels []string
	value  string
}

type family struct {
	name, help, typ string
	labels          []string // label names, in output order
	samples         []sample
}

// WriteText writes the text exposition format 0.0.4. Families are sorted by
// name and samples by label values, so the output is deterministic.
func (m *Metrics) WriteText(w io.Writer) {
	if m == nil {
		return
	}
	bw := bufio.NewWriter(w)
	for _, f := range m.families() {
		bw.WriteString("# HELP " + f.name + " " + f.help + "\n")
		bw.WriteString("# TYPE " + f.name + " " + f.typ + "\n")
		for _, s := range f.samples {
			bw.WriteString(f.name + s.suffix + "{")
			for i, l := range f.labels {
				if i > 0 {
					bw.WriteByte(',')
				}
				bw.WriteString(l + `="` + escapeLabel(s.labels[i]) + `"`)
			}
			bw.WriteString("} " + s.value + "\n")
		}
	}
	bw.Flush()
}

// families copies the values under the lock and sorts them.
func (m *Metrics) families() []family {
	u := func(n uint64) string { return strconv.FormatUint(n, 10) }
	ts := func(t time.Time) string { return formatFloat(float64(t.UnixMilli()) / 1000) }
	one := func(t string) []string { return []string{t} }

	runs := family{name: "task_runs_total", typ: "counter", labels: []string{"task", "node", "state"},
		help: "Finished attempts, by task, node and state."}
	dur := family{name: "task_run_duration_seconds", typ: "summary", labels: []string{"task"},
		help: "Duration of finished attempts."}
	sent := family{name: "task_alert_sent_total", typ: "counter", labels: []string{"task", "kind", "ok"},
		help: "Notifications and pings attempted, by kind and outcome."}
	fails := family{name: "task_notify_failures_total", typ: "counter", labels: []string{"task"},
		help: "Notifications and pings that failed."}
	streak := family{name: "task_failure_streak", typ: "gauge", labels: []string{"task"},
		help: "Sequences in a row that ended in failure."}
	last := family{name: "task_last_success_timestamp_seconds", typ: "gauge", labels: []string{"task"},
		help: "Unix time of the last successful attempt."}
	next := family{name: "task_next_run_timestamp_seconds", typ: "gauge", labels: []string{"task"},
		help: "Unix time the next attempt is due."}

	m.mu.Lock()
	for k, v := range m.runs {
		runs.samples = append(runs.samples, sample{labels: []string{k[0], k[1], k[2]}, value: u(v)})
	}
	for t, c := range m.durCount {
		dur.samples = append(dur.samples,
			sample{suffix: "_sum", labels: one(t), value: formatFloat(m.durSum[t])},
			sample{suffix: "_count", labels: one(t), value: u(c)})
	}
	for k, v := range m.sent {
		sent.samples = append(sent.samples, sample{labels: []string{k[0], k[1], k[2]}, value: u(v)})
	}
	for t, v := range m.notifyFail {
		fails.samples = append(fails.samples, sample{labels: one(t), value: u(v)})
	}
	for t, v := range m.streak {
		streak.samples = append(streak.samples, sample{labels: one(t), value: strconv.Itoa(v)})
	}
	for t, v := range m.lastSuccess {
		last.samples = append(last.samples, sample{labels: one(t), value: ts(v)})
	}
	for t, v := range m.nextRun {
		next.samples = append(next.samples, sample{labels: one(t), value: ts(v)})
	}
	m.mu.Unlock()

	fams := []family{runs, dur, sent, fails, streak, last, next}
	slices.SortFunc(fams, func(a, b family) int { return strings.Compare(a.name, b.name) })
	for i := range fams {
		slices.SortFunc(fams[i].samples, func(a, b sample) int {
			if c := slices.Compare(a.labels, b.labels); c != 0 {
				return c
			}
			return strings.Compare(b.suffix, a.suffix) // "_sum" before "_count"
		})
	}
	return fams
}

var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

func escapeLabel(v string) string { return labelEscaper.Replace(v) }

func formatFloat(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

const (
	metricsReadHeaderTimeout = 5 * time.Second
	metricsShutdownTimeout   = 5 * time.Second
	metricsContentType       = "text/plain; version=0.0.4; charset=utf-8"
)

// ServeMetrics answers GET /metrics on addr until ctx is done (then shuts
// down gracefully, bounded) or the listener fails. Nothing else is served and
// nothing is logged per request.
func ServeMetrics(ctx context.Context, addr string, m *Metrics) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return serveMetrics(ctx, ln, m)
}

func serveMetrics(ctx context.Context, ln net.Listener, m *Metrics) error {
	srv := &http.Server{
		Handler:           metricsHandler(m),
		ReadHeaderTimeout: metricsReadHeaderTimeout,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsShutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	<-errc
	return nil
}

func metricsHandler(m *Metrics) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/metrics" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", metricsContentType)
		m.WriteText(w)
	})
}
