package policymap

import (
	"errors"
	"os"
	"testing"
)

// FuzzBuild checks that any text gives a graph within the limits or ErrSyntax, and never a panic
// (ADR 0002, rule 9).
func FuzzBuild(f *testing.F) {
	full, err := os.ReadFile("testdata/full.hujson")
	if err != nil {
		f.Fatal(err)
	}
	seeds := []string{string(full), "", "{}", `{"acls": [1]}`, `{"grants": [{"ip": ["::"]}]}`,
		`{"acls": [{"action": "accept", "src": ["*"], "dst": ["[::1]:*"]}]}`,
		`{"acls": [{"action": "accept", "src": ["*", "*", "a@"], "dst": ["*:*", "*:*", "fd7a::1"]}]}`}
	for _, seed := range seeds {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		g, err := Build(text)
		if err != nil {
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("error %v is not ErrSyntax", err)
			}
			return
		}
		if len(g.Nodes) > NodesMax || len(g.Edges) > EdgesMax || len(g.Warnings) > WarningsMax+1 {
			t.Fatalf("limits: %d nodes, %d edges, %d warnings", len(g.Nodes), len(g.Edges), len(g.Warnings))
		}
		for _, e := range g.Edges {
			if len(e.Refs) == 0 || len(e.Refs) > RefsMax || e.Refs[0].Line < 1 {
				t.Fatalf("edge without a valid ref: %+v", e)
			}
		}
	})
}
