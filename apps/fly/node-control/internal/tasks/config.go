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

// Target is either a named node or a tag selector, never both.
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

// presence records which fields with a non-zero default were written in the
// file, so an explicit "0s" can be rejected while an omitted field gets its
// default. Types stay plain values; this is only a second, lenient decode.
type presence struct {
	Tasks map[string]struct {
		Timeout *json.RawMessage `json:"timeout"`
		Retry   *struct {
			Attempts *json.RawMessage `json:"attempts"`
		} `json:"retry"`
		Alert *struct {
			Cooldown *json.RawMessage `json:"cooldown"`
		} `json:"alert"`
		OnFailure *struct {
			Cooldown *json.RawMessage `json:"cooldown"`
		} `json:"on_failure"`
	} `json:"tasks"`
}

// ParseConfig decodes strictly (unknown fields are errors), applies defaults
// and validates. Every validation problem is reported, not just the first.
func ParseConfig(b []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("tasks config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("tasks config: trailing data after JSON document")
	}
	var seen presence
	if err := json.Unmarshal(b, &seen); err != nil {
		return nil, fmt.Errorf("tasks config: %w", err)
	}

	ids := make([]string, 0, len(cfg.Tasks))
	for id := range cfg.Tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var errs []error
	for _, id := range ids {
		t := cfg.Tasks[id]
		p := seen.Tasks[id]
		bad := func(field, format string, args ...any) {
			errs = append(errs, fmt.Errorf("task %s: %s: %s", id, field, fmt.Sprintf(format, args...)))
		}

		if strings.TrimSpace(id) == "" {
			errs = append(errs, errors.New("task: empty task id"))
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
		} else if t.Timeout <= 0 || time.Duration(t.Timeout) > maxTimeout {
			bad("timeout", "%s is outside (0, %s]", time.Duration(t.Timeout), maxTimeout)
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
