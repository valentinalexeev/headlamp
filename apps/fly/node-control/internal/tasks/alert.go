package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"example.com/node-control/internal/scrub"
)

const (
	sendAttempts    = 3
	pingTimeout     = 10 * time.Second
	sendTimeout     = 15 * time.Second
	maxNotifyReason = 300
	maxDrain        = 4 << 10
)

// failedNoSecret is the only text a missing secret ever produces: the NAME of the
// secret stays out of results, which end up on run records and in the UI.
const failedNoSecret = "failed: secret not configured"

// jsonNumber converts the number types a decoded result can hold.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// Eval compares the result field with the constant. A missing or non-numeric
// field is an error rather than a miss: a script that silently stops reporting
// the number would otherwise never alert. The error names the field, never the
// value.
func (c Condition) Eval(result map[string]any) (bool, error) {
	raw, ok := result[c.Field]
	if !ok {
		return false, fmt.Errorf("result has no field %q", c.Field)
	}
	v, ok := jsonNumber(raw)
	if !ok {
		return false, fmt.Errorf("result field %q is not a number", c.Field)
	}
	switch c.Op {
	case "<":
		return v < c.Value, nil
	case "<=":
		return v <= c.Value, nil
	case ">":
		return v > c.Value, nil
	case ">=":
		return v >= c.Value, nil
	case "==":
		return v == c.Value, nil
	case "!=":
		return v != c.Value, nil
	}
	return false, fmt.Errorf("unknown operator %q", c.Op)
}

// CheckResult lets the engine treat an unusable result as a failed attempt
// before any notification logic runs.
func CheckResult(t Task, result map[string]any) error {
	if t.Alert == nil {
		return nil
	}
	_, err := t.Alert.When.Eval(result)
	return err
}

var renderRe = regexp.MustCompile(`\{[A-Za-z0-9_]+\}`)

// Render replaces {name} with a scalar from vars. Anything else (unknown name,
// nested value, null) stays as written so a typo is visible in the message.
func Render(tpl string, vars map[string]any) string {
	return renderRe.ReplaceAllStringFunc(tpl, func(m string) string {
		v, ok := vars[m[1:len(m)-1]]
		if !ok {
			return m
		}
		switch x := v.(type) {
		case string:
			return x
		case bool:
			return strconv.FormatBool(x)
		default:
			if f, ok := jsonNumber(v); ok {
				return strconv.FormatFloat(f, 'f', -1, 64)
			}
		}
		return m
	})
}

// priorityNumber maps the ntfy priority names to the 1-5 scale of the JSON API.
func priorityNumber(name string) int {
	switch name {
	case "min":
		return 1
	case "low":
		return 2
	case "high":
		return 4
	case "max":
		return 5
	}
	return 3
}

// NotifyResult is stored on the run record. "" means not applicable.
type NotifyResult struct{ Alert, Ping string } // "sent", "suppressed", "none" or "failed: <reason>"

type Notifier interface {
	Success(ctx context.Context, task string, t Task, result map[string]any) NotifyResult
	Failure(ctx context.Context, task string, t Task, reason string) NotifyResult
}

// HTTPNotifier sends ntfy messages and healthcheck pings.
type HTTPNotifier struct {
	store    *Store
	hc       *http.Client
	ntfyBase string
	log      *slog.Logger
	now      func() time.Time
	sleep    func(context.Context, time.Duration) // replaced by tests

	mu       sync.Mutex
	observer func(task, kind string, ok bool)

	// inflight holds the cooldown slots a send is using right now (key: task
	// and kind). It is memory only: the cooldown itself is stored after the
	// send succeeded, so a process that dies mid-send leaves no cooldown and
	// the next run alerts again (a duplicate, never a missed alert).
	resMu    sync.Mutex
	inflight map[string]bool
}

var _ Notifier = (*HTTPNotifier)(nil)

