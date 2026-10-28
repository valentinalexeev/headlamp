package tasks

import (
	"errors"
	"strings"
	"testing"

	"example.com/node-control/internal/headscale"
)

func node(name string, online bool, tags []string, ips ...string) headscale.Node {
	return headscale.Node{Name: name, GivenName: name, Online: online, Tags: tags, IPAddresses: ips}
}

func always(headscale.Node) bool { return true }

func TestSelectByName(t *testing.T) {
	nodes := []headscale.Node{
		node("a", true, nil, "100.64.0.1"),
		node("b", true, nil, "fd7a::2", "100.64.0.2"),
	}
	c, err := Select(Target{Node: "b"}, nodes, always, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Node.Name != "b" || c.Addr != "100.64.0.2" {
		t.Fatalf("got %s %s, want b 100.64.0.2 (IPv4 only)", c.Node.Name, c.Addr)
	}
}

func TestSelectByNameMatchesGivenNameOnly(t *testing.T) {
	n := node("host-1234", true, nil, "100.64.0.1")
	n.GivenName = "runner"
	c, err := Select(Target{Node: "runner"}, []headscale.Node{n}, always, nil, "")
	if err != nil || c.Node.Name != "host-1234" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := Select(Target{Node: "Runner"}, []headscale.Node{n}, always, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("match must be case-sensitive, got %v", err)
	}
	// The reported host name is chosen by the node itself: never a match.
	if _, err := Select(Target{Node: "host-1234"}, []headscale.Node{n}, always, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("reported name must not match, got %v", err)
	}
}

func TestSelectImpostorWithSameReportedNameNeverSelectedOrProbed(t *testing.T) {
	real := node("runner-1", true, nil, "100.64.0.1")
	impostor := node("runner-1", true, nil, "100.64.0.66") // same reported name
	impostor.GivenName = "laptop"
	var asked []string
	healthy := func(n headscale.Node) bool { asked = append(asked, n.GivenName); return true }
	for _, order := range [][]headscale.Node{{real, impostor}, {impostor, real}} {
		asked = nil
		c, err := Select(Target{Node: "runner-1"}, order, healthy, nil, "")
		if err != nil || c.Addr != "100.64.0.1" {
			t.Fatalf("got %+v, %v", c, err)
		}
		if len(asked) != 1 || asked[0] != "runner-1" {
			t.Fatalf("probed %v, want only the real node", asked)
		}
	}
}

func TestSelectAmbiguousGivenName(t *testing.T) {
	a := node("a", true, nil, "100.64.0.1")
	b := node("b", false, nil, "100.64.0.2")
	a.GivenName, b.GivenName = "dup", "dup"
	probed := 0
	healthy := func(headscale.Node) bool { probed++; return true }
	_, err := Select(Target{Node: "dup"}, []headscale.Node{a, b}, healthy, nil, "")
	if !errors.Is(err, ErrAmbiguousTarget) || !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("got %v, want ambiguous (wrapping no eligible node)", err)
	}
	if !strings.Contains(err.Error(), "ambiguous target") {
		t.Fatalf("message %q", err)
	}
	if probed != 0 {
		t.Fatalf("an ambiguous target probed %d nodes", probed)
	}
}

func TestWithTag(t *testing.T) {
	nodes := []headscale.Node{
		node("tagged", true, []string{"tag:x", DefaultRunnerTag}, "100.64.0.1"),
		node("plain", true, nil, "100.64.0.2"),
		node("other", true, []string{"tag:x"}, "100.64.0.3"),
	}
	got := WithTag(nodes, DefaultRunnerTag)
	if len(got) != 1 || got[0].Name != "tagged" {
		t.Fatalf("got %+v", got)
	}
	if got := WithTag(nodes, ""); len(got) != 0 {
		t.Fatalf("empty tag must keep nothing, got %+v", got)
	}
}

