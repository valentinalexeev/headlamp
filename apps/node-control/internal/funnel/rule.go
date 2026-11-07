package funnel

import (
	"bytes"
	"slices"
)

// ComposeRule returns policy with the one rule whose src is exactly [src] set to accept dst (#132: the
// bridge-to-tailnet entry, src "tag:ingress"). An empty dst removes that rule. Every other rule, and the
// key order of the document, is left alone; when the rule already matches, the original text comes back.
// The caller must pick a src no other generator manages (funnel manages its own ingress tag).
func ComposeRule(policy, src string, dst []string) (string, error) {
	root, err := parseOrdered(policy)
	if err != nil {
		return "", ErrNotPlainJSON
	}
	top, ok := root.(*ordObj)
	if !ok {
		return "", ErrPolicyShape
	}
	var acls []any
	aclsPresent := false
	if v, present := top.get("acls"); present {
		aclsPresent = true
		if acls, ok = v.([]any); !ok {
			return "", ErrPolicyShape
		}
	}
	mine := func(r any) bool {
		o, ok := r.(*ordObj)
		if !ok {
			return false
		}
		s, ok := stringList(o.m["src"])
		return ok && len(s) == 1 && s[0] == src
	}
	var have, kept []any
	for _, r := range acls {
		if mine(r) {
			have = append(have, r)
		} else {
			kept = append(kept, r)
		}
	}
	var want []any
	if len(dst) > 0 {
		d := slices.Clone(dst)
		slices.Sort(d)
		ds := make([]any, len(d))
		for i, x := range d {
			ds[i] = x
		}
		want = []any{newObj("action", "accept", "src", []any{src}, "dst", ds)}
	}
	if sameRules(have, want) {
		return policy, nil
	}
	if len(want) > 0 || aclsPresent {
		top.set("acls", append(kept, want...))
	}
	var buf bytes.Buffer
	writeValue(&buf, top, 0)
	buf.WriteByte('\n')
	return buf.String(), nil
}
