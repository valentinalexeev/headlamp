package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConditionOperators(t *testing.T) {
	cases := []struct {
		op   string
		v    float64
		got  float64
		want bool
	}{
		{"<", 1000, 999.99, true},
		{"<", 1000, 1000, false},
		{"<=", 1000, 1000, true},
		{"<=", 1000, 1000.01, false},
		{">", 1000, 1000, false},
		{">", 1000, 1000.01, true},
		{">=", 1000, 1000, true},
		{">=", 1000, 999.99, false},
		{"==", 1000, 1000, true},
		{"==", 1000, 999.99, false},
		{"!=", 1000, 999.99, true},
		{"!=", 1000, 1000, false},
	}
	for _, c := range cases {
		hit, err := Condition{Field: "x", Op: c.op, Value: c.v}.Eval(map[string]any{"x": c.got})
		if err != nil || hit != c.want {
			t.Errorf("%v %s %v: hit=%v err=%v want %v", c.got, c.op, c.v, hit, err, c.want)
		}
	}
	// json.Number is a number too (results decoded with UseNumber).
	if hit, err := (Condition{Field: "x", Op: "<", Value: 1000}).Eval(map[string]any{"x": json.Number("999.99")}); err != nil || !hit {
		t.Fatalf("json.Number: %v %v", hit, err)
	}
}

func TestConditionMissingFieldIsError(t *testing.T) {
	_, err := Condition{Field: "balance", Op: "<", Value: 1}.Eval(map[string]any{"other": 1.0})
	if err == nil || !strings.Contains(err.Error(), "balance") {
		t.Fatalf("want error naming the field, got %v", err)
	}
	if _, err := (Condition{Field: "balance", Op: "<", Value: 1}).Eval(nil); err == nil {
		t.Fatal("nil result must be an error")
	}
}

func TestConditionNonNumberIsError(t *testing.T) {
	for _, v := range []any{"842", true, nil, []any{1.0}, map[string]any{}} {
		hit, err := Condition{Field: "x", Op: "<", Value: 1000}.Eval(map[string]any{"x": v})
		if err == nil || hit {
			t.Errorf("%#v: hit=%v err=%v", v, hit, err)
		}
		if err != nil && strings.Contains(err.Error(), "842") {
			t.Errorf("error leaks the value: %v", err)
		}
	}
}

func TestCheckResult(t *testing.T) {
	withAlert := Task{Alert: &Alert{When: Condition{Field: "balance", Op: "<", Value: 1}}}
	if err := CheckResult(Task{}, map[string]any{"balance": "x"}); err != nil {
		t.Fatalf("no alert: %v", err)
	}
	if err := CheckResult(withAlert, map[string]any{"balance": 5.0}); err != nil {
		t.Fatal(err)
	}
	if err := CheckResult(withAlert, map[string]any{"balance": json.Number("5")}); err != nil {
		t.Fatal(err)
	}
	for _, r := range []map[string]any{nil, {}, {"balance": "842"}, {"balance": nil}} {
		err := CheckResult(withAlert, r)
		if err == nil || !strings.Contains(err.Error(), "balance") || strings.Contains(err.Error(), "842") {
			t.Errorf("%v: %v", r, err)
		}
	}
}

