// Package policymap turns a headscale policy (JSON or HuJSON) into a reachability graph: one node
// for each selector (group, tag, user, host, CIDR, autogroup) and one edge for each "source can
// reach destination" pair of an ACL, grant or SSH rule. Each edge keeps the rule references and
// their line numbers in the policy text. The policy console serves the graph at
// GET /policy/api/map (#428, ADR 0005).
//
// The idea of a policy map comes from tailscale-network-topology-mapper by SimplyMinimal
// (https://github.com/SimplyMinimal/tailscale-network-topology-mapper, Apache-2.0): a graph of
// the policy, nodes by kind, ports and protocol on the edges, and rule line numbers in the
// tooltip. This package copies no code from that project.
//
// headscale validates the policy. This package is not a second validator: input that it cannot read
// becomes a warning, never an error. Only text that is not JSON or HuJSON, or a root that is not an
// object, is an error. The package never logs.
package policymap

import (
	"errors"
	"sort"
)

// Limits of one graph. Above them the graph is cut and Truncated is set. PairsMax limits the
// work: the number of source, destination and access combinations that one build looks at.
// RefsMax limits the rule references of one edge, and with EdgesMax the size of the answer.
const (
	NodesMax    = 1500
	EdgesMax    = 3000
	RefsMax     = 20
	PairsMax    = 100000
	WarningsMax = 100
)

// ErrSyntax is the error for text that is not JSON or HuJSON, or whose root is not an object. It
// never carries policy text.
var ErrSyntax = errors.New("the policy is not a JSON or HuJSON object")

// Kind is the kind of a graph node.
type Kind string

// The node kinds. KindInternet is autogroup:internet: the internet through an exit node.
const (
	KindGroup     Kind = "group"
	KindTag       Kind = "tag"
	KindUser      Kind = "user"
	KindHost      Kind = "host"
	KindCIDR      Kind = "cidr"
	KindAutogroup Kind = "autogroup"
	KindInternet  Kind = "internet"
	KindAny       Kind = "any"
	KindUnknown   Kind = "unknown"
)

// The rule sections, also the values of Node.RuleTypes.
const (
	SectionACL   = "acls"
	SectionGrant = "grants"
	SectionSSH   = "ssh"
)

// Ref points to one rule: its section, its index in the section (from 0) and the line of the policy
// text where the rule starts (from 1).
type Ref struct {
	Section string `json:"section"`
	Index   int    `json:"index"`
	Line    int    `json:"line"`
}

// Node is one selector of the policy.
type Node struct {
	ID    string `json:"id"`
	Kind  Kind   `json:"kind"`
	Label string `json:"label"`
	// RuleTypes are the sections whose rules use the node, in the order acls, grants, ssh.
	RuleTypes []string `json:"rule_types"`
	Members   []string `json:"members,omitempty"`  // a group: its members as written
	Owners    []string `json:"owners,omitempty"`   // a tag: its tagOwners as written
	Address   string   `json:"address,omitempty"`  // a host: its address from "hosts"
	Approves  []string `json:"approves,omitempty"` // autoApprovers: "exit node", "route <prefix>"
	// Unused is true for a group, tag or host that the policy defines but that no rule uses.
	Unused bool `json:"unused,omitempty"`
}

// Edge is "From can reach To" with one access: a protocol and ports, or apps, or SSH users.
type Edge struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Proto    string   `json:"proto,omitempty"`
	Ports    string   `json:"ports,omitempty"`
	Via      []string `json:"via,omitempty"`
	Posture  []string `json:"posture,omitempty"`
	Apps     []string `json:"apps,omitempty"`
	SSHUsers []string `json:"ssh_users,omitempty"`
	// SSHAction is "accept" or "check" for an SSH rule, empty otherwise.
	SSHAction string `json:"ssh_action,omitempty"`
	Refs      []Ref  `json:"refs"`
}

// Graph is the map of one policy. The slices are never nil, so the JSON has [] and not null.
type Graph struct {
	Nodes     []Node   `json:"nodes"`
	Edges     []Edge   `json:"edges"`
	Warnings  []string `json:"warnings"`
	Truncated bool     `json:"truncated"`
}

// Build parses policy text and returns its graph. Blank text gives an empty graph.
func Build(text string) (Graph, error) {
	doc, err := parseDocument(text)
	if err != nil {
		return Graph{}, err
	}
	b := newBuilder(doc.hosts)
	b.addDefinitions(doc)
	for _, r := range doc.acls {
		b.addACL(r)
	}
	for _, r := range doc.grants {
		b.addGrant(r)
	}
	for _, r := range doc.ssh {
		b.addSSH(r)
	}
	b.addApprovers(doc.approvers)
	for _, w := range doc.warnings {
		b.warn(w)
	}
	return b.graph(), nil
}

// sortedKeys returns the keys of m in order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
