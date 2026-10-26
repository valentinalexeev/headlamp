package tasks

import (
	"errors"
	"net/netip"
	"slices"
	"sort"

	"example.com/node-control/internal/headscale"
)

// ErrNoEligibleNode is returned when no node can run the task right now; the
// scheduler treats it as a skipped run, not a script failure.
var ErrNoEligibleNode = errors.New("no eligible node")

// Candidate is a node chosen to run a task and the address to reach it at.
type Candidate struct {
	Node headscale.Node
	Addr string // first IPv4 address of the node
}

// Select picks the node to run a task on. It is pure: the caller supplies the
// node list, the health probe, the per-node load and the last node that
// succeeded, so the choice is deterministic and testable.
//
// Order among eligible nodes: lastOK (keeps a task on the node whose state it
// already warmed), then the least busy, then name, independent of input order.
func Select(t Target, nodes []headscale.Node, healthy func(headscale.Node) bool, busy map[string]int, lastOK string) (Candidate, error) {
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
	sort.Slice(cands, func(i, j int) bool {
		a, b := cands[i].Node, cands[j].Node
		if al, bl := a.Name == lastOK, b.Name == lastOK; al != bl {
			return al
		}
		if busy[a.Name] != busy[b.Name] {
			return busy[a.Name] < busy[b.Name]
		}
		return a.Name < b.Name
	})
	return cands[0], nil
}

func matches(t Target, n headscale.Node) bool {
	if t.Node != "" {
		return n.Name == t.Node || n.GivenName == t.Node
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
