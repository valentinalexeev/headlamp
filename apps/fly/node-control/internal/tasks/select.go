package tasks

import (
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"sort"

	"example.com/node-control/internal/headscale"
)

// ErrNoEligibleNode is returned when no node can run the task right now; the
// scheduler treats it as a skipped run, not a script failure.
var ErrNoEligibleNode = errors.New("no eligible node")

// ErrAmbiguousTarget: more than one node has the given name of a node target.
// It wraps ErrNoEligibleNode: nothing is probed and nothing runs.
var ErrAmbiguousTarget = fmt.Errorf("%w: ambiguous target", ErrNoEligibleNode)

// DefaultRunnerTag is the headscale tag a node must carry to be a candidate
// for any task; TASKS_RUNNER_TAG overrides it.
const DefaultRunnerTag = "tag:task-runner"

// runnerTagRe is the accepted form of TASKS_RUNNER_TAG.
var runnerTagRe = regexp.MustCompile(`^tag:[a-z0-9][a-z0-9-]{0,62}$`)

// WithTag keeps the nodes that carry tag (headscale.Node.Tags: the single tags
// list of headscale 0.28+, or the forced and valid tags of older versions).
// This filter is what keeps any other tailnet node from ever being probed or
// sent the runner token. A node gets a tag at registration only if the
// policy's tagOwners allows it (older versions also accept a later request),
// and otherwise only from an admin, so the trust boundary for the runner tag
// is its tagOwners entry: only the admin/service identity may own it, never a
// human user whose devices are not runners. Tag runners as an admin. An empty
// tag keeps nothing.
func WithTag(nodes []headscale.Node, tag string) []headscale.Node {
	var out []headscale.Node
	if tag == "" {
		return out
	}
	for _, n := range nodes {
		if slices.Contains(n.Tags, tag) {
			out = append(out, n)
		}
	}
	return out
}

// Candidate is a node chosen to run a task and the address to reach it at.
type Candidate struct {
	Node headscale.Node
	Addr string // first IPv4 address of the node
}

// Select picks the node to run a task on. It is pure: the caller supplies the
// node list (already narrowed to the runner tag, see WithTag), the health
// probe, the per-node load and the last node that succeeded, so the choice is
// deterministic and testable.
//
// A node target matches the headscale given name only: headscale keeps it
// unique and the admin can pin it (headscale nodes rename), while the reported
// host name is whatever the node claims. Two nodes with the same given name
// make the target ambiguous: no node is probed and the error wraps
// ErrAmbiguousTarget. Trust itself comes from the runner tag (WithTag).
//
// Order among eligible nodes: lastOK (keeps a task on the node whose state it
// already warmed), then the least busy, then name and given name, independent
// of input order.
func Select(t Target, nodes []headscale.Node, healthy func(headscale.Node) bool, busy map[string]int, lastOK string) (Candidate, error) {
	if t.Node != "" {
		same := 0
		for _, n := range nodes {
			if n.GivenName == t.Node {
				same++
			}
		}
		if same > 1 {
			return Candidate{}, fmt.Errorf("%w: %d nodes have the given name %q", ErrAmbiguousTarget, same, t.Node)
		}
	}
	var cands []Candidate
	for _, n := range nodes {
		if !matches(t, n) || (t.Online && !n.Online) {
			continue
		}
		addr, ok := firstIPv4(n)
		if !ok {
			continue
		}
		// Last: the probe may be a network call, so only survivors of the
		// cheap filters are asked, once each.
		if !healthy(n) {
			continue
		}
		cands = append(cands, Candidate{Node: n, Addr: addr})
	}
	if len(cands) == 0 {
		return Candidate{}, ErrNoEligibleNode
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i].Node, cands[j].Node
		if al, bl := a.Name == lastOK, b.Name == lastOK; al != bl {
			return al
		}
		if busy[a.Name] != busy[b.Name] {
			return busy[a.Name] < busy[b.Name]
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.GivenName < b.GivenName
	})
	return cands[0], nil
}

func matches(t Target, n headscale.Node) bool {
	if t.Node != "" {
		return n.GivenName == t.Node
	}
	if len(t.Tags) == 0 {
		return false
	}
	for _, tag := range t.Tags {
		if !slices.Contains(n.Tags, tag) {
			return false
		}
	}
	return true
}

func firstIPv4(n headscale.Node) (string, bool) {
	for _, s := range n.IPAddresses {
		if a, err := netip.ParseAddr(s); err == nil && a.Is4() {
			return a.String(), true
		}
	}
	return "", false
}
