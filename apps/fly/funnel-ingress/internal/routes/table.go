// Package routes is the route table of funnel-ingress: static routes (from the configuration source),
// dynamic routes (registered by tailnet nodes and persisted), and blocked keys.
package routes

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"sync"
)

// Key names a service inside a namespace: it is served at <Service>.<NS>.<public domain>.
type Key struct {
	NS      string `json:"ns"`
	Service string `json:"service"`
}

// Kind tells where a route came from.
type Kind string

const (
	// Static routes come from the configuration source and are read-only to the API.
	Static Kind = "static"
	// Dynamic routes are registered by a node through the API.
	Dynamic Kind = "dynamic"
)

// Route is one entry of the table.
type Route struct {
	Key
	Target             string `json:"target"` // IP or MagicDNS name; dynamic routes always store the owner's tailnet IP
	Port               int    `json:"port"`
	Scheme             string `json:"scheme"` // "http" (default) or "https"
	InsecureSkipVerify bool   `json:"insecure_skip_verify,omitempty"`
	NodeID             string `json:"node_id,omitempty"` // owner's stable ID; empty for static routes
	Kind               Kind   `json:"-"`
}

// Errors returned by the table.
var (
	ErrStaticConflict = errors.New("a static route owns this name")
	ErrOwnedByOther   = errors.New("this name is owned by another node")
	ErrLimit          = errors.New("route limit reached for this node")
	ErrBadName        = errors.New("invalid namespace, service, port or scheme")
	ErrBlocked        = errors.New("this name is blocked")
	// ErrPersist is returned when a mutation could not be saved: nothing changed.
	ErrPersist = errors.New("could not persist the routes")
)

// Persister stores the dynamic routes as one blob. The table calls Save only when the set changed.
type Persister interface {
	Load() ([]byte, bool, error)
	Save([]byte) error
}

var nameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidName reports whether s is usable as a namespace or a service name (one DNS label).
func ValidName(s string) bool { return nameRE.MatchString(s) }

// Validate checks the fields shared by static and dynamic routes and fills the default scheme.
func (r *Route) Validate() error {
	if !ValidName(r.NS) || !ValidName(r.Service) || r.Port < 1 || r.Port > 65535 || r.Target == "" {
		return ErrBadName
	}
	switch r.Scheme {
	case "":
		r.Scheme = "http"
	case "http", "https":
	default:
		return ErrBadName
	}
	return nil
}

// Table is safe for concurrent use.
type Table struct {
	mu      sync.RWMutex
	p       Persister
	max     int
	static  map[Key]Route
	dynamic map[Key]Route
	blocked map[Key]struct{}
}

const blobVersion = 1

type blob struct {
	Version int     `json:"version"`
	Routes  []Route `json:"routes"`
}

// NewTable loads the persisted dynamic routes. A blob that cannot be read or parsed is an error and is never
// overwritten: starting empty and saving would destroy the only copy. Static and blocked sets start empty.
func NewTable(p Persister, maxPerNode int) (*Table, error) {
	t := &Table{p: p, max: maxPerNode, static: map[Key]Route{}, dynamic: map[Key]Route{}, blocked: map[Key]struct{}{}}
	raw, ok, err := p.Load()
	if err != nil {
		return nil, fmt.Errorf("loading the persisted routes: %w", err)
	}
	if !ok {
		return t, nil
	}
	var b blob
	if err := json.Unmarshal(raw, &b); err != nil || b.Version != blobVersion {
		return nil, errors.New("the persisted routes are unreadable; fix or delete them (they are not overwritten)")
	}
	for _, r := range b.Routes {
		r.Kind = Dynamic
		if err := r.Validate(); err != nil || r.NodeID == "" {
			return nil, errors.New("the persisted routes contain an invalid entry; fix or delete them (they are not overwritten)")
		}
		if _, dup := t.dynamic[r.Key]; dup {
			return nil, errors.New("the persisted routes contain a duplicate entry; fix or delete them (they are not overwritten)")
		}
		t.dynamic[r.Key] = r
	}
	return t, nil
}

func sortKeys(ks []Key) {
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].NS != ks[j].NS {
			return ks[i].NS < ks[j].NS
		}
		return ks[i].Service < ks[j].Service
	})
}

func sortRoutes(rs []Route) {
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].NS != rs[j].NS {
			return rs[i].NS < rs[j].NS
		}
		return rs[i].Service < rs[j].Service
	})
}

