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
	visits        int // combinations looked at, up to PairsMax
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

// addEdge adds e for the rule ref, or adds ref to an equal edge. It returns false when the work
// budget (PairsMax) is used up: the caller then stops its loops.
func (b *builder) addEdge(e Edge, ref Ref) bool {
	if b.visits >= PairsMax {
		b.truncated = true
		return false
	}
	b.visits++
	if b.node(e.From, &ref) == nil || b.node(e.To, &ref) == nil {
		return true
	}
	b.use(e.From, ref.Section)
	b.use(e.To, ref.Section)
	key := edgeKey(e)
	if old := b.edges[key]; old != nil {
		b.addRef(old, ref)
		return true
	}
	if len(b.edges) >= EdgesMax {
		b.truncated = true
		return true
	}
	e.Refs = []Ref{ref}
	b.edges[key] = &e
	return true
}

// addRef adds ref to e unless it is already the last one, within RefsMax.
func (b *builder) addRef(e *Edge, ref Ref) {
	if e.Refs[len(e.Refs)-1] == ref {
		return
	}
	if len(e.Refs) >= RefsMax {
		b.truncated = true
		return
	}
	e.Refs = append(e.Refs, ref)
}

// distinct returns list without repeated entries, in the order of their first use.
func distinct(list []string) []string {
	seen := make(map[string]bool, len(list))
	out := make([]string, 0, len(list))
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// edgeKey is the identity of an edge: everything but the rule references. The two separators
// cannot occur in a selector, so different edges never get the same key.
func edgeKey(e Edge) string {
	return strings.Join([]string{e.From, e.To, e.Proto, e.Ports, strings.Join(e.Via, "\x01"),
		strings.Join(e.Posture, "\x01"), strings.Join(e.Apps, "\x01"),
		strings.Join(e.SSHUsers, "\x01"), e.SSHAction}, "\x00")
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
	sources := distinct(rule.Src)
	for _, dst := range distinct(rule.Dst) {
		alias, ports, ok := splitDestination(dst)
		if !ok {
			b.warn(fmt.Sprintf("%s: the destination %q has no valid ports", where(rule.ref), dst))
		}
		for _, src := range sources {
			e := Edge{From: src, To: alias, Proto: proto, Ports: ports, Posture: rule.SrcPosture}
			if !b.addEdge(e, rule.ref) {
				return
			}
		}
	}
}

func (b *builder) addGrant(rule grantRule) {
	accesses := grantAccesses(rule)
	if len(accesses) == 0 {
		b.warn(fmt.Sprintf("%s: the grant has no \"ip\" and no \"app\"", where(rule.ref)))
		return
	}
	sources := distinct(rule.Src)
	for _, dst := range distinct(rule.Dst) {
		for _, src := range sources {
			for _, a := range accesses {
				e := Edge{From: src, To: dst, Proto: a.proto, Ports: a.ports, Apps: a.apps,
					Via: rule.Via, Posture: rule.SrcPosture}
				if !b.addEdge(e, rule.ref) {
					return
				}
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
	sources := distinct(rule.Src)
	for _, dst := range distinct(rule.Dst) {
		for _, src := range sources {
			e := Edge{From: src, To: dst, Proto: "ssh", Ports: "22", SSHUsers: rule.Users,
				SSHAction: rule.Action}
			if !b.addEdge(e, rule.ref) {
				return
			}
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
	referenced := b.referenced()
	for _, id := range sortedKeys(b.nodes) {
		n := *b.nodes[id]
		n.RuleTypes = make([]string, 0, len(b.used[id]))
		for _, section := range []string{SectionACL, SectionGrant, SectionSSH} {
			if b.used[id][section] {
				n.RuleTypes = append(n.RuleTypes, section)
			}
		}
		definition := n.Kind == KindGroup || n.Kind == KindTag || n.Kind == KindHost
		n.Unused = definition && len(n.RuleTypes) == 0 && len(n.Approves) == 0 && !referenced[id]
		g.Nodes = append(g.Nodes, n)
	}
	for _, key := range sortedKeys(b.edges) {
		g.Edges = append(g.Edges, *b.edges[key])
	}
	return g
}

// referenced returns the selectors that a definition names: the owners of a tag and the members
// of a group. Such a group, tag or host is in use even when no rule names it.
func (b *builder) referenced() map[string]bool {
	out := map[string]bool{}
	for _, n := range b.nodes {
		for _, id := range n.Owners {
			out[id] = true
		}
		for _, id := range n.Members {
			out[id] = true
		}
	}
	return out
}
