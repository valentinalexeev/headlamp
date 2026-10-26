package tasks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const specExample = `{
  "tasks": {
    "check-balance": {
      "script": "check-balance.js",
      "target": { "tags": ["tag:region-xx"], "online": true },
      "schedule": { "every": "12h", "jitter": "20m" },
      "timeout": "3m",
      "retry": { "attempts": 4, "backoff": ["1m", "5m", "20m", "1h"] },
      "secrets": ["EXAMPLE_LOGIN", "EXAMPLE_PASSWORD"],
      "alert": {
        "when": { "field": "balance", "op": "<", "value": 1000 },
        "cooldown": "24h",
        "notify": { "priority": "high", "title": "Balance low", "message": "Balance {balance} units" }
      },
      "on_failure": { "notify": { "priority": "default", "title": "Check failed", "message": "{reason}" } }
    }
  }
}`

func TestParseConfigAcceptsSpecExample(t *testing.T) {
	cfg, err := ParseConfig([]byte(specExample))
	if err != nil {
		t.Fatal(err)
	}
	task, ok := cfg.Tasks["check-balance"]
	if !ok {
		t.Fatal("task missing")
	}
	if len(task.Retry.Backoff) != 4 || task.Retry.Backoff[3] != Duration(time.Hour) {
		t.Fatalf("backoff = %v", task.Retry.Backoff)
	}
	if task.Schedule.Every != Duration(12*time.Hour) || task.Schedule.Jitter != Duration(20*time.Minute) {
		t.Fatalf("schedule = %+v", task.Schedule)
	}
	if task.Alert == nil || task.Alert.When.Op != "<" || task.Alert.When.Value != 1000 {
		t.Fatalf("alert = %+v", task.Alert)
	}
	if task.OnFailure == nil || task.OnFailure.Cooldown != Duration(24*time.Hour) {
		t.Fatalf("on_failure cooldown default not applied: %+v", task.OnFailure)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"tasks":{"a":{"script":"a.js","target":{"node":"n1"},"schedule":{"every":"1h"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	a := cfg.Tasks["a"]
	if a.Retry.Attempts != 1 || a.Timeout != Duration(3*time.Minute) {
		t.Fatalf("defaults: %+v", a)
	}
}

func TestDurationRoundTrip(t *testing.T) {
	b, err := json.Marshal(Duration(90 * time.Minute))
	if err != nil || string(b) != `"1h30m0s"` {
		t.Fatalf("marshal = %s, %v", b, err)
	}
	var d Duration
	if err := json.Unmarshal([]byte(`"12h"`), &d); err != nil || d != Duration(12*time.Hour) {
		t.Fatalf("unmarshal = %v, %v", d, err)
	}
	if err := json.Unmarshal([]byte(`3600`), &d); err == nil {
		t.Fatal("bare number accepted")
	}
}

// task builds a config around one task body, replacing fields of a valid base.
func taskJSON(mod func(t map[string]any)) string {
	t := map[string]any{
		"script":   "a.js",
		"target":   map[string]any{"node": "n1"},
		"schedule": map[string]any{"every": "1h"},
		"timeout":  "3m",
		"retry":    map[string]any{"attempts": 2, "backoff": []string{"1m"}},
	}
	mod(t)
	b, _ := json.Marshal(map[string]any{"tasks": map[string]any{"a": t}})
	return string(b)
}

func TestParseConfigRejects(t *testing.T) {
	notify := func(msg string) map[string]any {
		return map[string]any{"title": "t", "message": msg}
	}
	cases := []struct {
		name string
		json string
		want []string
	}{
		{"node and tags", taskJSON(func(t map[string]any) {
			t["target"] = map[string]any{"node": "n1", "tags": []string{"tag:x"}}
		}), []string{"task a", "target"}},
		{"no target", taskJSON(func(t map[string]any) { t["target"] = map[string]any{} }), []string{"task a", "target"}},
		{"attempts zero", taskJSON(func(t map[string]any) {
			t["retry"] = map[string]any{"attempts": 0}
		}), []string{"task a", "retry.attempts"}},
		{"attempts too many", taskJSON(func(t map[string]any) {
			t["retry"] = map[string]any{"attempts": 11, "backoff": []string{"1m"}}
		}), []string{"task a", "retry.attempts"}},
		{"empty backoff", taskJSON(func(t map[string]any) {
			t["retry"] = map[string]any{"attempts": 3}
		}), []string{"task a", "retry.backoff"}},
		{"every too small", taskJSON(func(t map[string]any) {
			t["schedule"] = map[string]any{"every": "30s"}
		}), []string{"task a", "schedule.every"}},
		{"timeout zero", taskJSON(func(t map[string]any) { t["timeout"] = "0s" }), []string{"task a", "timeout"}},
		{"timeout negative", taskJSON(func(t map[string]any) { t["timeout"] = "-1m" }), []string{"task a", "timeout"}},
		{"timeout too long", taskJSON(func(t map[string]any) { t["timeout"] = "31m" }), []string{"task a", "timeout"}},
		{"unknown op", taskJSON(func(t map[string]any) {
			t["alert"] = map[string]any{"when": map[string]any{"field": "x", "op": "~", "value": 1}, "notify": notify("hi")}
		}), []string{"task a", "alert.when.op"}},
		{"secret placeholder", taskJSON(func(t map[string]any) {
			t["on_failure"] = map[string]any{"notify": notify("key {secret:TOKEN}")}
		}), []string{"task a", "on_failure.notify.message", "secret:TOKEN"}},
		{"script slash", taskJSON(func(t map[string]any) { t["script"] = "dir/a.js" }), []string{"task a", "script"}},
		{"script dotdot", taskJSON(func(t map[string]any) { t["script"] = ".." }), []string{"task a", "script"}},
		{"unknown field", `{"tasks":{},"extra":1}`, []string{"extra"}},
		{"unknown task field", taskJSON(func(t map[string]any) { t["bogus"] = 1 }), []string{"bogus"}},
		{"bare number duration", taskJSON(func(t map[string]any) { t["timeout"] = 180 }), []string{"duration"}},
		{"trailing data", specExample + `{}`, []string{"trailing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.json))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}

func TestParseConfigAllowsResultPlaceholders(t *testing.T) {
	_, err := ParseConfig([]byte(taskJSON(func(t map[string]any) {
		t["on_failure"] = map[string]any{"notify": map[string]any{"title": "t", "message": "{anything} and {reason}"}}
	})))
	if err != nil {
		t.Fatal(err)
	}
}

func TestParseConfigReportsAllProblems(t *testing.T) {
	_, err := ParseConfig([]byte(taskJSON(func(t map[string]any) {
		t["script"] = "../x.js"
		t["timeout"] = "0s"
		t["target"] = map[string]any{}
		t["schedule"] = map[string]any{"every": "1s"}
	})))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, w := range []string{"script", "timeout", "target", "schedule.every"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("error %q lacks %q", err, w)
		}
	}
}

func twoTasks(second string) string {
	return `{"tasks":{"a":{"script":"a.js","target":{"node":"n"},"schedule":{"every":"1h"}},"b":` + second + `}}`
}

func TestParseConfigDecodeErrorsNameTaskAndField(t *testing.T) {
	cases := []struct {
		name, json string
		want       []string
	}{
		{"bad duration in second task", twoTasks(`{"script":"b.js","target":{"node":"n"},"schedule":{"every":"12x"}}`),
			[]string{"task b", "schedule.every", "12x"}},
		{"bad backoff entry", twoTasks(`{"script":"b.js","target":{"node":"n"},"schedule":{"every":"1h"},"retry":{"attempts":2,"backoff":["1m","zz"]}}`),
			[]string{"task b", "retry.backoff[1]"}},
		{"unknown field in task", twoTasks(`{"script":"b.js","target":{"node":"n"},"schedule":{"every":"1h"},"bogus":1}`),
			[]string{"task b", "bogus"}},
		{"wrong type", twoTasks(`{"script":7,"target":{"node":"n"},"schedule":{"every":"1h"}}`),
			[]string{"task b", "script"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(tc.json))
			if err == nil {
				t.Fatal("expected error")
			}
			for _, w := range tc.want {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
			if strings.Contains(err.Error(), "task a") {
				t.Errorf("valid task a blamed: %q", err)
			}
		})
	}
}

func TestParseConfigTaskIDs(t *testing.T) {
	body := `{"script":"a.js","target":{"node":"n"},"schedule":{"every":"1h"}}`
	for _, id := range []string{"road-balance", "a.b_c-1", "0", strings.Repeat("a", 64)} {
		if _, err := ParseConfig([]byte(`{"tasks":{"` + id + `":` + body + `}}`)); err != nil {
			t.Errorf("id %q rejected: %v", id, err)
		}
	}
	for _, id := range []string{"Upper", "a/b", "has space", "", strings.Repeat("a", 65), "-lead", ".lead"} {
		_, err := ParseConfig([]byte(`{"tasks":{"` + id + `":` + body + `}}`))
		if err == nil || !strings.Contains(err.Error(), "id") {
			t.Errorf("id %q accepted or unnamed: %v", id, err)
		}
	}
}

func TestParseConfigRejectsDuplicateTaskID(t *testing.T) {
	body := `{"script":"a.js","target":{"node":"n"},"schedule":{"every":"1h"}}`
	_, err := ParseConfig([]byte(`{"tasks":{"a":` + body + `,"a":` + body + `}}`))
	if err == nil || !strings.Contains(err.Error(), "task a") || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("err = %v", err)
	}
}

func TestParseConfigMoreRejects(t *testing.T) {
	notify := map[string]any{"title": "t", "message": "m"}
	cases := []struct {
		name string
		mod  func(t map[string]any)
		want string
	}{
		{"zero alert cooldown", func(t map[string]any) {
			t["alert"] = map[string]any{"when": map[string]any{"field": "x", "op": "<", "value": 1}, "cooldown": "0s", "notify": notify}
		}, "alert.cooldown"},
		{"zero on_failure cooldown", func(t map[string]any) {
			t["on_failure"] = map[string]any{"cooldown": "0s", "notify": notify}
		}, "on_failure.cooldown"},
		{"alert field required", func(t map[string]any) {
			t["alert"] = map[string]any{"when": map[string]any{"op": "<", "value": 1}, "notify": notify}
		}, "alert.when.field"},
		{"negative jitter", func(t map[string]any) {
			t["schedule"] = map[string]any{"every": "1h", "jitter": "-1m"}
		}, "schedule.jitter"},
		{"zero backoff entry", func(t map[string]any) {
			t["retry"] = map[string]any{"attempts": 2, "backoff": []string{"0s"}}
		}, "retry.backoff"},
		{"empty tag", func(t map[string]any) {
			t["target"] = map[string]any{"tags": []string{" "}}
		}, "target.tags"},
		{"empty secret", func(t map[string]any) { t["secrets"] = []string{""} }, "secrets"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(taskJSON(tc.mod)))
			if err == nil || !strings.Contains(err.Error(), "task a") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestParseConfigAlertCooldownDefault(t *testing.T) {
	cfg, err := ParseConfig([]byte(taskJSON(func(t map[string]any) {
		t["alert"] = map[string]any{"when": map[string]any{"field": "x", "op": "<", "value": 1},
			"notify": map[string]any{"title": "t", "message": "m"}}
	})))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Tasks["a"].Alert.Cooldown; got != Duration(24*time.Hour) {
		t.Fatalf("cooldown = %v", got)
	}
}
