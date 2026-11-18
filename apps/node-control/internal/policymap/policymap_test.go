package policymap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

func buildFile(t *testing.T, name string) Graph {
	t.Helper()
	text, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	g, err := Build(string(text))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func findNode(t *testing.T, g Graph, id string) Node {
	t.Helper()
	for _, n := range g.Nodes {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("no node %q", id)
	return Node{}
}

func findEdges(g Graph, from, to string) []Edge {
	var out []Edge
	for _, e := range g.Edges {
		if e.From == from && e.To == to {
			out = append(out, e)
		}
	}
	return out
}

func oneEdge(t *testing.T, g Graph, from, to string) Edge {
	t.Helper()
	edges := findEdges(g, from, to)
	if len(edges) != 1 {
		t.Fatalf("%s -> %s: %d edges, want 1: %+v", from, to, len(edges), edges)
	}
	return edges[0]
}

func hasWarning(g Graph, part string) bool {
	for _, w := range g.Warnings {
		if strings.Contains(w, part) {
			return true
		}
	}
	return false
}

func TestNodeKinds(t *testing.T) {
	g := buildFile(t, "full.hujson")
	want := map[string]Kind{
		"group:admins": KindGroup, "tag:web": KindTag, "db": KindHost, "*": KindAny,
		"autogroup:internet": KindInternet, "autogroup:member": KindAutogroup,
		"198.51.100.0/24": KindCIDR, "nobody": KindUnknown,
	}
	for id, kind := range want {
		if n := findNode(t, g, id); n.Kind != kind {
			t.Errorf("%s: kind %q, want %q", id, n.Kind, kind)
		}
	}
	if n := findNode(t, g, "autogroup:internet"); n.Label != "internet (exit node)" {
		t.Errorf("internet label %q", n.Label)
	}
}

func TestDefinitions(t *testing.T) {
	g := buildFile(t, "full.hujson")
	staff := findNode(t, g, "group:staff")
	if want := []string{"bob@example.com", "group:admins"}; !reflect.DeepEqual(staff.Members, want) {
		t.Errorf("group in a group: members %v, want %v", staff.Members, want)
	}
	if web := findNode(t, g, "tag:web"); !reflect.DeepEqual(web.Owners, []string{"group:admins"}) {
		t.Errorf("tag owners %v", web.Owners)
	}
	if db := findNode(t, g, "db"); db.Address != "192.0.2.10/32" {
		t.Errorf("host address %q", db.Address)
	}
	for id, unused := range map[string]bool{"group:idle": true, "tag:spare": true, "group:admins": false,
		"tag:exit": false, "tag:router": false} {
		if n := findNode(t, g, id); n.Unused != unused {
			t.Errorf("%s: unused %v, want %v", id, n.Unused, unused)
		}
	}
	exit := findNode(t, g, "tag:exit")
	if !reflect.DeepEqual(exit.Approves, []string{"exit node"}) {
		t.Errorf("exit approves %v", exit.Approves)
	}
	if r := findNode(t, g, "tag:router"); !reflect.DeepEqual(r.Approves, []string{"route 192.0.2.0/24"}) {
		t.Errorf("router approves %v", r.Approves)
	}
}

func TestACLEdgesAndLines(t *testing.T) {
	g := buildFile(t, "full.hujson")
	e := oneEdge(t, g, "group:admins", "db")
	want := []Ref{{SectionACL, 0, 19}, {SectionACL, 5, 29}}
	if e.Ports != "5432" || !reflect.DeepEqual(e.Refs, want) {
		t.Errorf("admins -> db: %+v, want ports 5432 refs %v", e, want)
	}
	if e := oneEdge(t, g, "tag:web", "tag:web"); e.Ports != "80,443" {
		t.Errorf("web -> web ports %q", e.Ports)
	}
	internet := oneEdge(t, g, "group:staff", "autogroup:internet")
	if internet.Proto != "tcp" || internet.Ports != "*" || internet.Refs[0] != (Ref{SectionACL, 1, 20}) {
		t.Errorf("staff -> internet: %+v", internet)
	}
	if e := oneEdge(t, g, "group:staff", "198.51.100.0/24"); e.Ports != "22" {
		t.Errorf("staff -> cidr ports %q", e.Ports)
	}
	if len(findEdges(g, "*", "*")) != 0 {
		t.Error("the deny rule made an edge")
	}
}

func TestGrantEdges(t *testing.T) {
	g := buildFile(t, "full.hujson")
	edges := findEdges(g, "group:staff", "tag:web")
	got := map[string]Edge{}
	for _, e := range edges {
		got[e.Proto+"/"+e.Ports] = e
	}
	tcp, any := got["tcp/443"], got["/*"]
	if len(edges) != 2 || tcp.Refs == nil || any.Refs == nil {
		t.Fatalf("staff -> web: %+v", edges)
	}
	if !reflect.DeepEqual(tcp.Via, []string{"tag:router"}) || tcp.Refs[0] != (Ref{SectionGrant, 0, 32}) {
		t.Errorf("grant edge %+v", tcp)
	}
	app := oneEdge(t, g, "autogroup:member", "tag:web")
	if !reflect.DeepEqual(app.Apps, []string{"example.com/cap/web"}) || app.Ports != "" {
		t.Errorf("app edge %+v", app)
	}
	staff := findNode(t, g, "group:staff")
	if !reflect.DeepEqual(staff.RuleTypes, []string{SectionACL, SectionGrant}) {
		t.Errorf("staff rule types %v", staff.RuleTypes)
	}
}

func TestSSHEdges(t *testing.T) {
	g := buildFile(t, "full.hujson")
	var ssh *Edge
	for _, e := range findEdges(g, "group:admins", "tag:web") {
		if e.Proto == "ssh" {
			ssh = &e
		}
	}
	if ssh == nil || ssh.SSHAction != "check" || !reflect.DeepEqual(ssh.SSHUsers, []string{"root"}) ||
		ssh.Refs[0] != (Ref{SectionSSH, 0, 37}) {
		t.Errorf("ssh edge %+v", ssh)
	}
	admins := findNode(t, g, "group:admins")
	if !reflect.DeepEqual(admins.RuleTypes, []string{SectionACL, SectionSSH}) {
		t.Errorf("admins rule types %v", admins.RuleTypes)
	}
}

func TestWarnings(t *testing.T) {
	g := buildFile(t, "full.hujson")
	for _, part := range []string{
		`acls[2] (line 26): the destination "tag:web:bad" has no valid ports`,
		`acls[3] (line 27): "nobody" is not a known selector form`,
		`acls[4] (line 28): the action "deny"`,
		`grants[2] (line 34): the grant has no "ip" and no "app"`,
		`the key "futureKey" is not known`,
	} {
		if !hasWarning(g, part) {
			t.Errorf("no warning %q in %q", part, g.Warnings)
		}
	}
	if g.Truncated {
		t.Error("truncated")
	}
}

func TestBadSectionsAreWarningsNotErrors(t *testing.T) {
	g, err := Build(`{"acls": {"x": 1}, "groups": [1], "grants": [7, {"src": ["a@"], "dst": ["b@"], "ip": ["*"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range []string{`"acls" is not a list`, `cannot read "groups"`, `grants[0] (line 1): cannot read`} {
		if !hasWarning(g, part) {
			t.Errorf("no warning %q in %q", part, g.Warnings)
		}
	}
	if len(g.Edges) != 1 {
		t.Errorf("edges %+v", g.Edges)
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, text := range []string{`{`, `[1]`, `"x"`, `{"acls": [}`, `secret-text {`} {
		_, err := Build(text)
		if !errors.Is(err, ErrSyntax) {
			t.Errorf("%q: err %v", text, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: the error has policy text", text)
		}
	}
}

func TestBlankIsEmptyGraphWithJSONLists(t *testing.T) {
	for _, text := range []string{"", "  \n", "{}", "// only a comment\n{}"} {
		g, err := Build(text)
		if err != nil {
			t.Fatalf("%q: %v", text, err)
		}
		raw, err := json.Marshal(g)
		if err != nil {
			t.Fatal(err)
		}
		if want := `{"nodes":[],"edges":[],"warnings":[],"truncated":false}`; string(raw) != want {
			t.Errorf("%q: %s, want %s", text, raw, want)
		}
	}
}

func TestStableOrder(t *testing.T) {
	a, b := buildFile(t, "full.hujson"), buildFile(t, "full.hujson")
	if !reflect.DeepEqual(a, b) {
		t.Error("two builds of the same text differ")
	}
	for i := 1; i < len(a.Nodes); i++ {
		if a.Nodes[i-1].ID >= a.Nodes[i].ID {
			t.Fatalf("nodes not sorted at %d", i)
		}
	}
}

func TestEdgeCap(t *testing.T) {
	var dst []string
	for i := range EdgesMax + 10 {
		dst = append(dst, fmt.Sprintf(`"192.0.2.1:%d"`, i+1))
	}
	g, err := Build(`{"acls": [{"action": "accept", "src": ["a@"], "dst": [` + strings.Join(dst, ",") + `]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Truncated || len(g.Edges) != EdgesMax {
		t.Errorf("truncated %v, %d edges", g.Truncated, len(g.Edges))
	}
}

func TestNodeCap(t *testing.T) {
	var src []string
	for i := range NodesMax + 10 {
		src = append(src, fmt.Sprintf(`"u%d@"`, i))
	}
	g, err := Build(`{"acls": [{"action": "accept", "src": [` + strings.Join(src, ",") + `], "dst": ["*:*"]}]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !g.Truncated || len(g.Nodes) != NodesMax {
		t.Errorf("truncated %v, %d nodes", g.Truncated, len(g.Nodes))
	}
}

func TestWarningCap(t *testing.T) {
	var rules []string
	for i := range WarningsMax + 5 {
		rules = append(rules, fmt.Sprintf(`{"action": "accept", "src": ["x%d"], "dst": ["*:*"]}`, i))
	}
	g, err := Build(`{"acls": [` + strings.Join(rules, ",") + `]}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Warnings) != WarningsMax+1 || g.Warnings[WarningsMax] != "and 5 more warnings" {
		t.Errorf("%d warnings, last %q", len(g.Warnings), g.Warnings[len(g.Warnings)-1])
	}
}

func TestSplitDestination(t *testing.T) {
	cases := []struct {
		in, alias, ports string
		ok               bool
	}{
		{"tag:web:80,443", "tag:web", "80,443", true},
		{"*:*", "*", "*", true},
		{"autogroup:internet:*", "autogroup:internet", "*", true},
		{"[fd7a::1]:22", "fd7a::1", "22", true},
		{"192.0.2.0/24:1000-2000", "192.0.2.0/24", "1000-2000", true},
		{"tag:web", "tag:web", "", false},
		{"db:70000", "db:70000", "", false},
		{"db:", "db:", "", false},
		{"db:08", "db:08", "", false},
		{"[fd7a::1]", "[fd7a::1]", "", false},
	}
	for _, c := range cases {
		alias, ports, ok := splitDestination(c.in)
		if alias != c.alias || ports != c.ports || ok != c.ok {
			t.Errorf("%q: %q %q %v, want %q %q %v", c.in, alias, ports, ok, c.alias, c.ports, c.ok)
		}
	}
}

func TestProtoAndIPAccess(t *testing.T) {
	if got := protoString(json.RawMessage(`6`)); got != "6" {
		t.Errorf("number proto %q", got)
	}
	if got := protoString(json.RawMessage(`"udp"`)); got != "udp" {
		t.Errorf("name proto %q", got)
	}
	cases := map[string]access{"*": {ports: "*"}, "tcp:443": {proto: "tcp", ports: "443"},
		"53": {ports: "53"}, "icmp": {proto: "icmp"}}
	for in, want := range cases {
		if got := ipAccess(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
}