func TestRenderPlaceholders(t *testing.T) {
	vars := map[string]any{
		"int": 842.0, "frac": 1369.57, "s": "héllo", "b": true,
		"nested": map[string]any{"a": 1.0}, "list": []any{1.0}, "n": nil,
	}
	cases := map[string]string{
		"{int}":               "842",
		"{frac}":              "1369.57",
		"{s} {b}":             "héllo true",
		"{unknown}":           "{unknown}",
		"{nested} {list} {n}": "{nested} {list} {n}",
		"{}":                  "{}",
		"{a b}":               "{a b}",
		"{int:x}":             "{int:x}",
		"plain":               "plain",
		"{int}{int} {{int}}":  "842842 {842}",
		"{ int }":             "{ int }",
	}
	for in, want := range cases {
		if got := Render(in, vars); got != want {
			t.Errorf("Render(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---- senders ----

type ntfyReq struct {
	path, ctype, auth, query string
	body                     []byte
}

type fixture struct {
	t     *testing.T
	store *Store
	n     *HTTPNotifier
	now   time.Time
	logs  *bytes.Buffer

	mu       sync.Mutex
	ntfy     []ntfyReq
	ntfyCode []int // status per call; last one repeats
	pings    []string
	pingCode int
	sleeps   []time.Duration
	events   []string
	location string // Location header sent with ntfy responses
	ntfySrv  *httptest.Server
	pingSrv  *httptest.Server
}

const (
	testTopic = "s3cret-topic-xyz"
)

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), ntfyCode: []int{200}, pingCode: 200, logs: &bytes.Buffer{}}
	f.ntfySrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b := make([]byte, 1<<16)
		n, _ := r.Body.Read(b)
		f.mu.Lock()
		i := len(f.ntfy)
		f.ntfy = append(f.ntfy, ntfyReq{r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.URL.RawQuery, b[:n]})
		code := f.ntfyCode[len(f.ntfyCode)-1]
		if i < len(f.ntfyCode) {
			code = f.ntfyCode[i]
		}
		loc := f.location
		f.mu.Unlock()
		if loc != "" {
			w.Header().Set("Location", loc)
		}
		w.WriteHeader(code)
		w.Write([]byte("body mentions " + testTopic))
	}))
	f.pingSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.pings = append(f.pings, r.Method+" "+r.URL.RequestURI())
		code := f.pingCode
		f.mu.Unlock()
		w.WriteHeader(code)
	}))
	t.Cleanup(f.ntfySrv.Close)
	t.Cleanup(f.pingSrv.Close)

	f.store = openStore(t, newMemKV())
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.store.PutSecret("topic", testTopic))
	must(f.store.PutSecret("fail-topic", "failure-topic-abc"))
	must(f.store.PutSecret("hc", f.pingSrv.URL+"/ping/uuid-1234/"))

	log := slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	f.n = NewNotifier(f.store, f.ntfySrv.Client(), f.ntfySrv.URL+"/", log, func() time.Time { return f.now })
	f.n.sleep = func(_ context.Context, d time.Duration) { f.sleeps = append(f.sleeps, d) }
	f.n.SetObserver(func(task, kind string, ok bool) {
		f.events = append(f.events, task+"/"+kind+"/"+map[bool]string{true: "ok", false: "fail"}[ok])
	})
	return f
}

func alertTask() Task {
	return Task{
		Healthcheck: "hc",
		Alert: &Alert{
			When:     Condition{Field: "balance", Op: "<", Value: 1000},
			Cooldown: Duration(24 * time.Hour),
			Notify:   Notify{Topic: "topic", Priority: "high", Title: "Low balance", Message: "balance {balance} on {task}"},
		},
		OnFailure: &OnFailure{
			Cooldown: Duration(6 * time.Hour),
			Notify:   Notify{Topic: "fail-topic", Priority: "max", Title: "Task failed", Message: "{task}: {reason}"},
		},
	}
}

func (f *fixture) success(bal float64) NotifyResult {
	return f.n.Success(context.Background(), "tk", alertTask(), map[string]any{"balance": bal})
}

func (f *fixture) ntfyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ntfy)
}

func TestAlertSentOnceThenCooldown(t *testing.T) {
	f := newFixture(t)
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("first: %+v", r)
	}
	f.now = f.now.Add(time.Hour)
	if r := f.success(400); r.Alert != "suppressed" {
		t.Fatalf("second: %+v", r)
	}
	if f.ntfyCount() != 1 {
		t.Fatalf("ntfy calls = %d", f.ntfyCount())
	}
	want := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	if got := f.store.State("tk").AlertUntil; !got.Equal(want) {
		t.Fatalf("AlertUntil = %v, want %v", got, want)
	}
}

func TestAlertRepeatsAfterCooldownWhileStillLow(t *testing.T) {
	f := newFixture(t)
	f.success(500)
	f.now = f.now.Add(24 * time.Hour)
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	if f.ntfyCount() != 2 {
		t.Fatalf("ntfy calls = %d", f.ntfyCount())
	}
}

