package policymap

import (
	"reflect"
	"testing"
)

func mustBuild(t *testing.T, text string) Graph {
	t.Helper()
	g, err := Build(text)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestDiffAddedRemovedChanged(t *testing.T) {
	before := mustBuild(t, `{"acls": [
		{"action": "accept", "src": ["group:a"], "dst": ["tag:web:443"]},
		{"action": "accept", "src": ["group:b"], "dst": ["tag:db:5432"]}]}`)
	after := mustBuild(t, `{"acls": [
		{"action": "accept", "src": ["group:a"], "dst": ["tag:web:80,443"]},
		{"action": "accept", "src": ["group:c"], "dst": ["autogroup:internet:*"]}]}`)
	d := Diff(before, after)
	if len(d.Added) != 2 || len(d.Removed) != 2 {
		t.Fatalf("added %+v removed %+v", d.Added, d.Removed)
	}
	if want := []Pair{{"group:a", "tag:web"}}; !reflect.DeepEqual(d.Changed, want) {
		t.Errorf("changed %v, want %v", d.Changed, want)
	}
	for _, e := range d.Added {
		if e.From == "group:c" && e.Refs[0] != (Ref{SectionACL, 1, 3}) {
			t.Errorf("added edge refs %v, want the ref of the new graph", e.Refs)
		}
	}
}

func TestDiffIgnoresMovedRules(t *testing.T) {
	before := mustBuild(t, `{"acls": [{"action": "accept", "src": ["a@"], "dst": ["b@:22"]}]}`)
	after := mustBuild(t, "// moved\n\n{\"acls\": [\n{\"action\": \"accept\", \"src\": [\"a@\"], \"dst\": [\"b@:22\"]}]}")
	d := Diff(before, after)
	if len(d.Added)+len(d.Removed)+len(d.Changed) != 0 {
		t.Errorf("diff of a moved rule: %+v", d)
	}
}

func TestDiffOfEmptyGraphsHasLists(t *testing.T) {
	d := Diff(Graph{}, Graph{})
	if d.Added == nil || d.Removed == nil || d.Changed == nil {
		t.Errorf("nil lists: %+v", d)
	}
}
