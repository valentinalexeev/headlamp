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
	sleep    func(time.Duration) // replaced by tests

	mu       sync.Mutex
	observer func(task, kind string, ok bool)
}

var _ Notifier = (*HTTPNotifier)(nil)

func NewNotifier(store *Store, hc *http.Client, ntfyBase string, log *slog.Logger, now func() time.Time) *HTTPNotifier {
	return &HTTPNotifier{
		store: store, hc: hc, ntfyBase: strings.TrimRight(ntfyBase, "/"),
		log: log, now: now, sleep: time.Sleep,
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
	hit, err := t.Alert.When.Eval(result)
	if err != nil {
		// The engine rejects such a result through CheckResult already.
		n.log.Warn("alert condition not evaluable", "task", task)
		res.Alert = "failed: bad result"
		return res
	}
	if !hit {
		n.setState(task, func(s *TaskState) { s.AlertUntil = time.Time{} })
		res.Alert = "none"
		return res
	}
	if now.Before(n.store.State(task).AlertUntil) {
		res.Alert = "suppressed"
		return res
	}
	vars := make(map[string]any, len(result)+1)
	for k, v := range result {
		vars[k] = v
	}
	vars["task"] = task
	res.Alert = n.send(ctx, task, "alert", t.Alert.Notify, vars)
	if res.Alert == "sent" {
		until := now.Add(time.Duration(t.Alert.Cooldown))
		n.setState(task, func(s *TaskState) { s.AlertUntil = until })
	}
	return res
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
	if now.Before(n.store.State(task).FailNotifyUntil) {
		res.Alert = "suppressed"
		return res
	}
	vars := map[string]any{"task": task, "reason": capBytes(reason, maxNotifyReason)}
	res.Alert = n.send(ctx, task, "failure", t.OnFailure.Notify, vars)
	if res.Alert == "sent" {
		until := now.Add(time.Duration(t.OnFailure.Cooldown))
		n.setState(task, func(s *TaskState) { s.FailNotifyUntil = until })
	}
	return res
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

// ping GETs the healthcheck URL (suffix "/fail" for a failure ping).
func (n *HTTPNotifier) ping(ctx context.Context, task, secretName, suffix string) string {
	sec, err := n.store.Secrets([]string{secretName})
	if err != nil {
		n.log.Warn("ping not sent", "task", task, "kind", "ping", "outcome", failedNoSecret)
		n.observe(task, "ping", false)
		return failedNoSecret
	}
	base := sec[secretName]
	target := strings.TrimRight(base, "/") + suffix
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	var outcome string
	if err == nil {
		outcome = n.do(req, []string{base, target})
	} else {
		outcome = "failed: " + reasonText(err, []string{base, target})
	}
	n.log.Info("ping", "task", task, "kind", "ping", "outcome", outcome)
	n.observe(task, "ping", outcome == "sent")
	return outcome
}

// send publishes one ntfy message through the JSON API: the topic travels in
// the body, so it never appears in a URL, and UTF-8 text stays valid.
func (n *HTTPNotifier) send(ctx context.Context, task, kind string, nt Notify, vars map[string]any) string {
	sec, err := n.store.Secrets([]string{nt.Topic})
	if err != nil {
		n.log.Warn("notification not sent", "task", task, "kind", kind, "outcome", failedNoSecret)
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
		n.sleep(time.Duration(1<<(attempt-1)) * time.Second) // 1 s, then 2 s
	}
	n.log.Info("notification", "task", task, "kind", kind, "outcome", outcome)
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
