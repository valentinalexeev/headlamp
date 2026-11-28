package policymap

// Pair names a source and a destination.
type Pair struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// EdgeDiff is the change of reachability from one graph to the next. An edge is the same in both
// graphs when everything but its rule references is equal, so a rule that only moved is no change.
// Added edges carry the references of the new graph, removed edges those of the old graph.
// Changed lists the pairs that have both an added and a removed edge, for example new ports.
// RemovedNodes are the ends of removed edges that the new graph does not have, so that a page can
// draw the removed edges.
//
// A new group member changes no edge, because the edges start at the group. Thus the diff also
// compares paths: the edges with each group replaced by its members (MembersAdded, MembersRemoved,
// NewPaths, RemovedPaths). PathsTruncated is true when a graph has more than PathsMax paths; the
// path counts are then too low.
//
// When a graph is truncated, the diff can show edges as added or removed that only the cut hid:
// the caller shows the Truncated flag of both graphs with the diff.
type EdgeDiff struct {
	Added          []Edge   `json:"added"`
	Removed        []Edge   `json:"removed"`
	Changed        []Pair   `json:"changed"`
	RemovedNodes   []Node   `json:"removed_nodes"`
	MembersAdded   []Member `json:"members_added"`
	MembersRemoved []Member `json:"members_removed"`
	NewPaths       int      `json:"new_paths"`
	RemovedPaths   int      `json:"removed_paths"`
	PathsTruncated bool     `json:"paths_truncated"`
}

// Member is one member of a group, as written in the policy.
type Member struct {
	Group  string `json:"group"`
	Member string `json:"member"`
}

// PathsMax limits the paths that Diff counts in one graph.
const PathsMax = PairsMax

// Diff compares the edges, group members and paths of before and after. The slices are never nil
// and keep the order of the graphs.
func Diff(before, after Graph) EdgeDiff {
	d := EdgeDiff{Added: []Edge{}, Removed: []Edge{}, Changed: []Pair{}, RemovedNodes: []Node{},
		MembersAdded: []Member{}, MembersRemoved: []Member{}}
	old := make(map[string]bool, len(before.Edges))
	for _, e := range before.Edges {
		old[edgeKey(e)] = true
	}
	next := make(map[string]bool, len(after.Edges))
	for _, e := range after.Edges {
		next[edgeKey(e)] = true
		if !old[edgeKey(e)] {
			d.Added = append(d.Added, e)
		}
	}
	for _, e := range before.Edges {
		if !next[edgeKey(e)] {
			d.Removed = append(d.Removed, e)
		}
	}
	added := make(map[Pair]bool, len(d.Added))
	for _, e := range d.Added {
		added[Pair{e.From, e.To}] = true
	}
	seen := make(map[Pair]bool, len(d.Removed))
	for _, e := range d.Removed {
		p := Pair{e.From, e.To}
		if added[p] && !seen[p] {
			seen[p] = true
			d.Changed = append(d.Changed, p)
		}
	}
	d.RemovedNodes = removedNodes(before, after, d.Removed)
	d.MembersAdded = memberChanges(after, before)
	d.MembersRemoved = memberChanges(before, after)
	oldPaths, cutOld := paths(before)
	newPaths, cutNew := paths(after)
	d.PathsTruncated = cutOld || cutNew
	for k := range newPaths {
		if !oldPaths[k] {
			d.NewPaths++
		}
	}
	for k := range oldPaths {
		if !newPaths[k] {
			d.RemovedPaths++
		}
	}
	return d
}

// removedNodes returns the nodes of before that end a removed edge and that after does not have.
func removedNodes(before, after Graph, removed []Edge) []Node {
	have := make(map[string]bool, len(after.Nodes))
	for _, n := range after.Nodes {
		have[n.ID] = true
	}
	ends := map[string]bool{}
	for _, e := range removed {
		ends[e.From], ends[e.To] = true, true
	}
	out := []Node{}
	for _, n := range before.Nodes {
		if ends[n.ID] && !have[n.ID] {
			out = append(out, n)
		}
	}
	return out
}

// memberChanges returns the group members of a that b does not have, in the order of a.
func memberChanges(a, b Graph) []Member {
	old := map[Member]bool{}
	for _, n := range b.Nodes {
		for _, m := range n.Members {
			old[Member{n.ID, m}] = true
		}
	}
	out := []Member{}
	for _, n := range a.Nodes {
		if n.Kind != KindGroup {
			continue
		}
		for _, m := range distinct(n.Members) {
			if !old[Member{n.ID, m}] {
				out = append(out, Member{n.ID, m})
			}
		}
	}
	return out
}

// paths returns the keys of the paths of g: each edge with a defined group at either end
// replaced by each of its members, through groups in groups. It stops at PathsMax paths, or after
// 2*PathsMax combinations when edges repeat paths, and then reports true.
func paths(g Graph) (map[string]bool, bool) {
	groups := map[string][]string{}
	for _, n := range g.Nodes {
		if n.Kind == KindGroup && n.Members != nil {
			groups[n.ID] = n.Members
		}
	}
	memo := map[string][]string{}
	out := map[string]bool{}
	visits := 0
	for _, e := range g.Edges {
		access := e
		access.From, access.To, access.Refs = "", "", nil
		rest := edgeKey(access)
		for _, src := range expand(e.From, groups, memo) {
			for _, dst := range expand(e.To, groups, memo) {
				if len(out) >= PathsMax || visits >= 2*PathsMax {
					return out, true
				}
				visits++
				out[src+"\x00"+dst+"\x00"+rest] = true
			}
		}
	}
	return out, false
}

// expand returns the members of a defined group, with groups in it expanded, or id itself for any
// other selector. A cycle of groups is cut where it closes, so a group in a cycle can miss members
// of the cycle; headscale refuses such a policy anyway.
func expand(id string, groups map[string][]string, memo map[string][]string) []string {
	if _, ok := groups[id]; !ok {
		return []string{id}
	}
	if m, ok := memo[id]; ok {
		return m
	}
	memo[id] = nil // a cycle back to id adds nothing
	var out []string
	for _, m := range groups[id] {
		out = append(out, expand(m, groups, memo)...)
	}
	out = distinct(out)
	memo[id] = out
	return out
}
