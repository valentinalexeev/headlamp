package policymap

import (
	"fmt"
	"reflect"
	"strings"
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

func TestDiffNewGroupMemberGivesNewPaths(t *testing.T) {
	before := mustBuild(t, `{"groups": {"group:a": ["alice@"]},
		"acls": [{"action": "accept", "src": ["group:a"], "dst": ["tag:web:443", "tag:db:5432"]}]}`)
	after := mustBuild(t, `{"groups": {"group:a": ["alice@", "bob@"]},
		"acls": [{"action": "accept", "src": ["group:a"], "dst": ["tag:web:443", "tag:db:5432"]}]}`)
	d := Diff(before, after)
	if len(d.Added)+len(d.Removed) != 0 {
		t.Errorf("edges changed: %+v", d)
	}
	if want := []Member{{"group:a", "bob@"}}; !reflect.DeepEqual(d.MembersAdded, want) {
		t.Errorf("members added %v, want %v", d.MembersAdded, want)
	}
	if d.NewPaths != 2 || d.RemovedPaths != 0 || len(d.MembersRemoved) != 0 {
		t.Errorf("new %d removed %d members removed %v", d.NewPaths, d.RemovedPaths, d.MembersRemoved)
	}
	back := Diff(after, before)
	if back.RemovedPaths != 2 || len(back.MembersRemoved) != 1 {
		t.Errorf("reverse: removed %d members %v", back.RemovedPaths, back.MembersRemoved)
	}
}

func TestDiffInternetRuleGivesNewPathsAndRemovedNodes(t *testing.T) {
	before := mustBuild(t, `{"groups": {"group:a": ["alice@", "bob@"]},
		"acls": [{"action": "accept", "src": ["carol@"], "dst": ["tag:old:22"]}]}`)
	after := mustBuild(t, `{"groups": {"group:a": ["alice@", "bob@"]},
		"acls": [{"action": "accept", "src": ["group:a"], "dst": ["autogroup:internet:*"]}]}`)
	d := Diff(before, after)
	if len(d.Added) != 1 || d.Added[0].To != "autogroup:internet" || len(d.Removed) != 1 {
		t.Fatalf("added %+v removed %+v", d.Added, d.Removed)
	}
	if d.NewPaths != 2 || d.RemovedPaths != 1 {
		t.Errorf("new %d removed %d", d.NewPaths, d.RemovedPaths)
	}
	var ids []string
	for _, n := range d.RemovedNodes {
		ids = append(ids, n.ID)
	}
	if want := []string{"carol@", "tag:old"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("removed nodes %v, want %v", ids, want)
	}
}

func TestDiffPathsExpandNestedAndCyclicGroups(t *testing.T) {
	g := mustBuild(t, `{"groups": {"group:a": ["group:b", "x@"], "group:b": ["group:a", "y@"]},
		"acls": [{"action": "accept", "src": ["group:a"], "dst": ["group:b:22"]}]}`)
	p, cut := paths(g)
	if cut || len(p) == 0 {
		t.Fatalf("paths %d cut %v", len(p), cut)
	}
	if d := Diff(Graph{}, g); d.NewPaths != len(p) || d.PathsTruncated {
		t.Errorf("new paths %d, want %d", d.NewPaths, len(p))
	}
}

func TestDiffPathsStopAtTheCap(t *testing.T) {
	members := make([]string, 0, 400)
	for i := range 400 {
		members = append(members, fmt.Sprintf("%q", fmt.Sprintf("u%d@", i)))
	}
	list := strings.Join(members, ",")
	g := mustBuild(t, `{"groups": {"group:a": [`+list+`], "group:b": [`+list+`]},
		"acls": [{"action": "accept", "src": ["group:a"], "dst": ["group:b:*"]}]}`)
	p, cut := paths(g)
	if !cut || len(p) > PathsMax {
		t.Errorf("paths %d cut %v", len(p), cut)
	}
	if d := Diff(Graph{}, g); !d.PathsTruncated {
		t.Error("PathsTruncated is false")
	}
}
