package policymap

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

const buildTimeMax = 2 * time.Second

func repeated(item string, count int) string {
	return strings.TrimSuffix(strings.Repeat(item+",", count), ",")
}

func timedBuild(t *testing.T, text string) Graph {
	t.Helper()
	start := time.Now()
	g, err := Build(text)
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > buildTimeMax {
		t.Errorf("Build took %v, more than %v", took, buildTimeMax)
	}
	return g
}

func TestRepeatedSelectorsAreCheap(t *testing.T) {
	g := timedBuild(t, `{"acls": [{"action": "accept", "src": [`+repeated(`"*"`, 16000)+
		`], "dst": [`+repeated(`"*:*"`, 16000)+`]}]}`)
	if len(g.Edges) != 1 || g.Truncated {
		t.Errorf("%d edges, truncated %v", len(g.Edges), g.Truncated)
	}
}

func TestPairBudgetStopsWork(t *testing.T) {
	var src, dst []string
	for i := range 9000 {
		src = append(src, fmt.Sprintf(`"u%d@"`, i))
		dst = append(dst, fmt.Sprintf(`"192.0.2.1:%d"`, i+1))
	}
	g := timedBuild(t, `{"acls": [{"action": "accept", "src": [`+strings.Join(src, ",")+
		`], "dst": [`+strings.Join(dst, ",")+`]}]}`)
	if !g.Truncated {
		t.Error("not truncated")
	}
}

func TestGrantPairBudget(t *testing.T) {
	var ip []string
	for i := range PairsMax + 10 {
		ip = append(ip, fmt.Sprintf(`"tcp:%d"`, i%65536))
	}
	g := timedBuild(t, `{"grants": [{"src": ["a@"], "dst": ["b@"], "ip": [`+strings.Join(ip, ",")+`]}]}`)
	if len(g.Edges) > EdgesMax || !g.Truncated {
		t.Errorf("%d edges, truncated %v", len(g.Edges), g.Truncated)
	}
}

func TestRefsCap(t *testing.T) {
	rule := `{"action": "accept", "src": ["a@"], "dst": ["b@:22"]}`
	g := timedBuild(t, `{"acls": [`+repeated(rule, RefsMax+5)+`]}`)
	if len(g.Edges) != 1 || len(g.Edges[0].Refs) != RefsMax || !g.Truncated {
		t.Errorf("%d edges, %d refs, truncated %v", len(g.Edges), len(g.Edges[0].Refs), g.Truncated)
	}
}

func TestEdgeMergesAcrossSections(t *testing.T) {
	g := mustBuild(t, `{"acls": [{"action": "accept", "src": ["a@"], "dst": ["b@:*"]}],
		"grants": [{"src": ["a@"], "dst": ["b@"], "ip": ["*"]}]}`)
	if len(g.Edges) != 1 {
		t.Fatalf("edges %+v", g.Edges)
	}
	want := []Ref{{SectionACL, 0, 1}, {SectionGrant, 0, 2}}
	if got := g.Edges[0].Refs; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("refs %v, want %v", got, want)
	}
	if a := findNode(t, g, "a@"); fmt.Sprint(a.RuleTypes) != "[acls grants]" {
		t.Errorf("rule types %v", a.RuleTypes)
	}
}

func TestOwnerAndMemberAreInUse(t *testing.T) {
	g := mustBuild(t, `{"groups": {"group:owners": ["a@"], "group:outer": ["group:inner"],
		"group:inner": ["b@"]}, "tagOwners": {"tag:x": ["group:owners"]}}`)
	for id, unused := range map[string]bool{"group:owners": false, "group:inner": false,
		"group:outer": true, "tag:x": true} {
		if n := findNode(t, g, id); n.Unused != unused {
			t.Errorf("%s: unused %v, want %v", id, n.Unused, unused)
		}
	}
}

func TestStrictDestinations(t *testing.T) {
	for _, dst := range []string{"fd7a::1", "192.0.2.1:2000-1000"} {
		if _, _, ok := splitDestination(dst); ok {
			t.Errorf("%q accepted", dst)
		}
	}
	if _, _, ok := splitDestination("192.0.2.1:1000-2000"); !ok {
		t.Error("a valid range refused")
	}
}

func TestEdgeKeyKeepsListItemsApart(t *testing.T) {
	one := Edge{From: "a", To: "b", Via: []string{"x,y"}}
	two := Edge{From: "a", To: "b", Via: []string{"x", "y"}}
	if edgeKey(one) == edgeKey(two) {
		t.Error("two different edges have the same key")
	}
}
