package authz

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
)

// Feature is one area of the console. Every admin route belongs to exactly one.
type Feature string

// The features of the console (#122).
const (
	FeatureOverview Feature = "overview"
	FeatureNodes    Feature = "nodes"
	FeatureEnroll   Feature = "enroll"
	FeatureTasks    Feature = "tasks"
	FeaturePolicy   Feature = "policy"
	FeatureFunnel   Feature = "funnel"
	FeatureServe    Feature = "serve"
	FeatureDNS      Feature = "dns"
	FeatureAudit    Feature = "audit"
	// FeatureAdmin is the roles and grants page (#123). "*" does not include it: a viewer must not read who holds
	// what, and a "*.admin" role must not hand out roles. Name it explicitly ("admin.admin").
	FeatureAdmin Feature = "admin"
)

// Features lists every feature, in display order.
var Features = []Feature{FeatureOverview, FeatureNodes, FeatureEnroll, FeatureTasks, FeaturePolicy, FeatureFunnel, FeatureServe, FeatureDNS, FeatureAudit, FeatureAdmin}

// wildcard lists the features "*" stands for: every feature except admin.
func wildcard() []Feature {
	out := make([]Feature, 0, len(Features))
	for _, f := range Features {
		if f != FeatureAdmin {
			out = append(out, f)
		}
	}
	return out
}

// Level is how much a permission allows inside a feature. A higher level includes every lower one.
type Level int

// The levels: view reads, operate runs day-to-day actions, admin changes configuration.
const (
	LevelNone Level = iota
	LevelView
	LevelOperate
	LevelAdmin
)

func (l Level) String() string {
	switch l {
	case LevelView:
		return "view"
	case LevelOperate:
		return "operate"
	case LevelAdmin:
		return "admin"
	}
	return "none"
}

func parseLevel(s string) (Level, error) {
	switch s {
	case "view":
		return LevelView, nil
	case "operate":
		return LevelOperate, nil
	case "admin":
		return LevelAdmin, nil
	}
	return LevelNone, fmt.Errorf("unknown level %q (want view, operate or admin)", s)
}

// Perm is one permission: a feature at a level, written "nodes.operate".
type Perm struct {
	Feature Feature
	Level   Level
}

func (p Perm) String() string { return string(p.Feature) + "." + p.Level.String() }

// P builds a Perm; it is for route tables.
func P(f Feature, l Level) Perm { return Perm{f, l} }

// ParsePerm reads "feature.level", or "*.level" for every feature except admin.
func ParsePerm(s string) ([]Perm, error) {
	f, l, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		return nil, fmt.Errorf("permission %q: want feature.level", s)
	}
	lv, err := parseLevel(l)
	if err != nil {
		return nil, fmt.Errorf("permission %q: %w", s, err)
	}
	if f == "*" {
		out := make([]Perm, 0, len(Features))
		for _, ft := range wildcard() {
			out = append(out, Perm{ft, lv})
		}
		return out, nil
	}
	if !slices.Contains(Features, Feature(f)) {
		return nil, fmt.Errorf("permission %q: unknown feature %q", s, f)
	}
	return []Perm{{Feature(f), lv}}, nil
}

// Role is a named set of permissions, kept as the highest level per feature.
type Role map[Feature]Level

// Grants reports whether r includes p.
func (r Role) Grants(p Perm) bool { return r[p.Feature] >= p.Level }

func (r Role) add(p Perm) {
	if p.Level > r[p.Feature] {
		r[p.Feature] = p.Level
	}
}

func roleOf(perms ...string) Role {
	r := Role{}
	for _, s := range perms {
		ps, err := ParsePerm(s)
		if err != nil {
			panic(err)
		}
		for _, p := range ps {
			r.add(p)
		}
	}
	return r
}

// BuiltinRoles are always defined. A config file may not redefine them.
var BuiltinRoles = map[string]Role{
	"viewer":   roleOf("*.view"),
	"operator": roleOf("*.view", "nodes.operate", "tasks.operate", "enroll.operate"),
	"owner":    roleOf("*.admin", "admin.admin"),
}

// Policy maps users to roles. The zero value grants nothing.
type Policy struct {
	mu     sync.RWMutex
	roles  map[string]Role
	grants map[string][]string // subject -> role names
	// dyn is the layer of roles and grants stored by the UI (#123). It adds to the file grants above, which
	// the UI cannot change: they are the break-glass owners.
	dynRoles  map[string]Role
	dynGrants map[string][]string
	dynCfg    Config
}

// Config is the ADMIN_CONFIG file: custom roles and grants. A subject is a tailnet login ("alice@" or
// "alice@example.com"); with OIDC (#102) a subject may also be "group:<name>".
type Config struct {
	Roles  map[string][]string `json:"roles,omitempty"`
	Grants []Grant             `json:"grants"`
}

