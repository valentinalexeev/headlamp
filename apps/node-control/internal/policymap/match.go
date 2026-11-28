package policymap

// Device is one headscale node as the selector match sees it.
type Device struct {
	Tags []string
	// Owner holds the forms that a user selector of the node's user can take: "name@", the name and the e-mail.
	// It is empty for a tagged node, because a user selector never matches a tagged node.
	Owner []string
}

// Unmatched returns the tag, group and user nodes of g that match no device, in the order of g. A user counts only
// when a rule or autoApprovers names it: a user that is only a tag owner or a group member needs no node. A group
// matches when one of its members matches. #432
func Unmatched(g Graph, devices []Device) []Node {
	tags, owners := map[string]bool{}, map[string]bool{}
	for _, d := range devices {
		for _, t := range d.Tags {
			tags[t] = true
		}
		for _, o := range d.Owner {
			owners[o] = true
		}
	}
	members := map[string][]string{}
	for _, n := range g.Nodes {
		if n.Kind == KindGroup {
			members[n.ID] = n.Members
		}
	}
	var matches func(id string, seen map[string]bool) bool
	matches = func(id string, seen map[string]bool) bool {
		switch {
		case tags[id] || owners[id]:
			return true
		case seen[id]:
			return false
		}
		seen[id] = true
		for _, m := range members[id] {
			if matches(m, seen) {
				return true
			}
		}
		return false
	}
	var out []Node
	for _, n := range g.Nodes {
		switch n.Kind {
		case KindTag, KindGroup:
		case KindUser:
			if len(n.RuleTypes) == 0 && len(n.Approves) == 0 {
				continue
			}
		default:
			continue
		}
		if !matches(n.ID, map[string]bool{}) {
			out = append(out, n)
		}
	}
	return out
}
