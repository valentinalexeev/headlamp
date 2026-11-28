package enroll

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// GrantGroup is one grant group of one bridge: its id, tag, target names, the union of their dst,
// and its devices. OverLimit is set for the groups past MaxGroupsPerBridge: they get no tailnet
// route.
type GrantGroup struct {
	ID        string   `json:"id"`
	Tag       string   `json:"tag"`
	Targets   []string `json:"targets"`
	Dst       []string `json:"dst"`
	Devices   []int64  `json:"devices"`
	OverLimit bool     `json:"over_limit,omitempty"`
}

// BridgeGrants is the resolved grant state of one bridge in grants mode.
type BridgeGrants struct {
	Bridge string `json:"bridge"`
	// ByDevice maps a device to the group id it is in ("" or absent: no tailnet access).
	ByDevice map[int64]string `json:"-"`
	Groups   []GrantGroup     `json:"groups"`
	// Unresolved are the group subjects with grants on this bridge when node-control does not know
	// the groups of the owners (OIDC_GROUP_GRANT_HOURS unset): they give no access (#102).
	Unresolved []string `json:"unresolved"`
	// Resolved are the group subjects with grants on this bridge when it knows them (#461), each
	// with the number of active devices whose owner is in the group now.
	Resolved []ResolvedGroup `json:"resolved"`
	Error    string          `json:"error,omitempty"`
}

// ResolvedGroup is one group subject of a bridge and its matching active devices.
type ResolvedGroup struct {
	Subject string `json:"subject"`
	Devices int    `json:"devices"`
}

// inGroup says whether an owner with groups is in the group of a grant subject ("group:<name>",
// or the bare claim value).
func inGroup(subject string, groups []string) bool {
	name := strings.TrimPrefix(subject, "group:")
	for _, g := range groups {
		if g == subject || g == name {
			return true
		}
	}
	return false
}

// EffectiveMode is the tailnet_access of a bridge: the console override, else enroll.json, else
// "off".
func (d TailnetDoc) EffectiveMode(in Ingress) string {
	if m, ok := d.Modes[in.Name]; ok && ValidTailnetMode(m) {
		return m
	}
	if in.TailnetAccess == "" {
		return TailnetOff
	}
	return in.TailnetAccess
}

// AllTargets merges the config targets (they win on a name clash) with the console targets.
func AllTargets(config []TailnetTarget, doc TailnetDoc) []TailnetTarget {
	out := slices.Clone(config)
	for _, t := range doc.Targets {
		if !slices.ContainsFunc(config, func(c TailnetTarget) bool { return c.Name == t.Name }) {
			t.Source = "console"
			out = append(out, t)
		}
	}
	return out
}

// DeviceTargets is the set of target names a device holds on a bridge: its own grants, the
// grants of its owner and the grants of the groups of its owner (groups: owner -> groups, #461),
// limited to targets that exist.
func DeviceTargets(
	doc TailnetDoc, targets map[string][]string, bridge string, d Device, groups map[string][]string,
) []string {
	var out []string
	for _, g := range doc.Grants {
		if g.Bridge != bridge || targets[g.Target] == nil {
			continue
		}
		hit := (g.Kind == SubjectDevice && g.Subject == fmt.Sprint(d.ID)) ||
			(g.Kind == SubjectOwner && d.Owner != "" && g.Subject == d.Owner) ||
			(g.Kind == SubjectGroup && d.Owner != "" && inGroup(g.Subject, groups[d.Owner]))
		if hit && !slices.Contains(out, g.Target) {
			out = append(out, g.Target)
		}
	}
	slices.Sort(out)
	return out
}

// ResolveGrants computes the grant groups of one bridge from the active devices. Groups are ordered
// by their smallest device id, so a new device never moves an existing group past the limit.
// groups maps an owner to its groups now; nil: group grants are not resolved.
func ResolveGrants(
	doc TailnetDoc,
	all []TailnetTarget,
	bridge string,
	devices []Device,
	groups map[string][]string,
) BridgeGrants {
	dst := map[string][]string{}
	for _, t := range all {
		dst[t.Name] = t.Dst
	}
	out := BridgeGrants{
		Bridge:     bridge,
		ByDevice:   map[int64]string{},
		Groups:     []GrantGroup{},
		Unresolved: []string{},
		Resolved:   []ResolvedGroup{},
	}
	byID := map[string]*GrantGroup{}
	for _, d := range devices {
		names := DeviceTargets(doc, dst, bridge, d, groups)
		if len(names) == 0 {
			continue
		}
		id := GroupID(names)
		g := byID[id]
		if g == nil {
			g = &GrantGroup{ID: id, Tag: GroupTag(id), Targets: names, Dst: unionDst(dst, names)}
			byID[id] = g
		}
		g.Devices = append(g.Devices, d.ID)
		out.ByDevice[d.ID] = id
	}
	for _, g := range byID {
		slices.Sort(g.Devices)
		out.Groups = append(out.Groups, *g)
	}
	sort.Slice(
		out.Groups,
		func(i, j int) bool { return out.Groups[i].Devices[0] < out.Groups[j].Devices[0] },
	)
	for i := MaxGroupsPerBridge; i < len(out.Groups); i++ {
		out.Groups[i].OverLimit = true
		for _, id := range out.Groups[i].Devices {
			delete(out.ByDevice, id)
		}
	}
	if len(out.Groups) > MaxGroupsPerBridge {
		out.Error = fmt.Sprintf(
			"%d grant groups, the limit is %d per bridge",
			len(out.Groups),
			MaxGroupsPerBridge,
		)
	}
	for _, g := range doc.Grants {
		if g.Bridge != bridge || g.Kind != SubjectGroup {
			continue
		}
		if groups == nil {
			if !slices.Contains(out.Unresolved, g.Subject) {
				out.Unresolved = append(out.Unresolved, g.Subject)
			}
			continue
		}
		seen := func(r ResolvedGroup) bool { return r.Subject == g.Subject }
		if slices.ContainsFunc(out.Resolved, seen) {
			continue
		}
		r := ResolvedGroup{Subject: g.Subject}
		for _, d := range devices {
			if d.Owner != "" && inGroup(g.Subject, groups[d.Owner]) {
				r.Devices++
			}
		}
		out.Resolved = append(out.Resolved, r)
	}
	return out
}

func unionDst(dst map[string][]string, names []string) []string {
	var out []string
	for _, n := range names {
		out = append(out, dst[n]...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// WantedGroupRules is the generated ACL entry set of all bridges: tag -> dst, without the
// over-limit groups.
func WantedGroupRules(bridges []BridgeGrants) map[string][]string {
	out := map[string][]string{}
	for _, b := range bridges {
		for _, g := range b.Groups {
			if !g.OverLimit {
				out[g.Tag] = unionDst(
					map[string][]string{"a": out[g.Tag], "b": g.Dst},
					[]string{"a", "b"},
				)
			}
		}
	}
	return out
}
