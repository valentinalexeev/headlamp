package enroll

import (
	"fmt"
	"slices"
	"sort"
)

// GrantGroup is one grant group of one bridge: its id, tag, target names, the union of their dst, and its
// devices. OverLimit is set for the groups past MaxGroupsPerBridge: they get no tailnet route.
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
	// Unresolved are the group subjects with grants on this bridge: they give no access yet (#102).
	Unresolved []string `json:"unresolved"`
	Error      string   `json:"error,omitempty"`
}

// EffectiveMode is the tailnet_access of a bridge: the console override, else enroll.json, else "off".
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

// DeviceTargets is the set of target names a device holds on a bridge: its own grants and the grants of its
// owner, limited to targets that exist. Group grants are not resolved.
func DeviceTargets(doc TailnetDoc, targets map[string][]string, bridge string, d Device) []string {
	var out []string
	for _, g := range doc.Grants {
		if g.Bridge != bridge || targets[g.Target] == nil {
			continue
		}
		hit := (g.Kind == SubjectDevice && g.Subject == fmt.Sprint(d.ID)) ||
			(g.Kind == SubjectOwner && d.Owner != "" && g.Subject == d.Owner)
		if hit && !slices.Contains(out, g.Target) {
			out = append(out, g.Target)
		}
	}
	slices.Sort(out)
	return out
}

// ResolveGrants computes the grant groups of one bridge from the active devices. Groups are ordered by their
// smallest device id, so a new device never moves an existing group past the limit.
func ResolveGrants(doc TailnetDoc, all []TailnetTarget, bridge string, devices []Device) BridgeGrants {
	dst := map[string][]string{}
	for _, t := range all {
		dst[t.Name] = t.Dst
	}
	out := BridgeGrants{Bridge: bridge, ByDevice: map[int64]string{}, Groups: []GrantGroup{}, Unresolved: []string{}}
	byID := map[string]*GrantGroup{}
	for _, d := range devices {
		names := DeviceTargets(doc, dst, bridge, d)
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
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].Devices[0] < out.Groups[j].Devices[0] })
	for i := MaxGroupsPerBridge; i < len(out.Groups); i++ {
		out.Groups[i].OverLimit = true
		for _, id := range out.Groups[i].Devices {
			delete(out.ByDevice, id)
		}
	}
	if len(out.Groups) > MaxGroupsPerBridge {
		out.Error = fmt.Sprintf("%d grant groups, the limit is %d per bridge", len(out.Groups), MaxGroupsPerBridge)
	}
	for _, g := range doc.Grants {
		if g.Bridge == bridge && g.Kind == SubjectGroup && !slices.Contains(out.Unresolved, g.Subject) {
			out.Unresolved = append(out.Unresolved, g.Subject)
		}
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

// WantedGroupRules is the generated ACL entry set of all bridges: tag -> dst, without the over-limit groups.
func WantedGroupRules(bridges []BridgeGrants) map[string][]string {
	out := map[string][]string{}
	for _, b := range bridges {
		for _, g := range b.Groups {
			if !g.OverLimit {
				out[g.Tag] = unionDst(map[string][]string{"a": out[g.Tag], "b": g.Dst}, []string{"a", "b"})
			}
		}
	}
	return out
}
