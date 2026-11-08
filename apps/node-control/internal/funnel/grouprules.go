package funnel

import (
	"bytes"
	"errors"
	"slices"
	"strings"
)

// maxGroupRules limits the generated group rules one call may write (#139: 8 grant groups per bridge, 8 bridges).
const maxGroupRules = 64

// ErrNoTagOwner is returned when a group tag needs a tagOwners entry but the base tag has none to copy.
var ErrNoTagOwner = errors.New("the base tag has no tagOwners entry to copy for the group tags")

// GroupRules reads the generated group rules of the policy: for each rule whose src is exactly one tag that
// starts with prefix, the tag and its dst (sorted). Other rules are ignored.
func GroupRules(policy, prefix string) (map[string][]string, error) {
	_, acls, err := policyParts(policy)
	if err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, r := range acls {
		if tag, ok := groupSrc(r, prefix); ok {
			dst, _ := stringList(r.(*ordObj).m["dst"])
			d := slices.Clone(dst)
			slices.Sort(d)
			out[tag] = append(out[tag], d...)
		}
	}
	return out, nil
}

// ComposeGroupRules sets the generated group rules (#139): one rule "accept src [tag] dst want[tag]" and one
// tagOwners entry per tag in want, with the owners of baseTag. Rules and tagOwners entries whose tag starts
// with prefix and is not in want are removed. Everything else, and the key order, is left alone. When the
// policy already matches, the original text comes back. Each tag in want must start with prefix.
func ComposeGroupRules(policy, prefix, baseTag string, want map[string][]string) (string, error) {
	if prefix == "" || len(want) > maxGroupRules {
		return "", ErrPolicyShape
	}
	top, acls, err := policyParts(policy)
	if err != nil {
		return "", err
	}
	tags := make([]string, 0, len(want))
	for t := range want {
		if !strings.HasPrefix(t, prefix) || len(want[t]) == 0 {
			return "", ErrPolicyShape
		}
		tags = append(tags, t)
	}
	slices.Sort(tags)
	owners, ownersOK := tagOwnersOf(top)
	var base any
	if ownersOK {
		base = owners.m[baseTag]
	}
	if len(tags) > 0 && base == nil {
		return "", ErrNoTagOwner
	}
	var have, kept []any
	for _, r := range acls {
		if _, ok := groupSrc(r, prefix); ok {
			have = append(have, r)
		} else {
			kept = append(kept, r)
		}
	}
	wantRules := make([]any, 0, len(tags))
	for _, t := range tags {
		wantRules = append(wantRules, newObj("action", "accept", "src", []any{t}, "dst", sortedAny(want[t])))
	}
	if sameRules(have, wantRules) && sameOwners(owners, prefix, tags, base) {
		return policy, nil
	}
	setGroupOwners(top, owners, prefix, tags, base)
	if _, present := top.get("acls"); present || len(wantRules) > 0 {
		top.set("acls", append(kept, wantRules...))
	}
	var buf bytes.Buffer
	writeValue(&buf, top, 0)
	buf.WriteByte('\n')
	return buf.String(), nil
}

func policyParts(policy string) (*ordObj, []any, error) {
	root, err := parseOrdered(policy)
	if err != nil {
		return nil, nil, ErrNotPlainJSON
	}
	top, ok := root.(*ordObj)
	if !ok {
		return nil, nil, ErrPolicyShape
	}
	var acls []any
	if v, present := top.get("acls"); present {
		if acls, ok = v.([]any); !ok {
			return nil, nil, ErrPolicyShape
		}
	}
	if v, present := top.get("tagOwners"); present {
		if _, ok := v.(*ordObj); !ok {
			return nil, nil, ErrPolicyShape
		}
	}
	return top, acls, nil
}

func groupSrc(r any, prefix string) (string, bool) {
	o, ok := r.(*ordObj)
	if !ok {
		return "", false
	}
	s, ok := stringList(o.m["src"])
	if !ok || len(s) != 1 || !strings.HasPrefix(s[0], prefix) {
		return "", false
	}
	return s[0], true
}

func tagOwnersOf(top *ordObj) (*ordObj, bool) {
	v, present := top.get("tagOwners")
	if !present {
		return nil, false
	}
	o, ok := v.(*ordObj)
	return o, ok
}

func sameOwners(owners *ordObj, prefix string, tags []string, base any) bool {
	n := 0
	if owners != nil {
		for _, k := range owners.keys {
			if strings.HasPrefix(k, prefix) {
				n++
				if !slices.Contains(tags, k) || compact(owners.m[k]) != compact(base) {
					return false
				}
			}
		}
	}
	return n == len(tags)
}

func setGroupOwners(top, owners *ordObj, prefix string, tags []string, base any) {
	if owners == nil {
		if len(tags) == 0 {
			return
		}
		owners = newObj()
		top.set("tagOwners", owners)
	}
	for _, k := range slices.Clone(owners.keys) {
		if strings.HasPrefix(k, prefix) {
			owners.del(k)
		}
	}
	for _, t := range tags {
		owners.set(t, base)
	}
}

func sortedAny(in []string) []any {
	d := slices.Clone(in)
	slices.Sort(d)
	d = slices.Compact(d)
	out := make([]any, len(d))
	for i, x := range d {
		out[i] = x
	}
	return out
}
