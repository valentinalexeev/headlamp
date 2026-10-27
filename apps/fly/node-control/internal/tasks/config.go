// Package tasks holds the scheduled-task model: config types and validation,
// the scheduler and the runner client build on them.
package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"
)

const (
	defaultTimeout  = 3 * time.Minute
	minTimeout      = time.Second // the runner takes whole seconds, at least 1
	maxTimeout      = 30 * time.Minute
	minEvery        = time.Minute
	defaultCooldown = 24 * time.Hour
	maxAttempts     = 10
)

// Duration is a time.Duration that travels as a Go duration string ("12h").
// Bare numbers are rejected: their unit would be a guess.
type Duration time.Duration

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"12h\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duration: %w", err)
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	Tasks map[string]Task `json:"tasks"`
}

type Task struct {
	Script      string         `json:"script"` // file name, key into stored scripts
	Params      map[string]any `json:"params,omitempty"`
	Target      Target         `json:"target"`
	Schedule    Schedule       `json:"schedule"`
	Timeout     Duration       `json:"timeout"`
	Retry       Retry          `json:"retry"`
	Secrets     []string       `json:"secrets,omitempty"`     // secret names passed to the script
	Healthcheck string         `json:"healthcheck,omitempty"` // secret name holding the ping URL
	Alert       *Alert         `json:"alert,omitempty"`
	OnFailure   *OnFailure     `json:"on_failure,omitempty"`
}

// Target is either a named node or a tag selector, never both. Node is the
// node's headscale given name (unique in headscale; fixed once the admin
// renames the node), never the host name the node reports. Either way only
// nodes that carry the runner tag (TASKS_RUNNER_TAG) are candidates. A node
// can carry a tag it advertised itself if the policy's tagOwners lets its user
// own it (at registration; older headscale also later), so the trust boundary
// is that tagOwners entry: list only the admin/service identity as its owner.
// See WithTag and Select.
type Target struct {
	Node   string   `json:"node,omitempty"`
	Tags   []string `json:"tags,omitempty"`
	Online bool     `json:"online,omitempty"`
}

type Schedule struct {
	Every  Duration `json:"every"`
	Jitter Duration `json:"jitter,omitempty"`
}

type Retry struct {
	Attempts int        `json:"attempts"`
	Backoff  []Duration `json:"backoff,omitempty"`
}

// Condition compares one numeric result field with a constant.
type Condition struct {
	Field string  `json:"field"`
	Op    string  `json:"op"` // < <= > >= == !=
	Value float64 `json:"value"`
}

// Notify.Topic is a secret NAME, so the topic never sits in the config file.
type Notify struct {
	Topic    string `json:"topic,omitempty"`
	Priority string `json:"priority,omitempty"`
	Title    string `json:"title,omitempty"`
	Message  string `json:"message,omitempty"`
}

type Alert struct {
	When     Condition `json:"when"`
	Cooldown Duration  `json:"cooldown,omitempty"`
	Notify   Notify    `json:"notify"`
}

type OnFailure struct {
	Notify   Notify   `json:"notify"`
	Cooldown Duration `json:"cooldown,omitempty"`
}

// shadow is a lenient view of one task's raw JSON. It serves two purposes:
// naming the field of a bad duration (encoding/json drops the field name when a
// custom unmarshaler fails) and telling an omitted field from an explicit zero,
// so "0s" is rejected while a missing timeout gets its default.
type shadow struct {
	Timeout  *json.RawMessage `json:"timeout"`
	Schedule *struct {
		Every  *json.RawMessage `json:"every"`
		Jitter *json.RawMessage `json:"jitter"`
	} `json:"schedule"`
	Retry *struct {
		Attempts *json.RawMessage  `json:"attempts"`
		Backoff  []json.RawMessage `json:"backoff"`
	} `json:"retry"`
	Alert *struct {
		Cooldown *json.RawMessage `json:"cooldown"`
	} `json:"alert"`
	OnFailure *struct {
		Cooldown *json.RawMessage `json:"cooldown"`
	} `json:"on_failure"`
}

// durationErrors lists every duration field of the task that does not parse.
func (s *shadow) durationErrors() []string {
	var out []string
	check := func(field string, raw *json.RawMessage) {
		if raw == nil {
			return
		}
		var d Duration
		if err := d.UnmarshalJSON(*raw); err != nil {
			out = append(out, fmt.Sprintf("%s: %v", field, err))
		}
	}
	check("timeout", s.Timeout)
	if s.Schedule != nil {
		check("schedule.every", s.Schedule.Every)
		check("schedule.jitter", s.Schedule.Jitter)
	}
	if s.Retry != nil {
		for i := range s.Retry.Backoff {
			check(fmt.Sprintf("retry.backoff[%d]", i), &s.Retry.Backoff[i])
		}
	}
	if s.Alert != nil {
		check("alert.cooldown", s.Alert.Cooldown)
	}
	if s.OnFailure != nil {
		check("on_failure.cooldown", s.OnFailure.Cooldown)
	}
	return out
}

// Ids end up in Redis field names, URL paths and metric labels.
var taskIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// rawTasks splits the "tasks" object into per-task raw JSON, in file order,
// and reports duplicate keys (a map would collapse them silently).
func rawTasks(raw json.RawMessage) (ids []string, bodies map[string]json.RawMessage, errs []error) {
	bodies = map[string]json.RawMessage{}
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		errs = append(errs, errors.New("tasks: must be an object keyed by task id"))
		return
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			errs = append(errs, fmt.Errorf("tasks: %w", err))
			return
		}
		id, _ := tok.(string)
		var body json.RawMessage
		if err := dec.Decode(&body); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
			return
		}
		if _, dup := bodies[id]; dup {
			errs = append(errs, fmt.Errorf("task %s: duplicate task id", id))
			continue
		}
		bodies[id] = body
		ids = append(ids, id)
	}
	return
}