func NewNotifier(store *Store, hc *http.Client, ntfyBase string, log *slog.Logger, now func() time.Time) *HTTPNotifier {
	// Copy the client: a redirect must never re-POST the topic or take a ping
	// somewhere else, and the caller's client stays untouched.
	var c http.Client
	if hc != nil {
		c = *hc
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPNotifier{
		store: store, hc: &c, ntfyBase: strings.TrimRight(ntfyBase, "/"),
		log: log, now: now, sleep: waitCtx, inflight: map[string]bool{},
	}
}

// waitCtx sleeps for d or until ctx is done.
func waitCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// SetObserver registers a callback for every send that was attempted; kind is
// "alert", "failure" or "ping". nil removes it.
func (n *HTTPNotifier) SetObserver(f func(task, kind string, ok bool)) {
	n.mu.Lock()
	n.observer = f
	n.mu.Unlock()
}

func (n *HTTPNotifier) observe(task, kind string, ok bool) {
	n.mu.Lock()
	f := n.observer
	n.mu.Unlock()
	if f != nil {
		f(task, kind, ok)
	}
}

func (n *HTTPNotifier) Success(ctx context.Context, task string, t Task, result map[string]any) NotifyResult {
	var res NotifyResult
	now := n.now()
	if t.Healthcheck != "" {
		res.Ping = n.ping(ctx, task, t.Healthcheck, "")
	}
	if t.Alert == nil {
		return res
	}
	hit, outcome := n.alert(ctx, task, t, result, now)
	res.Alert = outcome
	n.log.Info("alert_evaluated", "task", task, "hit", hit, "outcome", outcome)
	return res
}

// alert evaluates the condition and sends the alert when it holds and the
// cooldown allows; it returns whether it held and the outcome text.
func (n *HTTPNotifier) alert(ctx context.Context, task string, t Task, result map[string]any, now time.Time) (bool, string) {
	hit, err := t.Alert.When.Eval(result)
	if err != nil {
		// The engine rejects such a result through CheckResult already.
		return false, "failed: bad result"
	}
	if !hit {
		// Skip the write (and its Redis flush) when there is nothing to clear.
		if !n.store.State(task).AlertUntil.IsZero() {
			n.setState(task, func(s *TaskState) { s.AlertUntil = time.Time{} })
		}
		return false, "none"
	}
	if !n.reserve(task, "alert", now, alertField) {
		return true, "suppressed"
	}
	vars := make(map[string]any, len(result)+1)
	for k, v := range result {
		vars[k] = v
	}
	vars["task"] = task
	var outcome string // stays "" if send panics: the slot is freed, nothing stored
	defer func() {
		n.settle(task, "alert", alertField, now.Add(time.Duration(t.Alert.Cooldown)), outcome == "sent")
	}()
	outcome = n.send(ctx, task, "alert", t.Alert.Notify, vars)
	return true, outcome
}

func (n *HTTPNotifier) Failure(ctx context.Context, task string, t Task, reason string) NotifyResult {
	var res NotifyResult
	now := n.now()
	if t.Healthcheck != "" {
		res.Ping = n.ping(ctx, task, t.Healthcheck, "/fail")
	}
	if t.OnFailure == nil {
		return res
	}
	if !n.reserve(task, "failure", now, failField) {
		res.Alert = "suppressed"
		return res
	}
	reason = capBytes(scrub.Scrub(reason, n.known(t)), maxNotifyReason)
	vars := map[string]any{"task": task, "reason": reason}
	var sent bool // stays false if send panics: the slot is freed, nothing stored
	defer func() { n.settle(task, "failure", failField, now.Add(time.Duration(t.OnFailure.Cooldown)), sent) }()
	res.Alert = n.send(ctx, task, "failure", t.OnFailure.Notify, vars)
	sent = res.Alert == "sent"
	return res
}

func alertField(s *TaskState) *time.Time { return &s.AlertUntil }
func failField(s *TaskState) *time.Time  { return &s.FailNotifyUntil }

// reserve takes the cooldown slot of task and kind for one send: it fails
// while the stored cooldown runs or another send for the same slot is in
// flight, so concurrent calls cannot all send. Nothing is stored here, and the
// send itself happens with no lock held.
func (n *HTTPNotifier) reserve(task, kind string, now time.Time, field func(*TaskState) *time.Time) bool {
	key := task + "\x00" + kind
	n.resMu.Lock()
	defer n.resMu.Unlock()
	if n.inflight[key] {
		return false
	}
	st := n.store.State(task)
	if now.Before(*field(&st)) {
		return false
	}
	n.inflight[key] = true
	return true
}

// settle ends a reservation; callers defer it, so a panic in the send frees
// the slot too. Only a successful send stores the cooldown, and
// it is stored before the slot is freed, so a caller that reserves next sees
// either the reservation or the cooldown. A failed or interrupted send stores
// nothing: the next run retries.
func (n *HTTPNotifier) settle(task, kind string, field func(*TaskState) *time.Time, until time.Time, sent bool) {
	if sent {
		n.setState(task, func(s *TaskState) { *field(s) = until })
	}
	n.resMu.Lock()
	delete(n.inflight, task+"\x00"+kind)
	n.resMu.Unlock()
}

// known lists the secret values this notifier resolves for the task, for
// scrubbing text that came from outside (a failure reason).
func (n *HTTPNotifier) known(t Task) []string {
	names := []string{t.Healthcheck}
	if t.Alert != nil {
		names = append(names, t.Alert.Notify.Topic)
	}
	if t.OnFailure != nil {
		names = append(names, t.OnFailure.Notify.Topic)
	}
	out := []string{n.ntfyBase}
	for _, name := range names {
		if name == "" {
			continue
		}
		if m, err := n.store.Secrets([]string{name}); err == nil {
			out = append(out, m[name])
			if tr := strings.TrimRight(m[name], "/"); tr != m[name] {
				out = append(out, tr) // a ping URL as a tool would print it
			}
		}
	}
	// Longest first: a value that contains a shorter one (a ping URL on the
	// ntfy host, say) must be replaced whole, not leave its tail behind.
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

func (n *HTTPNotifier) setState(task string, fn func(*TaskState)) {
	if err := n.store.UpdateState(task, fn); err != nil {
		n.log.Warn("task state not persisted", "task", task, "err", scrub.Scrub(err.Error(), nil))
	}
}

func capBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ping GETs the healthcheck URL (suffix "/fail" for a failure ping), logs
// the outcome and reports it to the observer.
func (n *HTTPNotifier) ping(ctx context.Context, task, secretName, suffix string) string {
	outcome := n.pingOnce(ctx, secretName, suffix)
	if outcome == "sent" {
		n.log.Info("ping_result", "task", task, "kind", "ping", "outcome", outcome)
	} else {
		n.log.Warn("ping_result", "task", task, "kind", "ping", "outcome", outcome)
	}
	n.observe(task, "ping", outcome == "sent")
	return outcome
}

// heartbeatTask is the task label of the scheduler heartbeat ping.
const heartbeatTask = "scheduler"

// PingSecret pings the URL stored under the secret name, for the scheduler
// heartbeat. The secret is read on every call. The outcome goes to the
// observer (task "scheduler", kind "ping") but is not logged here: the caller
// logs it when it changes. The URL is never logged or returned.
func (n *HTTPNotifier) PingSecret(ctx context.Context, secretName string) string {
	outcome := n.pingOnce(ctx, secretName, "")
	n.observe(heartbeatTask, "ping", outcome == "sent")
	return outcome
}

// pingOnce performs one ping and returns its outcome text, which carries no
// URL. It neither logs nor observes.
func (n *HTTPNotifier) pingOnce(ctx context.Context, secretName, suffix string) string {
	sec, err := n.store.Secrets([]string{secretName})
	if err != nil {
		return failedNoSecret
	}
	base := sec[secretName]
	target, ok := pingURL(base, suffix)
	if !ok {
		return "failed: bad ping url"
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return "failed: " + reasonText(err, []string{base, target})
	}
	return n.do(req, []string{base, target})
}

// pingURL appends suffix to the URL's path (not after its query) and drops any
// fragment. ok is false when the URL does not parse; callers must not echo it.
func pingURL(raw, suffix string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", false
	}
	u.Path = strings.TrimRight(u.Path, "/") + suffix
	if u.RawPath != "" {
		u.RawPath = strings.TrimRight(u.RawPath, "/") + suffix
	}
	u.Fragment, u.RawFragment = "", ""
	return u.String(), true
}

// send publishes one ntfy message through the JSON API: the topic travels in
// the body, so it never appears in a URL, and UTF-8 text stays valid.
func (n *HTTPNotifier) send(ctx context.Context, task, kind string, nt Notify, vars map[string]any) string {
	sec, err := n.store.Secrets([]string{nt.Topic})
	if err != nil {
		n.log.Warn("notify_result", "task", task, "kind", kind, "outcome", failedNoSecret)
		n.observe(task, kind, false)
		return failedNoSecret
	}
	topic := sec[nt.Topic]
	body, err := json.Marshal(map[string]any{
		"topic":    topic,
		"title":    Render(nt.Title, vars),
		"message":  Render(nt.Message, vars),
		"priority": priorityNumber(nt.Priority),
	})
	if err != nil {
		n.log.Warn("notify_result", "task", task, "kind", kind, "outcome", "failed: encode")
		n.observe(task, kind, false)
		return "failed: encode"
	}
	secrets := []string{topic, n.ntfyBase}
	var outcome string
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, sendTimeout)
		req, rerr := http.NewRequestWithContext(cctx, http.MethodPost, n.ntfyBase+"/", bytes.NewReader(body))
		if rerr != nil {
			cancel()
			outcome = "failed: " + reasonText(rerr, secrets)
			break
		}
		req.Header.Set("Content-Type", "application/json")
		outcome = n.do(req, secrets)
		cancel()
		if outcome == "sent" || !retryable(outcome) || attempt >= sendAttempts || ctx.Err() != nil {
			break
		}
		n.sleep(ctx, time.Duration(1<<(attempt-1))*time.Second) // 1 s, then 2 s
		if ctx.Err() != nil {
			break
		}
	}
	if outcome == "sent" {
		n.log.Info("notify_result", "task", task, "kind", kind, "outcome", outcome)
	} else {
		n.log.Warn("notify_result", "task", task, "kind", kind, "outcome", outcome)
	}
	n.observe(task, kind, outcome == "sent")
	return outcome
}

// do performs one request. The outcome is "sent" for 2xx; otherwise a failure
// text with no URL, topic or response body. Retryable failures carry a marker
// that retryable() recognises.
func (n *HTTPNotifier) do(req *http.Request, secrets []string) string {
	resp, err := n.hc.Do(req)
	if err != nil {
		return "failed: " + reasonText(err, secrets)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return "sent"
	}
	return "failed: status " + strconv.Itoa(resp.StatusCode)
}

// retryable: transport errors, 429 and 5xx. Other 4xx will not improve.
func retryable(outcome string) bool {
	rest, ok := strings.CutPrefix(outcome, "failed: status ")
	if !ok {
		return strings.HasPrefix(outcome, "failed: ")
	}
	code, err := strconv.Atoi(rest)
	return err != nil || code == http.StatusTooManyRequests || code >= 500
}

// reasonText drops the request URL net/http puts into its errors, then scrubs
// the secrets from whatever is left.
func reasonText(err error, secrets []string) string {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return scrub.Scrub(err.Error(), secrets)
}
