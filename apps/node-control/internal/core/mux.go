package core

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
)

// Authorizer decides who may use an admin route. Wrap returns the route's handler behind its permission check, or an
// error when the route has no permission: then the server refuses to start, so no admin route can ship without a gate.
type Authorizer interface {
	Wrap(pattern string, h http.Handler) (http.Handler, error)
}

// Mux is the admin router. It is an http.ServeMux that remembers every pattern, so that Seal can put each route
// behind the Authorizer. Before Seal (in module tests) routes are served as registered.
type Mux struct {
	*http.ServeMux
	mu       sync.RWMutex
	handlers map[string]http.Handler
	gated    map[string]http.Handler // nil until Seal
}

// NewMux returns an empty admin router.
func NewMux() *Mux { return &Mux{ServeMux: http.NewServeMux(), handlers: map[string]http.Handler{}} }

// Handle registers h for pattern, like http.ServeMux.Handle.
func (m *Mux) Handle(pattern string, h http.Handler) {
	m.mu.Lock()
	m.handlers[pattern] = h
	m.mu.Unlock()
	m.ServeMux.Handle(pattern, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		g, sealed := m.gated[pattern], m.gated != nil
		m.mu.RUnlock()
		switch {
		case !sealed:
			h.ServeHTTP(w, r)
		case g == nil: // registered after Seal: never reachable without a check
			w.WriteHeader(http.StatusForbidden)
		default:
			g.ServeHTTP(w, r)
		}
	}))
}

// HandleFunc registers f for pattern, like http.ServeMux.HandleFunc.
func (m *Mux) HandleFunc(pattern string, f func(http.ResponseWriter, *http.Request)) {
	m.Handle(pattern, http.HandlerFunc(f))
}

// Patterns returns every registered pattern, sorted.
func (m *Mux) Patterns() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]string, 0, len(m.handlers))
	for p := range m.handlers {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// RegisterCoreRoutes adds the routes the core owns (health, attention, activity, search) after every module has
// registered, and returns the collected navigation. Run calls it; a test can call it to see the full route set.
func RegisterCoreRoutes(admin *Mux, mods []Module, tn Tailnet) ([]NavItem, []NavAction, error) {
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	navItems, err := CollectNav(mods)
	if err != nil {
		return nil, nil, err
	}
	navActions, err := CollectActions(mods)
	if err != nil {
		return nil, nil, err
	}
	registerAttention(admin, mods, navItems, tn)
	return navItems, navActions, nil
}

// Seal puts every route behind a. It fails, listing them, if any route has no permission.
func (m *Mux) Seal(a Authorizer) error {
	gated := map[string]http.Handler{}
	var missing []string
	for _, p := range m.Patterns() {
		m.mu.RLock()
		h := m.handlers[p]
		m.mu.RUnlock()
		g, err := a.Wrap(p, h)
		if err != nil {
			missing = append(missing, p)
			continue
		}
		gated[p] = g
	}
	if len(missing) > 0 {
		return fmt.Errorf("admin routes without a permission: %v", missing)
	}
	m.mu.Lock()
	m.gated = gated
	m.mu.Unlock()
	return nil
}