// ParseConfig decodes strictly (unknown fields are errors), applies defaults
// and validates. Every problem is reported, each naming its task and field.
func ParseConfig(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var top struct {
		Tasks json.RawMessage `json:"tasks"`
	}
	if err := dec.Decode(&top); err != nil {
		return nil, fmt.Errorf("tasks config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("tasks config: trailing data after JSON document")
	}

	ids, bodies, errs := rawTasks(top.Tasks)
	sort.Strings(ids)
	cfg := Config{Tasks: map[string]Task{}}

	for _, id := range ids {
		bad := func(field, format string, args ...any) {
			errs = append(errs, fmt.Errorf("task %s: %s: %s", id, field, fmt.Sprintf(format, args...)))
		}
		if !taskIDRe.MatchString(id) {
			bad("id", "%q must match %s", id, taskIDRe)
			continue
		}

		var p shadow
		if json.Unmarshal(bodies[id], &p) == nil {
			if de := p.durationErrors(); len(de) > 0 {
				for _, e := range de {
					errs = append(errs, fmt.Errorf("task %s: %s", id, e))
				}
				continue
			}
		}
		var t Task
		td := json.NewDecoder(bytes.NewReader(bodies[id]))
		td.DisallowUnknownFields()
		if err := td.Decode(&t); err != nil {
			errs = append(errs, fmt.Errorf("task %s: %w", id, err))
			continue
		}

		validateScript(t.Script, bad)

		switch {
		case t.Target.Node != "" && len(t.Target.Tags) > 0:
			bad("target", "set either node or tags, not both")
		case t.Target.Node == "" && len(t.Target.Tags) == 0:
			bad("target", "set node or tags")
		}
		for _, tag := range t.Target.Tags {
			if strings.TrimSpace(tag) == "" {
				bad("target.tags", "empty tag")
			}
		}

		if time.Duration(t.Schedule.Every) < minEvery {
			bad("schedule.every", "%s is below the %s minimum", time.Duration(t.Schedule.Every), minEvery)
		}
		if t.Schedule.Jitter < 0 {
			bad("schedule.jitter", "must not be negative")
		}

		if p.Timeout == nil {
			t.Timeout = Duration(defaultTimeout)
		} else if time.Duration(t.Timeout) < minTimeout || time.Duration(t.Timeout) > maxTimeout {
			bad("timeout", "%s is outside [%s, %s]", time.Duration(t.Timeout), minTimeout, maxTimeout)
		}

		if p.Retry == nil || p.Retry.Attempts == nil {
			t.Retry.Attempts = 1
		} else if t.Retry.Attempts < 1 || t.Retry.Attempts > maxAttempts {
			bad("retry.attempts", "%d is outside 1..%d", t.Retry.Attempts, maxAttempts)
		}
		if t.Retry.Attempts > 1 && len(t.Retry.Backoff) == 0 {
			bad("retry.backoff", "required when attempts > 1")
		}
		for i, d := range t.Retry.Backoff {
			if d <= 0 {
				bad("retry.backoff", "entry %d must be positive", i)
			}
		}

		for _, s := range t.Secrets {
			if strings.TrimSpace(s) == "" {
				bad("secrets", "empty secret name")
			}
		}

		if t.Alert != nil {
			if t.Alert.When.Field == "" {
				bad("alert.when.field", "required")
			}
			switch t.Alert.When.Op {
			case "<", "<=", ">", ">=", "==", "!=":
			default:
				bad("alert.when.op", "unknown operator %q (use < <= > >= == !=)", t.Alert.When.Op)
			}
			if p.Alert == nil || p.Alert.Cooldown == nil {
				t.Alert.Cooldown = Duration(defaultCooldown)
			} else if t.Alert.Cooldown <= 0 {
				bad("alert.cooldown", "must be positive")
			}
			validateNotify("alert.notify", t.Alert.Notify, bad)
		}
		if t.OnFailure != nil {
			if p.OnFailure == nil || p.OnFailure.Cooldown == nil {
				t.OnFailure.Cooldown = Duration(defaultCooldown)
			} else if t.OnFailure.Cooldown <= 0 {
				bad("on_failure.cooldown", "must be positive")
			}
			validateNotify("on_failure.notify", t.OnFailure.Notify, bad)
		}

		cfg.Tasks[id] = t
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return &cfg, nil
}

// validateScript keeps the name a plain file name: it is a key into stored
// scripts and may end up in a path on the runner.
func validateScript(name string, bad func(field, format string, args ...any)) {
	switch {
	case name == "":
		bad("script", "required")
	case strings.ContainsAny(name, `/\`) || strings.Contains(name, ".."):
		bad("script", "%q must be a plain file name", name)
	}
}

var placeholderRe = regexp.MustCompile(`\{([^{}]*)\}`)

// Placeholder names refer to result fields and are checked at run time. A
// colon would be a namespaced reference (for example to a secret), which
// templates must never have.
func validateNotify(field string, n Notify, bad func(field, format string, args ...any)) {
	for _, m := range placeholderRe.FindAllStringSubmatch(n.Message+"\x00"+n.Title, -1) {
		if strings.Contains(m[1], ":") {
			bad(field+"."+messageField(n, m[0]), "placeholder {%s} not allowed: only plain result field names", m[1])
		}
	}
}

func messageField(n Notify, ph string) string {
	if strings.Contains(n.Message, ph) {
		return "message"
	}
	return "title"
}