func TestAlertCooldownResetsWhenConditionClears(t *testing.T) {
	f := newFixture(t)
	f.success(500)
	f.now = f.now.Add(time.Hour)
	if r := f.success(5000); r.Alert != "none" {
		t.Fatalf("%+v", r)
	}
	if !f.store.State("tk").AlertUntil.IsZero() {
		t.Fatal("AlertUntil not cleared")
	}
	f.now = f.now.Add(time.Hour)
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	if f.ntfyCount() != 2 {
		t.Fatalf("ntfy calls = %d", f.ntfyCount())
	}
}

func TestAlertBadResultSendsNothing(t *testing.T) {
	f := newFixture(t)
	r := f.n.Success(context.Background(), "tk", alertTask(), map[string]any{"balance": "842"})
	if r.Alert != "failed: bad result" || f.ntfyCount() != 0 {
		t.Fatalf("%+v calls=%d", r, f.ntfyCount())
	}
}

func TestFailureNotificationHasOwnCooldown(t *testing.T) {
	f := newFixture(t)
	if r := f.n.Failure(context.Background(), "tk", alertTask(), "exit 3"); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	f.now = f.now.Add(time.Hour)
	if r := f.n.Failure(context.Background(), "tk", alertTask(), "exit 3"); r.Alert != "suppressed" {
		t.Fatalf("%+v", r)
	}
	f.now = f.now.Add(6 * time.Hour)
	if r := f.n.Failure(context.Background(), "tk", alertTask(), "exit 4"); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	// The alert cooldown is independent.
	if !f.store.State("tk").AlertUntil.IsZero() {
		t.Fatal("failure touched AlertUntil")
	}
	var last map[string]any
	json.Unmarshal(f.ntfy[len(f.ntfy)-1].body, &last)
	if last["message"] != "tk: exit 4" || last["topic"] != "failure-topic-abc" {
		t.Fatalf("%v", last)
	}
}

func TestFailureReasonCapped(t *testing.T) {
	f := newFixture(t)
	f.n.Failure(context.Background(), "tk", alertTask(), strings.Repeat("é", 1000))
	var m map[string]any
	json.Unmarshal(f.ntfy[0].body, &m)
	msg := m["message"].(string)
	if len(msg) > 400 {
		t.Fatalf("message %d bytes", len(msg))
	}
}

func TestPingSuccessAndFail(t *testing.T) {
	f := newFixture(t)
	tk := Task{Healthcheck: "hc"}
	if r := f.n.Success(context.Background(), "tk", tk, nil); r.Ping != "sent" || r.Alert != "" {
		t.Fatalf("%+v", r)
	}
	if r := f.n.Failure(context.Background(), "tk", tk, "boom"); r.Ping != "sent" || r.Alert != "" {
		t.Fatalf("%+v", r)
	}
	want := []string{"GET /ping/uuid-1234", "GET /ping/uuid-1234/fail"}
	if strings.Join(f.pings, "|") != strings.Join(want, "|") {
		t.Fatalf("pings = %v", f.pings)
	}
	f.pingCode = 500
	if r := f.n.Success(context.Background(), "tk", tk, nil); !strings.HasPrefix(r.Ping, "failed: ") {
		t.Fatalf("%+v", r)
	}
}

func TestNoHealthcheckNoAlertIsEmptyResult(t *testing.T) {
	f := newFixture(t)
	if r := f.n.Success(context.Background(), "tk", Task{}, nil); r != (NotifyResult{}) {
		t.Fatalf("%+v", r)
	}
}

func TestNtfyRetriesThenRecordsOutcome(t *testing.T) {
	f := newFixture(t)
	f.ntfyCode = []int{503, 429, 200}
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
	if f.ntfyCount() != 3 || len(f.sleeps) != 2 || f.sleeps[0] != time.Second || f.sleeps[1] != 2*time.Second {
		t.Fatalf("calls=%d sleeps=%v", f.ntfyCount(), f.sleeps)
	}

	g := newFixture(t)
	g.ntfyCode = []int{500}
	r := g.success(500)
	if !strings.HasPrefix(r.Alert, "failed: ") || g.ntfyCount() != 3 {
		t.Fatalf("%+v calls=%d", r, g.ntfyCount())
	}
	if strings.Contains(r.Alert, testTopic) || strings.Contains(r.Alert, g.ntfySrv.URL) || strings.Contains(r.Alert, "body mentions") {
		t.Fatalf("leaks: %q", r.Alert)
	}
}

