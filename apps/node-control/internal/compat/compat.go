// Package compat holds the compatibility matrix of headlamp (#300): for each interface between
// node-control and
// another component, the interface version this release speaks and the range it accepts.
// node-control checks what
// each part reports against it. The repo version stays the version of node-control; the other
// components are not
// redeployed when they did not change, and the matrix says whether the installed mix still works
// together.
package compat

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
)

//go:embed compat.json
var raw []byte

// The interfaces node-control has with other components.
const (
	AgentAPI  = "agent-api"  // node-control <-> ingress-agent
	FunnelAPI = "funnel-api" // node-control <-> funnel-ingress
	RunnerAPI = "runner-api" // node-control <-> task-runner
)

// Range is the interface versions node-control accepts, both ends included.
type Range struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// Interface is one row of the matrix.
type Interface struct {
	Between  []string `json:"between"`
	Current  int      `json:"current"`
	Accepted Range    `json:"accepted"`
	Covers   []string `json:"covers"`
}

// Matrix is the whole file.
type Matrix struct {
	Interfaces map[string]Interface `json:"interfaces"`
}

// parts is the number of components an interface is between.
const parts = 2

// Load parses the embedded matrix and checks that it is consistent.
func Load() (Matrix, error) { return Parse(raw) }

// Parse parses and checks a matrix: every interface has two parts, a current version of 1 or more
// inside its accepted
// range, and the files that define it.
func Parse(b []byte) (Matrix, error) {
	var m Matrix
	if err := json.Unmarshal(b, &m); err != nil {
		return Matrix{}, fmt.Errorf("compat.json: %w", err)
	}
	if len(m.Interfaces) == 0 {
		return Matrix{}, fmt.Errorf("compat.json: no interfaces")
	}
	for name, i := range m.Interfaces {
		switch {
		case len(i.Between) != parts:
			return Matrix{}, fmt.Errorf("compat.json: %s: between must name two components", name)
		case i.Current < 1 || i.Accepted.Min < 1 || i.Accepted.Min > i.Accepted.Max:
			return Matrix{}, fmt.Errorf("compat.json: %s: bad versions", name)
		case i.Current < i.Accepted.Min || i.Current > i.Accepted.Max:
			return Matrix{}, fmt.Errorf("compat.json: %s: current %d is outside its own range",
				name, i.Current)
		case len(i.Covers) == 0:
			return Matrix{}, fmt.Errorf("compat.json: %s: covers names no file", name)
		}
	}
	return m, nil
}

// Must is Load for the embedded matrix, which the tests check: a bad file is a build error, not a
// runtime state.
func Must() Matrix {
	m, err := Load()
	if err != nil {
		panic(err)
	}
	return m
}

// State is the result of one check.
type State string

// The results of Check.
const (
	OK           State = "ok"
	Incompatible State = "incompatible"
	Unknown      State = "unknown" // the part reports no interface version (it is older than #300)
)

// Result is the check of one part.
type Result struct {
	Interface string `json:"interface"`
	State     State  `json:"state"`
	Reported  int    `json:"reported,omitempty"`
	Accepted  Range  `json:"accepted"`
	// Hint is what the admin does about an incompatible part: newer means the part is ahead of
	// node-control.
	Hint string `json:"hint,omitempty"`
}

// Check compares the interface version a part reports (0: none) with the range of the interface. An
// unknown
// interface name is a programming error and panics.
func (m Matrix) Check(iface string, reported int) Result {
	i, ok := m.Interfaces[iface]
	if !ok {
		panic("compat: unknown interface " + iface)
	}
	r := Result{Interface: iface, Reported: reported, Accepted: i.Accepted}
	switch {
	case reported <= 0:
		r.State = Unknown
	case reported < i.Accepted.Min:
		r.State, r.Hint = Incompatible, "older than node-control accepts: upgrade "+i.Between[1]
	case reported > i.Accepted.Max:
		r.State, r.Hint = Incompatible, "newer than node-control accepts: upgrade node-control"
	default:
		r.State = OK
	}
	return r
}

// Refused says whether COMPAT_STRICT refuses a part with this result (#322): an incompatible
// version, or no version at all (a part older than 0.18.0). A part that has not answered yet is
// not checked, so the caller does not call Refused for it.
func (r Result) Refused() bool { return r.State == Incompatible || r.State == Unknown }

// Why is the short reason of a refusal, for logs and sync states.
func (r Result) Why() string {
	if r.State == Unknown {
		return r.Interface + " version not reported (older than 0.18.0): upgrade the part"
	}
	return fmt.Sprintf("%s %d is %s", r.Interface, r.Reported, r.Hint)
}

// Names lists the interfaces in a fixed order.
func (m Matrix) Names() []string {
	out := make([]string, 0, len(m.Interfaces))
	for k := range m.Interfaces {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
