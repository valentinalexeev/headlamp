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
type EdgeDiff struct {
	Added   []Edge `json:"added"`
	Removed []Edge `json:"removed"`
	Changed []Pair `json:"changed"`
}

// Diff compares the edges of before and after. The slices are never nil and keep the order of
// the graphs.
func Diff(before, after Graph) EdgeDiff {
	d := EdgeDiff{Added: []Edge{}, Removed: []Edge{}, Changed: []Pair{}}
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
	return d
}