func TestNtfyDoesNotRetryOther4xx(t *testing.T) {
	f := newFixture(t)
	f.ntfyCode = []int{403}
	r := f.success(500)
	if !strings.HasPrefix(r.Alert, "failed: ") || f.ntfyCount() != 1 || len(f.sleeps) != 0 {
		t.Fatalf("%+v calls=%d sleeps=%v", r, f.ntfyCount(), f.sleeps)
	}
}

func TestNtfyTransportErrorRetriedAndScrubbed(t *testing.T) {
	f := newFixture(t)
	f.ntfySrv.Close() // connection refused; the error text contains the URL
	r := f.success(500)
	if !strings.HasPrefix(r.Alert, "failed: ") || strings.Contains(r.Alert, f.ntfySrv.URL) || strings.Contains(r.Alert, "127.0.0.1") && strings.Contains(r.Alert, "http") {
		t.Fatalf("%q", r.Alert)
	}
	if len(f.sleeps) != 2 {
		t.Fatalf("sleeps=%v", f.sleeps)
	}
}

func TestFailedSendDoesNotSetCooldown(t *testing.T) {
	f := newFixture(t)
	f.ntfyCode = []int{500}
	f.success(500)
	if !f.store.State("tk").AlertUntil.IsZero() {
		t.Fatal("cooldown set after failed send")
	}
	f.ntfyCode = []int{200}
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("next run must retry: %+v", r)
	}
	g := newFixture(t)
	g.ntfyCode = []int{500}
	g.n.Failure(context.Background(), "tk", alertTask(), "x")
	if !g.store.State("tk").FailNotifyUntil.IsZero() {
		t.Fatal("failure cooldown set after failed send")
	}
}

func TestMissingSecretIsFailedNotPanic(t *testing.T) {
	f := newFixture(t)
	tk := alertTask()
	tk.Alert.Notify.Topic = "nope"
	tk.Healthcheck = "nope-hc"
	r := f.n.Success(context.Background(), "tk", tk, map[string]any{"balance": 1.0})
	if r.Alert != "failed: secret not configured" || r.Ping != "failed: secret not configured" {
		t.Fatalf("%+v", r)
	}
	if f.ntfyCount() != 0 {
		t.Fatal("sent without topic")
	}
}

func TestNtfyJSONBodyShape(t *testing.T) {
	f := newFixture(t)
	f.success(842.5)
	q := f.ntfy[0]
	if q.path != "/" || q.query != "" || q.ctype != "application/json" || q.auth != "" {
		t.Fatalf("%+v", q)
	}
	if strings.Contains(q.path, testTopic) {
		t.Fatal("topic in URL")
	}
	var m map[string]any
	if err := json.Unmarshal(q.body, &m); err != nil {
		t.Fatal(err)
	}
	if m["topic"] != testTopic || m["title"] != "Low balance" || m["message"] != "balance 842.5 on tk" || m["priority"] != 4.0 {
		t.Fatalf("%v", m)
	}
}

func TestNtfyPriorityNames(t *testing.T) {
	want := map[string]int{"min": 1, "low": 2, "default": 3, "high": 4, "max": 5, "": 3, "bogus": 3}
	for name, p := range want {
		if got := priorityNumber(name); got != p {
			t.Errorf("%q: %d want %d", name, got, p)
		}
	}
}

