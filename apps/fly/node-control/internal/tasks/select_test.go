package tasks

import (
	"errors"
	"testing"

	"example.com/node-control/internal/headscale"
)

func node(name string, online bool, tags []string, ips ...string) headscale.Node {
	return headscale.Node{Name: name, Online: online, Tags: tags, IPAddresses: ips}
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

func TestSelectByNameMatchesGivenName(t *testing.T) {
	n := node("host-1234", true, nil, "100.64.0.1")
	n.GivenName = "runner"
	c, err := Select(Target{Node: "runner"}, []headscale.Node{n}, always, nil, "")
	if err != nil || c.Node.Name != "host-1234" {
		t.Fatalf("got %+v, %v", c, err)
	}
	if _, err := Select(Target{Node: "Runner"}, []headscale.Node{n}, always, nil, ""); !errors.Is(err, ErrNoEligibleNode) {
		t.Fatalf("match must be case-sensitive, got %v", err)
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