// save persists next (the complete dynamic set). The caller holds the write lock and swaps next in on nil.
func (t *Table) save(next map[Key]Route) error {
	rs := make([]Route, 0, len(next))
	for _, r := range next {
		rs = append(rs, r)
	}
	sortRoutes(rs)
	raw, err := json.Marshal(blob{Version: blobVersion, Routes: rs})
	if err != nil {
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	if err := t.p.Save(raw); err != nil {
		return fmt.Errorf("%w: %v", ErrPersist, err)
	}
	return nil
}

func clone(m map[Key]Route) map[Key]Route {
	c := make(map[Key]Route, len(m)+1)
	for k, v := range m {
		c[k] = v
	}
	return c
}

// SetStatic atomically replaces the static set and returns the keys of dynamic routes dropped because a
// static route now owns the key. Callers validate the routes first (invalid entries are skipped here).
// Nothing is saved: the configuration source owns the static set, and the next dynamic mutation persists the
// pruned dynamic set; until then a restart re-applies the cached configuration before serving, which drops
// the route again.
func (t *Table) SetStatic(rs []Route) []Key {
	next := make(map[Key]Route, len(rs))
	for _, r := range rs {
		r.Kind = Static
		r.NodeID = ""
		if r.Validate() != nil {
			continue
		}
		next[r.Key] = r
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.static = next
	var dropped []Key
	for k := range t.dynamic {
		if _, ok := next[k]; ok {
			dropped = append(dropped, k)
		}
	}
	for _, k := range dropped {
		delete(t.dynamic, k)
	}
	sortKeys(dropped)
	return dropped
}

// SetBlocked atomically replaces the blocked set and returns the dynamic routes removed because of it.
// Like SetStatic it saves nothing.
func (t *Table) SetBlocked(ks []Key) []Key {
	next := make(map[Key]struct{}, len(ks))
	for _, k := range ks {
		next[k] = struct{}{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.blocked = next
	var removed []Key
	for k := range t.dynamic {
		if _, ok := next[k]; ok {
			removed = append(removed, k)
		}
	}
	for _, k := range removed {
		delete(t.dynamic, k)
	}
	sortKeys(removed)
	return removed
}

// Blocked lists the blocked keys, sorted.
func (t *Table) Blocked() []Key {
	t.mu.RLock()
	defer t.mu.RUnlock()
	ks := make([]Key, 0, len(t.blocked))
	for k := range t.blocked {
		ks = append(ks, k)
	}
	sortKeys(ks)
	return ks
}

// Lookup returns the route for k. A static route wins over a dynamic one.
func (t *Table) Lookup(k Key) (Route, bool) {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if r, ok := t.static[k]; ok {
		return r, true
	}
	r, ok := t.dynamic[k]
	return r, ok
}

// Put registers or updates a dynamic route. It is idempotent for the same owner and saves nothing when the
// stored route would not change.
func (t *Table) Put(r Route) error {
	r.Kind = Dynamic
	if err := r.Validate(); err != nil || r.NodeID == "" {
		return ErrBadName
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.blocked[r.Key]; ok {
		return ErrBlocked
	}
	if _, ok := t.static[r.Key]; ok {
		return ErrStaticConflict
	}
	cur, exists := t.dynamic[r.Key]
	if exists && cur.NodeID != r.NodeID {
		return ErrOwnedByOther
	}
	if exists && cur == r {
		return nil
	}
	if !exists {
		n := 0
		for _, o := range t.dynamic {
			if o.NodeID == r.NodeID {
				n++
			}
		}
		if n >= t.max {
			return ErrLimit
		}
	}
	next := clone(t.dynamic)
	next[r.Key] = r
	if err := t.save(next); err != nil {
		return err
	}
	t.dynamic = next
	return nil
}

// Delete removes a dynamic route owned by nodeID. Deleting an absent key is not an error.
func (t *Table) Delete(k Key, nodeID string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	cur, ok := t.dynamic[k]
	if !ok {
		return nil
	}
	if cur.NodeID != nodeID {
		return ErrOwnedByOther
	}
	next := clone(t.dynamic)
	delete(next, k)
	if err := t.save(next); err != nil {
		return err
	}
	t.dynamic = next
	return nil
}

// ByNode lists the dynamic routes owned by nodeID, sorted.
func (t *Table) ByNode(nodeID string) []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	var rs []Route
	for _, r := range t.dynamic {
		if r.NodeID == nodeID {
			rs = append(rs, r)
		}
	}
	sortRoutes(rs)
	return rs
}

// RemoveNode removes every dynamic route of nodeID with one save and returns their keys. When the save
// fails nothing is removed and the error is returned; the caller (the reconcile loop) retries next tick.
func (t *Table) RemoveNode(nodeID string) ([]Key, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	next := clone(t.dynamic)
	var removed []Key
	for k, r := range t.dynamic {
		if r.NodeID == nodeID {
			delete(next, k)
			removed = append(removed, k)
		}
	}
	if len(removed) == 0 {
		return nil, nil
	}
	if err := t.save(next); err != nil {
		return nil, err
	}
	t.dynamic = next
	sortKeys(removed)
	return removed, nil
}

// NodeIDs lists the distinct owners of dynamic routes, sorted.
func (t *Table) NodeIDs() []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	seen := map[string]struct{}{}
	for _, r := range t.dynamic {
		seen[r.NodeID] = struct{}{}
	}
	ids := make([]string, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// All lists the static routes, then the dynamic ones, each group sorted. A dynamic route shadowed by a static
// one cannot exist (SetStatic drops it).
func (t *Table) All() []Route {
	t.mu.RLock()
	defer t.mu.RUnlock()
	st := make([]Route, 0, len(t.static))
	for _, r := range t.static {
		st = append(st, r)
	}
	dy := make([]Route, 0, len(t.dynamic))
	for _, r := range t.dynamic {
		dy = append(dy, r)
	}
	sortRoutes(st)
	sortRoutes(dy)
	return append(st, dy...)
}
