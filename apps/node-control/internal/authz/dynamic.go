package authz

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// The stored layer of roles and grants (#123). It is managed in the UI and kept in an admin store; it adds to the
// file layer (ADMIN_CONFIG, ADMIN_USERS and the legacy lists), which it can never change or remove. Every grant in
// the file layer is a break-glass grant: an operator who removes every stored grant still has the file's owners.

// legacyPrefix names the roles AddLegacy makes. They are internal: a stored config may not define or grant them.
const legacyPrefix = "legacy-"

// lookupRole finds a role by name in the file layer, then in the stored layer. The caller holds p.mu.
func (p *Policy) lookupRole(name string) Role {
	if r, ok := p.roles[name]; ok {
		return r
	}
	return p.dynRoles[name]
}

// SetDynamic validates cfg as the stored layer and replaces the current one. A stored role may not reuse the name of
// a built-in role, a file role or a legacy role. A stored grant may name a built-in, file or stored role. On error the
// current layer is kept.
func (p *Policy) SetDynamic(cfg Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	roles, grants, err := p.checkDynamic(cfg)
	if err != nil {
		return err
	}
	p.dynRoles, p.dynGrants, p.dynCfg = roles, grants, Normalize(cfg)
	return nil
}

// CheckDynamic validates cfg as a stored layer without applying it.
func (p *Policy) CheckDynamic(cfg Config) error {
	p.mu.RLock()
	defer p.mu.RUnlock()
	_, _, err := p.checkDynamic(cfg)
	return err
}

func (p *Policy) checkDynamic(cfg Config) (map[string]Role, map[string][]string, error) {
	roles := map[string]Role{}
	for _, n := range sortedKeys(cfg.Roles) {
		switch {
		case BuiltinRoles[n] != nil:
			return nil, nil, fmt.Errorf("roles.%s: a built-in role cannot be redefined", n)
		case strings.HasPrefix(n, legacyPrefix):
			return nil, nil, fmt.Errorf("roles.%s: the prefix %q is reserved", n, legacyPrefix)
		case p.roles[n] != nil:
			return nil, nil, fmt.Errorf("roles.%s: the role is defined in ADMIN_CONFIG and cannot be changed here", n)
		}
		r, err := parseRole(n, cfg.Roles[n])
		if err != nil {
			return nil, nil, err
		}
		roles[n] = r
	}
	grants := map[string][]string{}
	for i, g := range cfg.Grants {
		sub := normalizeSubject(g.Subject)
		if sub == "" || sub == "group:" {
			return nil, nil, fmt.Errorf("grants[%d]: empty subject", i)
		}
		if len(g.Roles) == 0 {
			return nil, nil, fmt.Errorf("grants[%d] (%s): no roles", i, sub)
		}
		for _, rn := range g.Roles {
			known := roles[rn] != nil || (p.roles[rn] != nil && !strings.HasPrefix(rn, legacyPrefix))
			if !known {
				return nil, nil, fmt.Errorf("grants[%d] (%s): unknown role %q", i, sub, rn)
			}
			if !slices.Contains(grants[sub], rn) {
				grants[sub] = append(grants[sub], rn)
			}
		}
	}
	return roles, grants, nil
}

func parseRole(name string, perms []string) (Role, error) {
	if name == "" || strings.ContainsAny(name, " ,") || len(name) > 64 {
		return nil, fmt.Errorf("roles: invalid role name %q", name)
	}
	if len(perms) == 0 {
		return nil, fmt.Errorf("roles.%s: no permissions", name)
	}
	r := Role{}
	for _, s := range perms {
		ps, err := ParsePerm(s)
		if err != nil {
			return nil, fmt.Errorf("roles.%s: %w", name, err)
		}
		for _, pm := range ps {
			r.add(pm)
		}
	}
	return r, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Normalize returns cfg with subjects normalised, grants merged per subject and sorted, so it compares and exports
// the same way every time.
func Normalize(cfg Config) Config {
	out := Config{Roles: map[string][]string{}}
	for n, ps := range cfg.Roles {
		out.Roles[n] = slices.Clone(ps)
	}
	by := map[string][]string{}
	for _, g := range cfg.Grants {
		sub := normalizeSubject(g.Subject)
		for _, rn := range g.Roles {
			if !slices.Contains(by[sub], rn) {
				by[sub] = append(by[sub], rn)
			}
		}
	}
	for _, sub := range sortedKeys(by) {
		rs := by[sub]
		sort.Strings(rs)
		out.Grants = append(out.Grants, Grant{Subject: sub, Roles: rs})
	}
	if out.Grants == nil {
		out.Grants = []Grant{}
	}
	return out
}

// Dynamic returns a copy of the stored layer.
func (p *Policy) Dynamic() Config {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return Normalize(p.dynCfg)
}

// RoleNamed returns a role of either layer by name.
func (p *Policy) RoleNamed(name string) (Role, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	r := p.lookupRole(name)
	return r, r != nil
}

// Covers reports whether have includes every permission of want: the no-escalation rule of #123.
func Covers(have, want Role) bool {
	for f, l := range want {
		if have[f] < l {
			return false
		}
	}
	return true
}

// Perms lists a role as "feature.level" strings, in feature order.
func (r Role) Perms() []string {
	var out []string
	for _, f := range Features {
		if l := r[f]; l > LevelNone {
			out = append(out, Perm{f, l}.String())
		}
	}
	return out
}

// AnyoneCanIn reports whether p grants perm to at least one subject, in either layer.
func AnyoneCanIn(p *Policy, perm Perm) bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	for _, layer := range []map[string][]string{p.grants, p.dynGrants} {
		for _, roles := range layer {
			for _, rn := range roles {
				if p.lookupRole(rn).Grants(perm) {
					return true
				}
			}
		}
	}
	return false
}

// RoleInfo is one role as the roles page shows it. Source is "builtin", "file" or "store".
type RoleInfo struct {
	Name   string   `json:"name"`
	Perms  []string `json:"perms"`
	Source string   `json:"source"`
}

// Description is the whole policy as the roles page shows it: every role, the locked file grants and the stored
// grants.
type Description struct {
	Roles        []RoleInfo `json:"roles"`
	FileGrants   []Grant    `json:"file_grants"`
	StoredGrants []Grant    `json:"stored_grants"`
}

// Describe returns the policy for display. Legacy feature roles are shown by their name, as file roles.
func (p *Policy) Describe() Description {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var d Description
	for _, n := range sortedKeys(p.roles) {
		src := "file"
		if BuiltinRoles[n] != nil {
			src = "builtin"
		}
		d.Roles = append(d.Roles, RoleInfo{Name: n, Perms: p.roles[n].Perms(), Source: src})
	}
	for _, n := range sortedKeys(p.dynRoles) {
		d.Roles = append(d.Roles, RoleInfo{Name: n, Perms: p.dynRoles[n].Perms(), Source: "store"})
	}
	toGrants := func(m map[string][]string) []Grant {
		out := []Grant{}
		for _, sub := range sortedKeys(m) {
			rs := slices.Clone(m[sub])
			sort.Strings(rs)
			out = append(out, Grant{Subject: sub, Roles: rs})
		}
		return out
	}
	d.FileGrants, d.StoredGrants = toGrants(p.grants), toGrants(p.dynGrants)
	return d
}