func TestSelectByTagsRequiresAllTags(t *testing.T) {
	nodes := []headscale.Node{
		node("only-region", true, []string{"tag:region-xx"}, "100.64.0.1"),
		node("both", true, []string{"tag:region-xx", "tag:runner"}, "100.64.0.2"),
	}
	c, err := Select(Target{Tags: []string{"tag:region-xx", "tag:runner"}}, nodes, always, nil, "")
	if err != nil || c.Node.Name != "both" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestSelectSkipsOfflineWhenOnlineRequired(t *testing.T) {
	nodes := []headscale.Node{
		node("a", false, nil, "100.64.0.1"),
		node("b", true, nil, "100.64.0.2"),
	}
	tags := []string{"tag:t"}
	for i := range nodes {
		nodes[i].Tags = tags
	}
	c, err := Select(Target{Tags: tags, Online: true}, nodes, always, nil, "")
	if err != nil || c.Node.Name != "b" {
		t.Fatalf("got %+v, %v", c, err)
	}
	c, err = Select(Target{Tags: tags}, nodes, always, nil, "")
	if err != nil || c.Node.Name != "a" {
		t.Fatalf("offline node must be allowed when Online is false, got %+v, %v", c, err)
	}
}

func TestSelectSkipsUnhealthy(t *testing.T) {
	nodes := []headscale.Node{
		node("a", true, []string{"tag:t"}, "100.64.0.1"),
		node("b", true, []string{"tag:t"}, "100.64.0.2"),
	}
	calls := map[string]int{}
	healthy := func(n headscale.Node) bool { calls[n.Name]++; return n.Name == "b" }
	c, err := Select(Target{Tags: []string{"tag:t"}}, nodes, healthy, nil, "")
	if err != nil || c.Node.Name != "b" {
		t.Fatalf("got %+v, %v", c, err)
	}
	for n, k := range calls {
		if k > 1 {
			t.Errorf("healthy called %d times for %s", k, n)
		}
	}
}

func TestSelectSkipsNodesWithoutIPv4AndNonMatchingBeforeHealth(t *testing.T) {
	nodes := []headscale.Node{
		node("v6", true, []string{"tag:t"}, "fd7a::1", "not-an-ip"),
		node("other", true, []string{"tag:x"}, "100.64.0.9"),
		node("ok", true, []string{"tag:t"}, "100.64.0.1"),
	}
	var asked []string
	healthy := func(n headscale.Node) bool { asked = append(asked, n.Name); return true }
	c, err := Select(Target{Tags: []string{"tag:t"}}, nodes, healthy, nil, "")
	if err != nil || c.Node.Name != "ok" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if len(asked) != 1 || asked[0] != "ok" {
		t.Fatalf("health consulted for %v, want only [ok]", asked)
	}
}

func TestSelectPrefersLastSuccessful(t *testing.T) {
	nodes := []headscale.Node{
		node("a", true, []string{"tag:t"}, "100.64.0.1"),
		node("b", true, []string{"tag:t"}, "100.64.0.2"),
	}
	busy := map[string]int{"b": 5}
	c, err := Select(Target{Tags: []string{"tag:t"}}, nodes, always, busy, "b")
	if err != nil || c.Node.Name != "b" {
		t.Fatalf("lastOK must win over load, got %+v, %v", c, err)
	}
	// A lastOK that is not eligible is ignored.
	c, err = Select(Target{Tags: []string{"tag:t"}}, nodes, always, busy, "gone")
	if err != nil || c.Node.Name != "a" {
		t.Fatalf("got %+v, %v", c, err)
	}
}

func TestSelectLeastBusyThenName(t *testing.T) {
	mk := func(names ...string) []headscale.Node {
		var out []headscale.Node
		for i, n := range names {
			out = append(out, node(n, true, []string{"tag:t"}, "100.64.0."+string(rune('1'+i))))
		}
		return out
	}
	tg := Target{Tags: []string{"tag:t"}}
	busy := map[string]int{"a": 2, "b": 1, "c": 1}
	for _, order := range [][]string{{"a", "b", "c"}, {"c", "b", "a"}, {"b", "a", "c"}} {
		c, err := Select(tg, mk(order...), always, busy, "")
		if err != nil || c.Node.Name != "b" {
			t.Fatalf("order %v: got %+v, %v, want b (least busy, then name)", order, c, err)
		}
	}
}

func TestSelectNoneReturnsErrNoEligibleNode(t *testing.T) {
	nodes := []headscale.Node{node("a", true, []string{"tag:t"}, "100.64.0.1")}
	if _, err := Select(Target{Node: "missing"}, nodes, always, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Errorf("no match: got %v", err)
	}
	never := func(headscale.Node) bool { return false }
	if _, err := Select(Target{Tags: []string{"tag:t"}}, nodes, never, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Errorf("all unhealthy: got %v", err)
	}
	if _, err := Select(Target{Tags: []string{"tag:t"}}, nil, always, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Errorf("empty list: got %v", err)
	}
}
