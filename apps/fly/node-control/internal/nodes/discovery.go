package nodes

import (
	"regexp"
	"sort"
	"strings"

	"example.com/node-control/internal/headscale"
)

const (
	exitTag     = "tag:exit"
	cloudPrefix = "tag:cloud-"
)

var dupSuffixRE = regexp.MustCompile(`^(.+)-[0-9]+$`)

// Discover turns the headscale node list into targets. A node is an exit node
// when it carries tag:exit; its id is the lowercased reported name; the
// provider comes from tag:cloud-<provider>. Explicit cfg.Nodes entries win and
// also create targets for nodes headscale does not list. The result is sorted
// by id.
func Discover(cfg *Config, ns []headscale.Node) []Target {
	found := discoverExit(ns)

	hidden := map[string]bool{}
	for id, e := range cfg.Nodes {
		if e.Hidden {
			hidden[id] = true
		}
	}

	byID := map[string]Target{}
	for id, t := range found {
		if !hidden[id] {
			byID[id] = t
		}
	}

	for id, e := range cfg.Nodes {
		if e.Provider == "" {
			continue // override-only entry
		}
		tn := firstNonEmpty(e.TailnetName, id)
		t := Target{ID: id, Name: id, TailnetName: tn, Provider: e.Provider, Block: e.Block}
		var m *headscale.Node
		if d, ok := found[id]; ok && e.TailnetName == "" {
			// the explicit id is a discovered (possibly collapsed) id: keep the winning registration
			t.Name, t.TailnetName = d.Name, d.TailnetName
			m = matchNode(ns, d.Name)
		} else if alias, ok := cfg.Aliases[id]; ok && e.TailnetName == "" && matchNode(ns, alias) != nil {
			// the explicit id is an old id that an alias maps to a live node
			m = matchNode(ns, alias)
			t.Name, t.TailnetName = m.Name, m.Name
		} else {
			m = matchNode(ns, tn)
			if m != nil {
				t.Name = m.Name
			}
		}
		if m != nil {
			// the explicit entry replaces the discovered one for that node
			for did, d := range byID {
				if did != id && strings.EqualFold(d.Name, m.Name) {
					delete(byID, did)
				}
			}
		}
		if hidden[id] {
			delete(byID, id)
			continue
		}
		byID[id] = t
	}

	out := make([]Target, 0, len(byID))
	for _, t := range byID {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// Find resolves an id from the API: a target id, else an alias (old id ->
// node name) matched against target ids and reported names.
func Find(cfg *Config, ts []Target, id string) (Target, bool) {
	for _, t := range ts {
		if t.ID == id {
			return t, true
		}
	}
	name, ok := cfg.Aliases[id]
	if !ok {
		return Target{}, false
	}
	for _, t := range ts {
		if t.ID == strings.ToLower(name) || strings.EqualFold(t.Name, name) || strings.EqualFold(t.TailnetName, name) {
			return t, true
		}
	}
	return Target{}, false
}

// discoverExit builds tag:exit targets keyed by id, collapsing duplicate
// registrations: x-N folds into x only when a node named x exists.
func discoverExit(ns []headscale.Node) map[string]Target {
	names := map[string]bool{}
	for _, n := range ns {
		names[strings.ToLower(n.Name)] = true
	}
	groups := map[string][]headscale.Node{}
	for _, n := range ns {
		id := strings.ToLower(n.Name)
		if !idRE.MatchString(id) || !hasTag(n.Tags, exitTag) {
			continue
		}
		if m := dupSuffixRE.FindStringSubmatch(id); m != nil && names[m[1]] {
			id = m[1]
		}
		groups[id] = append(groups[id], n)
	}
	out := map[string]Target{}
	for id, g := range groups {
		win := g[0]
		for _, n := range g[1:] {
			switch {
			case n.Online && !win.Online:
				win = n
			case n.Online == win.Online && strings.ToLower(n.Name) == id && strings.ToLower(win.Name) != id:
				win = n // tie: prefer the canonical name
			}
		}
		// The target is named after the canonical registration (the group id's own
		// name), not the winner: VMs are located by it and the tailnet line is
		// keyed by it. Online state and tags still come from the winner.
		canon := canonicalName(id, g, ns, win)
		out[id] = Target{ID: id, Name: canon, TailnetName: canon, Provider: cloudProvider(win.Tags)}
	}
	return out
}

// canonicalName is the reported name of the registration whose lowercased name
// is id: preferably inside the group, else any node (a collapsed -N group whose
// base registration lacks tag:exit), else the winner's own name.
func canonicalName(id string, group, all []headscale.Node, win headscale.Node) string {
	for _, list := range [][]headscale.Node{group, all} {
		for _, n := range list {
			if strings.ToLower(n.Name) == id {
				return n.Name
			}
		}
	}
	return win.Name
}

// matchNode finds the node an explicit entry refers to. Unlike discovery, which
// identifies nodes by reported Name only, an explicit tailnetName may be either
// the reported Name or the given name; an online node is preferred.
func matchNode(ns []headscale.Node, name string) *headscale.Node {
	var best *headscale.Node
	for i := range ns {
		n := &ns[i]
		if strings.EqualFold(n.Name, name) || strings.EqualFold(n.GivenName, name) {
			if best == nil || (n.Online && !best.Online) {
				best = n
			}
		}
	}
	return best
}

func cloudProvider(tags []string) string {
	for _, p := range Providers {
		if hasTag(tags, cloudPrefix+p) {
			return p
		}
	}
	return ""
}

func hasTag(tags []string, want string) bool {
	for _, t := range tags {
		if t == want {
			return true
		}
	}
	return false
}