// Grant gives the named roles to one subject.
type Grant struct {
	Subject string   `json:"subject"`
	Roles   []string `json:"roles"`
}

// NewPolicy checks cfg and builds a Policy. Every error names the field, so the operator can fix the file.
func NewPolicy(cfg Config) (*Policy, error) {
	p := &Policy{roles: map[string]Role{}, grants: map[string][]string{}}
	for n, r := range BuiltinRoles {
		p.roles[n] = r
	}
	names := make([]string, 0, len(cfg.Roles))
	for n := range cfg.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if _, ok := BuiltinRoles[n]; ok {
			return nil, fmt.Errorf("roles.%s: a built-in role cannot be redefined", n)
		}
		if n == "" || strings.ContainsAny(n, " ,") {
			return nil, fmt.Errorf("roles: invalid role name %q", n)
		}
		r := Role{}
		if len(cfg.Roles[n]) == 0 {
			return nil, fmt.Errorf("roles.%s: no permissions", n)
		}
		for _, s := range cfg.Roles[n] {
			ps, err := ParsePerm(s)
			if err != nil {
				return nil, fmt.Errorf("roles.%s: %w", n, err)
			}
			for _, pm := range ps {
				r.add(pm)
			}
		}
		p.roles[n] = r
	}
	for i, g := range cfg.Grants {
		sub := normalizeSubject(g.Subject)
		if sub == "" {
			return nil, fmt.Errorf("grants[%d]: empty subject", i)
		}
		if len(g.Roles) == 0 {
			return nil, fmt.Errorf("grants[%d] (%s): no roles", i, sub)
		}
		for _, rn := range g.Roles {
			if _, ok := p.roles[rn]; !ok {
				return nil, fmt.Errorf("grants[%d] (%s): unknown role %q", i, sub, rn)
			}
			if !slices.Contains(p.grants[sub], rn) {
				p.grants[sub] = append(p.grants[sub], rn)
			}
		}
	}
	return p, nil
}

// LoadPolicy reads and checks an ADMIN_CONFIG file. Unknown fields are refused.
func LoadPolicy(path string) (*Policy, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("ADMIN_CONFIG: %w", err)
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	var cfg Config
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("ADMIN_CONFIG: %w", err)
	}
	p, err := NewPolicy(cfg)
	if err != nil {
		return nil, fmt.Errorf("ADMIN_CONFIG: %w", err)
	}
	return p, nil
}

func normalizeSubject(s string) string {
	s = strings.TrimSpace(s)
	if g, ok := strings.CutPrefix(s, "group:"); ok {
		return "group:" + strings.TrimSpace(g)
	}
	return NormalizeLogin(s)
}

// AddLegacy grants one feature at admin level to each login in a legacy *_ADMIN_USERS list, under a role named
// "legacy-<feature>". It keeps those lists working when ADMIN_CONFIG is not set.
func (p *Policy) AddLegacy(f Feature, logins Allowed) {
	p.mu.Lock()
	defer p.mu.Unlock()
	name := "legacy-" + string(f)
	p.roles[name] = Role{f: LevelAdmin}
	for sub := range logins.set {
		if !slices.Contains(p.grants[sub], name) {
			p.grants[sub] = append(p.grants[sub], name)
		}
	}
}

// AddOwners grants the built-in owner role to each login (ADMIN_USERS).
func (p *Policy) AddOwners(logins Allowed) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sub := range logins.set {
		if !slices.Contains(p.grants[sub], "owner") {
			p.grants[sub] = append(p.grants[sub], "owner")
		}
	}
}

// Subject is who is asking: a tailnet login and, with OIDC, the groups of the session.
type Subject struct {
	Login  string
	Groups []string
}

// Role returns the merged role of s: the union of every role granted to the login or one of its groups.
func (p *Policy) Role(s Subject) Role {
	out := Role{}
	if p == nil {
		return out
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	keys := []string{NormalizeLogin(s.Login)}
	for _, g := range s.Groups {
		keys = append(keys, "group:"+strings.TrimSpace(g))
	}
	for _, k := range keys {
		if k == "" || k == "group:" {
			continue
		}
		for _, rn := range p.grants[k] {
			for f, l := range p.roles[rn] {
				out.add(Perm{f, l})
			}
		}
		for _, rn := range p.dynGrants[k] {
			for f, l := range p.lookupRole(rn) {
				out.add(Perm{f, l})
			}
		}
	}
	return out
}

// Can reports whether s holds perm.
func (p *Policy) Can(s Subject, perm Perm) bool { return p.Role(s).Grants(perm) }

// Empty reports whether the policy grants anything to anyone.
func (p *Policy) Empty() bool {
	if p == nil {
		return true
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return len(p.grants) == 0 && len(p.dynGrants) == 0
}