func TestNotifierNeverLogsTopicOrURL(t *testing.T) {
	f := newFixture(t)
	f.ntfyCode = []int{500, 500, 200}
	f.success(500)
	f.n.Failure(context.Background(), "tk", alertTask(), "boom")
	f.pingCode = 500
	f.success(5000)
	out := f.logs.String()
	if out == "" {
		t.Fatal("expected some logging")
	}
	for _, bad := range []string{testTopic, "failure-topic-abc", f.pingSrv.URL, "uuid-1234", f.ntfySrv.URL, "body mentions"} {
		if strings.Contains(out, bad) {
			t.Errorf("log contains %q:\n%s", bad, out)
		}
	}
}

func TestObserverReceivesEvents(t *testing.T) {
	f := newFixture(t)
	f.success(500)
	f.n.Failure(context.Background(), "tk", alertTask(), "x")
	want := "tk/ping/ok,tk/alert/ok,tk/ping/ok,tk/failure/ok"
	if got := strings.Join(f.events, ","); got != want {
		t.Fatalf("%s", got)
	}
	f.events = nil
	f.ntfyCode = []int{500}
	f.now = f.now.Add(48 * time.Hour)
	f.success(500)
	if got := strings.Join(f.events, ","); got != "tk/ping/ok,tk/alert/fail" {
		t.Fatalf("%s", got)
	}
}

func TestZeroObserverIsNoop(t *testing.T) {
	f := newFixture(t)
	f.n.SetObserver(nil)
	f.success(500) // must not panic
}

func TestConcurrentAlertSendsOnce(t *testing.T) {
	f := newFixture(t)
	f.n.SetObserver(nil)
	const N = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.success(500)
			mu.Lock()
			got[r.Alert]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got["sent"] != 1 || got["suppressed"] != N-1 || f.ntfyCount() != 1 {
		t.Fatalf("results %v, ntfy calls %d", got, f.ntfyCount())
	}
}

func TestConcurrentFailureSendsOnce(t *testing.T) {
	f := newFixture(t)
	f.n.SetObserver(nil)
	const N = 20
	var wg sync.WaitGroup
	var mu sync.Mutex
	got := map[string]int{}
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := f.n.Failure(context.Background(), "tk", alertTask(), "boom")
			mu.Lock()
			got[r.Alert]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if got["sent"] != 1 || got["suppressed"] != N-1 || f.ntfyCount() != 1 {
		t.Fatalf("results %v, ntfy calls %d", got, f.ntfyCount())
	}
}

func TestFailedSendRollsClaimBackToPrevious(t *testing.T) {
	f := newFixture(t)
	old := f.now.Add(-time.Hour) // an expired cooldown from an earlier alert
	f.store.UpdateState("tk", func(s *TaskState) { s.AlertUntil = old })
	f.ntfyCode = []int{500}
	f.success(500)
	if got := f.store.State("tk").AlertUntil; !got.Equal(old) {
		t.Fatalf("AlertUntil = %v, want the previous %v", got, old)
	}
	f.ntfyCode = []int{200}
	if r := f.success(500); r.Alert != "sent" {
		t.Fatalf("%+v", r)
	}
}

func TestReleaseDoesNotClobberNewerClaim(t *testing.T) {
	f := newFixture(t)
	mine := f.now.Add(time.Hour)
	newer := f.now.Add(5 * time.Hour)
	f.store.UpdateState("tk", func(s *TaskState) { s.AlertUntil = newer })
	f.n.release("tk", alertField, mine, time.Time{})
	if got := f.store.State("tk").AlertUntil; !got.Equal(newer) {
		t.Fatalf("clobbered: %v", got)
	}
	f.n.release("tk", alertField, newer, time.Time{})
	if !f.store.State("tk").AlertUntil.IsZero() {
		t.Fatal("own claim not released")
	}
}

func TestNoHitDoesNotWriteWhenAlreadyClear(t *testing.T) {
	f := newFixture(t)
	before := f.store.State("tk")
	f.success(5000)
	if f.store.State("tk") != before {
		t.Fatal("state changed")
	}
}

