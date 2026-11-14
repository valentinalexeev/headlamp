package tasks

import (
	"fmt"
	"strings"
)

// FailureClass says whether a retry can fix a failed attempt (#323).
type FailureClass string

const (
	// ClassTransient: a retry can fix it (node busy, timeout, runner unreachable, lost).
	ClassTransient FailureClass = "transient"
	// ClassStructural: a retry never fixes it; someone must change the setup
	// (no eligible node, ambiguous target, script not found, secret not configured).
	ClassStructural FailureClass = "structural"
	// ClassUnknown: the script failed or returned a bad result. A script
	// cannot mark its own failure as structural.
	ClassUnknown FailureClass = "unknown"
)

// NodeCounts are the candidate nodes left after each filter of Select, so a
// "no eligible node" failure says why. Each count is a subset of the one
// before it.
type NodeCounts struct {
	Tagged   int `json:"tagged"`   // carry the runner tag
	Matching int `json:"matching"` // of those, match the target
	// Online: of those, online (every matching node when target.online is not set).
	Online  int `json:"online"`
	Address int `json:"address"` // of those, have a tailnet IPv4 address
	Healthy int `json:"healthy"` // of those, the runner health probe passed
}

// Detail is the short text of the counts, up to the first filter that left no
// node: "tagged 0", "tagged 2, matching 1, online 0".
func (c NodeCounts) Detail() string {
	steps := []struct {
		name string
		n    int
	}{
		{"tagged", c.Tagged}, {"matching", c.Matching}, {"online", c.Online},
		{"with address", c.Address}, {"healthy", c.Healthy},
	}
	var parts []string
	for _, s := range steps {
		parts = append(parts, fmt.Sprintf("%s %d", s.name, s.n))
		if s.n == 0 {
			break
		}
	}
	return strings.Join(parts, ", ")
}

// runReasonText is the reason with the node counts appended, as the UI and the
// Attention item show it: "no eligible node (tagged 0)".
func runReasonText(r Run) string {
	if r.Counts == nil {
		return r.Reason
	}
	return r.Reason + " (" + r.Counts.Detail() + ")"
}

// FailureInfo is what the notifier gets about a failed sequence.
type FailureInfo struct {
	Reason string
	Class  FailureClass
	Detail string // the node counts of a "no eligible node" failure, else ""
	Streak int    // the failure streak including this failure; 0 counts as 1
}
