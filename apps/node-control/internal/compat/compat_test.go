package compat

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedMatrix(t *testing.T) {
	m := Must()
	for _, n := range []string{AgentAPI, FunnelAPI, RunnerAPI} {
		i, ok := m.Interfaces[n]
		if !ok {
			t.Fatalf("%s missing", n)
		}
		if i.Between[0] != "node-control" {
			t.Errorf("%s: node-control is the first part", n)
		}
		// every file the interface names exists, so the CI check that guards it is real
		for _, f := range i.Covers {
			if _, err := os.Stat(filepath.Join("..", "..", "..", "..", f)); err != nil {
				t.Errorf("%s covers %s: %v", n, f, err)
			}
		}
	}
}

func TestCheck(t *testing.T) {
	m, err := Parse([]byte(`{"interfaces":{"x":{"between":["node-control","agent"],"current":3,
		"accepted":{"min":2,"max":3},"covers":["f"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		v    int
		want State
	}{{0, Unknown}, {1, Incompatible}, {2, OK}, {3, OK}, {4, Incompatible}} {
		if got := m.Check("x", c.v); got.State != c.want {
			t.Errorf("%d: %s, want %s", c.v, got.State, c.want)
		}
	}
	if h := m.Check("x", 1).Hint; h != "older than node-control accepts: upgrade agent" {
		t.Errorf("hint %q", h)
	}
	if h := m.Check("x", 4).Hint; h != "newer than node-control accepts: upgrade node-control" {
		t.Errorf("hint %q", h)
	}
}

func TestParseRefusesBadFiles(t *testing.T) {
	for _, b := range []string{
		`{}`,
		`{"interfaces":{"x":{"between":["a"],"current":1,"accepted":{"min":1,"max":1},"covers":["f"]}}}`,
		`{"interfaces":{"x":{"between":["a","b"],"current":2,"accepted":{"min":1,"max":1},"covers":["f"]}}}`,
		`{"interfaces":{"x":{"between":["a","b"],"current":1,"accepted":{"min":2,"max":1},"covers":["f"]}}}`,
		`{"interfaces":{"x":{"between":["a","b"],"current":1,"accepted":{"min":1,"max":1},"covers":[]}}}`,
	} {
		if _, err := Parse([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}