func TestFailurePingKeepsQueryString(t *testing.T) {
	cases := []struct{ in, ok, fail string }{
		{"https://hc.example/ping/uuid", "https://hc.example/ping/uuid", "https://hc.example/ping/uuid/fail"},
		{"https://hc.example/ping/uuid/", "https://hc.example/ping/uuid", "https://hc.example/ping/uuid/fail"},
		{"https://hc.example/ping/uuid?x=1", "https://hc.example/ping/uuid?x=1", "https://hc.example/ping/uuid/fail?x=1"},
		{"https://hc.example/ping/uuid/?x=1&y=2#frag", "https://hc.example/ping/uuid?x=1&y=2", "https://hc.example/ping/uuid/fail?x=1&y=2"},
	}
	for _, c := range cases {
		if got, ok := pingURL(c.in, ""); !ok || got != c.ok {
			t.Errorf("%q ok-ping: %q %v", c.in, got, ok)
		}
		if got, ok := pingURL(c.in, "/fail"); !ok || got != c.fail {
			t.Errorf("%q fail-ping: %q %v", c.in, got, ok)
		}
	}
}

func TestPingBadURLNotEchoed(t *testing.T) {
	f := newFixture(t)
	f.store.PutSecret("badhc", "ht!tp://%zz-secret-host")
	r := f.n.Success(context.Background(), "tk", Task{Healthcheck: "badhc"}, nil)
	if r.Ping != "failed: bad ping url" {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(f.logs.String(), "secret-host") {
		t.Fatal("log echoes URL")
	}
}

func TestPingQueryReachesServer(t *testing.T) {
	f := newFixture(t)
	f.store.PutSecret("qhc", f.pingSrv.URL+"/ping/u?x=1")
	f.n.Failure(context.Background(), "tk", Task{Healthcheck: "qhc"}, "boom")
	if len(f.pings) != 1 || f.pings[0] != "GET /ping/u/fail?x=1" {
		t.Fatalf("%v", f.pings)
	}
}

func TestPingTransportErrorDoesNotLeak(t *testing.T) {
	f := newFixture(t)
	f.pingSrv.Close()
	r := f.n.Success(context.Background(), "tk", Task{Healthcheck: "hc"}, nil)
	if !strings.HasPrefix(r.Ping, "failed: ") {
		t.Fatalf("%+v", r)
	}
	for _, bad := range []string{f.pingSrv.URL, "uuid-1234", "/ping/", "http"} {
		if strings.Contains(r.Ping, bad) {
			t.Errorf("ping outcome %q contains %q", r.Ping, bad)
		}
	}
}

func TestRetryWaitHonoursContext(t *testing.T) {
	f := newFixture(t)
	f.n.sleep = waitCtx // the real wait
	f.ntfyCode = []int{500}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	r := f.n.Success(ctx, "tk", alertTask(), map[string]any{"balance": 1.0})
	if d := time.Since(start); d > 700*time.Millisecond {
		t.Fatalf("took %v", d)
	}
	if !strings.HasPrefix(r.Alert, "failed: ") || f.ntfyCount() != 1 {
		t.Fatalf("%+v calls=%d", r, f.ntfyCount())
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	f := newFixture(t)
	f.ntfyCode = []int{307}
	f.location = f.pingSrv.URL + "/elsewhere"
	r := f.success(500)
	if !strings.HasPrefix(r.Alert, "failed: ") || f.ntfyCount() != 1 {
		t.Fatalf("%+v calls=%d", r, f.ntfyCount())
	}
	for _, p := range f.pings {
		if strings.Contains(p, "elsewhere") {
			t.Fatalf("redirect followed: %v", f.pings)
		}
	}
	// The caller's client is left alone.
	if f.ntfySrv.Client().CheckRedirect != nil {
		t.Fatal("caller's client modified")
	}
}

func TestFailureReasonScrubbed(t *testing.T) {
	f := newFixture(t)
	reason := "curl " + f.pingSrv.URL + "/ping/uuid-1234 topic " + testTopic + " failed"
	f.n.Failure(context.Background(), "tk", alertTask(), reason)
	var m map[string]any
	json.Unmarshal(f.ntfy[0].body, &m)
	msg := m["message"].(string)
	for _, bad := range []string{testTopic, "uuid-1234", f.pingSrv.URL} {
		if strings.Contains(msg, bad) {
			t.Errorf("message contains %q: %s", bad, msg)
		}
	}
}
