package tasks

import (
	"os"
	"path/filepath"
	"testing"
)

// examplePath is the placeholder task file shipped for private repos. The
// documentation points at it, so it must keep parsing.
var examplePath = filepath.Join("..", "..", "..", "..", "..", "examples", "private-repo", "fly", "node-control", "tasks", "tasks.example.json")

func TestExampleConfigParses(t *testing.T) {
	b, err := os.ReadFile(examplePath)
	if err != nil {
		t.Fatalf("the example task config moved or is missing (%s): update examplePath and the links in apps/fly/node-control/README.md: %v", examplePath, err)
	}
	cfg, err := ParseConfig(b)
	if err != nil {
		t.Fatalf("example task config no longer validates: %v", err)
	}
	if len(cfg.Tasks) == 0 {
		t.Fatal("example task config has no tasks")
	}
	for id, task := range cfg.Tasks {
		script := filepath.Join(filepath.Dir(examplePath), "scripts", "check-balance.example.js")
		if task.Script != "check-balance.js" {
			t.Errorf("task %s: script %q, want check-balance.js", id, task.Script)
		}
		if _, err := os.Stat(script); err != nil {
			t.Errorf("example script missing next to the example config: %v", err)
		}
	}
}
