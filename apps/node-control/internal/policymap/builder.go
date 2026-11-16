package policymap

import (
	"fmt"
	"strings"
)

// builder collects the nodes and edges of one graph within NodesMax, EdgesMax and WarningsMax.
type builder struct {
	hosts         map[string]string
	nodes         map[string]*Node
	used          map[string]map[string]bool // node id -> sections whose rules use it
	edges         map[string]*Edge           // edgeKey -> edge
	warnings      []string
	seenWarnings  map[string]bool
	extraWarnings int
	truncated     bool
}

func newBuilder(hosts map[string]string) *builder {
	return &builder{hosts: hosts, nodes: map[string]*Node{}, used: map[string]map[string]bool{},
		edges: map[string]*Edge{}, seenWarnings: map[string]bool{}}
}

func (b *builder) warn(msg string) {
	if b.seenWarnings[msg] {
		return
	}
	b.seenWarnings[msg] = true
	if len(b.warnings) >= WarningsMax {
		b.extraWarnings++
		return
	}
	b.warnings = append(b.warnings, msg)
}

func where(ref Ref) string {
	return fmt.Sprintf("%s[%d] (line %d)", ref.Section, ref.Index, ref.Line)
}

// node returns the node of id and makes it when it is new. It returns nil when the graph is full.
// ref is the rule that uses the selector, nil for a definition.
func (b *builder) node(id string, ref *Ref) *Node {
	if n := b.nodes[id]; n != nil {
		return n
	}
	if len(b.nodes) >= NodesMax {
		b.truncated = true
		return nil
	}
	kind := classify(id, b.hosts)
	if kind == KindUnknown && ref != nil {
		b.warn(fmt.Sprintf("%s: %q is not a known selector form", where(*ref), id))
	}
	n := &Node{ID: id, Kind: kind, Label: label(id, kind)}
	b.nodes[id] = n
	return n
}

func (b *builder) use(id, section string) {
	if b.used[id] == nil {
		b.used[id] = map[string]bool{}
	}
	b.used[id][section] = true
}

// addEdge adds e for the rule ref, or adds ref to an equal edge.
func (b *builder) addEdge(e Edge, ref Ref) {
	if b.node(e.From, &ref) == nil || b.node(e.To, &ref) == nil {
		return
	}
	b.use(e.From, ref.Section)
	b.use(e.To, ref.Section)
	key := edgeKey(e)
	if old := b.edges[key]; old != nil {
		if last := old.Refs[len(old.Refs)-1]; last != ref {
			old.Refs = append(old.Refs, ref)
		}
		return
	}
	if len(b.edges) >= EdgesMax {
		b.truncated = true
		return
	}
	e.Refs = []Ref{ref}
	b.edges[key] = &e
}

// edgeKey is the identity of an edge: everything but the rule references.
func edgeKey(e Edge) string {
	return strings.Join([]string{e.From, e.To, e.Proto, e.Ports, strings.Join(e.Via, ","),
		strings.Join(e.Posture, ","), strings.Join(e.Apps, ","), strings.Join(e.SSHUsers, ","),
		e.SSHAction}, "\x00")
}

// addDefinitions adds the groups, tags and hosts that the policy defines, used or not.
func (b *builder) addDefinitions(doc document) {
	for _, id := range sortedKeys(doc.groups) {
		if n := b.node(id, nil); n != nil {
			n.Members = doc.groups[id]
		}
	}
	for _, id := range sortedKeys(doc.tagOwners) {
		if n := b.node(id, nil); n != nil {
			n.Owners = doc.tagOwners[id]
		}
	}
	for _, id := range sortedKeys(doc.hosts) {
		if n := b.node(id, nil); n != nil {
			n.Address = doc.hosts[id]
		}
	}
}

func (b *builder) addACL(rule aclRule) {
	if rule.Action != "accept" {
		b.warn(fmt.Sprintf("%s: the action %q is not \"accept\"; the map ignores the rule",
			where(rule.ref), rule.Action))
		return
	}
	proto := protoString(rule.Proto)
	for _, dst := range rule.Dst {
		alias, ports, ok := splitDestination(dst)
		if !ok {
			b.warn(fmt.Sprintf("%s: the destination %q has no valid ports", where(rule.ref), dst))
		}
		for _, src := range rule.Src {
			e := Edge{From: src, To: alias, Proto: proto, Ports: ports, Posture: rule.SrcPosture}
			b.addEdge(e, rule.ref)
		}
	}
}

func (b *builder) addGrant(rule grantRule) {
	accesses := grantAccesses(rule)
	if len(accesses) == 0 {
		b.warn(fmt.Sprintf("%s: the grant has no \"ip\" and no \"app\"", where(rule.ref)))
		return
	}
	for _, dst := range rule.Dst {
		for _, src := range rule.Src {
			for _, a := range accesses {
				b.addEdge(Edge{From: src, To: dst, Proto: a.proto, Ports: a.ports, Apps: a.apps,
					Via: rule.Via, Posture: rule.SrcPosture}, rule.ref)
			}
		}
	}
}

func (b *builder) addSSH(rule sshRule) {
	if rule.Action != "accept" && rule.Action != "check" {
		b.warn(fmt.Sprintf(
			"%s: the SSH action %q is not \"accept\" or \"check\"; the map ignores the rule",
			where(rule.ref), rule.Action))
		return
	}
	for _, dst := range rule.Dst {
		for _, src := range rule.Src {
			b.addEdge(Edge{From: src, To: dst, Proto: "ssh", Ports: "22", SSHUsers: rule.Users,
				SSHAction: rule.Action}, rule.ref)
		}
	}
}

// addApprovers notes on each selector of autoApprovers what it approves.
func (b *builder) addApprovers(a approvers) {
	for _, id := range a.ExitNode {
		if n := b.node(id, nil); n != nil {
			n.Approves = append(n.Approves, "exit node")
		}
	}
	for _, prefix := range sortedKeys(a.Routes) {
		for _, id := range a.Routes[prefix] {
			if n := b.node(id, nil); n != nil {
				n.Approves = append(n.Approves, "route "+prefix)
			}
		}
	}
}

// graph returns the collected graph in a stable order.
func (b *builder) graph() Graph {
	g := Graph{Nodes: make([]Node, 0, len(b.nodes)), Edges: make([]Edge, 0, len(b.edges)),
		Warnings: b.warnings, Truncated: b.truncated}
	if g.Warnings == nil {
		g.Warnings = []string{}
	}
	if b.extraWarnings > 0 {
		g.Warnings = append(g.Warnings, fmt.Sprintf("and %d more warnings", b.extraWarnings))
	}
	for _, id := range sortedKeys(b.nodes) {
		n := *b.nodes[id]
		n.RuleTypes = make([]string, 0, len(b.used[id]))
		for _, section := range []string{SectionACL, SectionGrant, SectionSSH} {
			if b.used[id][section] {
				n.RuleTypes = append(n.RuleTypes, section)
			}
		}
		definition := n.Kind == KindGroup || n.Kind == KindTag || n.Kind == KindHost
		n.Unused = definition && len(n.RuleTypes) == 0 && len(n.Approves) == 0
		g.Nodes = append(g.Nodes, n)
	}
	for _, key := range sortedKeys(b.edges) {
		g.Edges = append(g.Edges, *b.edges[key])
	}
	return g
}
