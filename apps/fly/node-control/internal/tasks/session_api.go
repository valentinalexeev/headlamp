package tasks

import (
	"context"
	_ "embed"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/scrub"
)

//go:embed static/sessions.html
var sessionsHTML []byte

//go:embed static/sessions.js
var sessionsJS []byte

const (
	defaultSessionTTL  = 3600
	defaultSessionIdle = 900
	sessionNodeTimeout = 3 * time.Second
)

// hostLabelRe is a plain DNS label: the only form of a given name that goes
// into a link. Anything else falls back to the node's tailnet address.
var hostLabelRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// sessionAPI serves the browser-session page and API. It needs neither the
// store nor the engine, so it works while the tasks store is degraded.
type sessionAPI struct {
	m *Module
	c SessionClient
}

func (a *sessionAPI) register(mux *http.ServeMux, gate func(http.Handler) http.Handler) {
	plain := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(h)) }
	body := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(authz.RequireJSON(h))) }
	plain("GET /sessions", asset("text/html; charset=utf-8", sessionsHTML))
	plain("GET /sessions/static/sessions.js", asset("text/javascript; charset=utf-8", sessionsJS))
	plain("GET /sessions.json", a.list)
	body("POST /sessions/{node}/start", a.start)
	body("POST /sessions/{node}/stop", a.stop)
}

type sessionJSON struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	StartedAt string `json:"started_at,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	Port      int    `json:"port,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

type sessionNodeView struct {
	ID        string       `json:"id"`
	Name      string       `json:"name"`
	Available bool         `json:"available"`
	Session   *sessionJSON `json:"session"`
	URL       string       `json:"url,omitempty"`
}

// nodeID is how a node is addressed in routes and the page: its headscale
// given name (unique per headscale), the name only when there is none.
func nodeID(n headscale.Node) string {
	if n.GivenName != "" {
		return n.GivenName
	}
	return n.Name
}

// sessionURL is the link to the node's session. The host is the given name
// when it is a plain host label, else the node's tailnet IPv4; never anything
// the node could supply as a public address.
func sessionURL(n headscale.Node, port int, token string) string {
	host := ""
	if hostLabelRe.MatchString(n.GivenName) {
		host = n.GivenName
	} else if ip, ok := firstIPv4(n); ok {
		host = ip
	}
	if host == "" || token == "" || !validPort(port) {
		return ""
	}
	return "http://" + host + ":" + strconv.Itoa(port) + "/vnc.html?autoconnect=1&resize=remote&quality=9&path=websockify%3Ftoken%3D" + token
}

func buildSessionView(n headscale.Node, available bool, s *SessionInfo) sessionNodeView {
	v := sessionNodeView{ID: nodeID(n), Name: nodeID(n), Available: available}
	if s == nil {
		return v
	}
	j := &sessionJSON{ID: s.ID, State: s.State, Port: s.Port, Reason: s.Reason}
	if !s.StartedAt.IsZero() {
		j.StartedAt = s.StartedAt.UTC().Format(time.RFC3339)
	}
	if !s.ExpiresAt.IsZero() {
		j.ExpiresAt = s.ExpiresAt.UTC().Format(time.RFC3339)
	}
	v.Session = j
	if s.State == "running" {
		v.URL = sessionURL(n, s.Port, s.Token)
	}
	return v
}

// tagged returns the session-tagged nodes in a stable order.
func (a *sessionAPI) tagged(ctx context.Context) ([]headscale.Node, error) {
	all, err := a.m.src.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	nodes := WithTag(all, a.m.sessionsTag)
	sort.SliceStable(nodes, func(i, j int) bool {
		if ni, nj := nodeID(nodes[i]), nodeID(nodes[j]); ni != nj {
			return ni < nj
		}
		return nodes[i].Name < nodes[j].Name
	})
	return nodes, nil
}

// scrubbed removes the runner token and any session tokens from error text.
func (a *sessionAPI) scrubbed(err error, sessionTokens ...string) string {
	return scrub.Scrub(err.Error(), append([]string{a.m.token}, sessionTokens...))
}

func (a *sessionAPI) list(w http.ResponseWriter, r *http.Request) {
	nodes, err := a.tagged(r.Context())
	if err != nil {
		a.m.log.Warn("sessions: node list failed", "err", a.scrubbed(err))
		fail(w, http.StatusServiceUnavailable, "node list unavailable")
		return
	}
	views := make([]sessionNodeView, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		views[i] = buildSessionView(n, false, nil)
		addr, ok := firstIPv4(n)
		if !ok || !n.Online {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), sessionNodeTimeout)
			defer cancel()
			s, err := a.c.Current(ctx, addr)
			if err != nil {
				return // unreachable: shown as unavailable, never as "no session"
			}
			views[i] = buildSessionView(n, true, s)
		}()
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"nodes": views})
}

// node finds the tagged node named by the route; it answers 404 (nothing is
// contacted) for an untagged or unknown name and 409 for an ambiguous one.
func (a *sessionAPI) node(w http.ResponseWriter, r *http.Request) (headscale.Node, string, bool) {
	nodes, err := a.tagged(r.Context())
	if err != nil {
		a.m.log.Warn("sessions: node list failed", "err", a.scrubbed(err))
		fail(w, http.StatusServiceUnavailable, "node list unavailable")
		return headscale.Node{}, "", false
	}
	id := r.PathValue("node")
	var found []headscale.Node
	for _, n := range nodes {
		if nodeID(n) == id {
			found = append(found, n)
		}
	}
	switch {
	case len(found) == 0:
		fail(w, http.StatusNotFound, "no such node")
		return headscale.Node{}, "", false
	case len(found) > 1:
		fail(w, http.StatusConflict, "more than one node has this name")
		return headscale.Node{}, "", false
	}
	addr, ok := firstIPv4(found[0])
	if !ok || !found[0].Online {
		fail(w, http.StatusServiceUnavailable, "node unreachable")
		return headscale.Node{}, "", false
	}
	return found[0], addr, true
}

func (a *sessionAPI) start(w http.ResponseWriter, r *http.Request) {
	n, addr, ok := a.node(w, r)
	if !ok {
		return
	}
	var req struct {
		TTL  *int `json:"ttl_seconds"`
		Idle *int `json:"idle_seconds"`
	}
	if !readBody(w, r, 4<<10, &req, true) {
		return
	}
	ttl, idle := defaultSessionTTL, defaultSessionIdle
	if req.TTL != nil {
		ttl = *req.TTL
	}
	if req.Idle != nil {
		idle = *req.Idle
	}
	s, err := a.c.Start(r.Context(), addr, ttl, idle)
	switch {
	case err == nil:
	case errors.Is(err, ErrRunnerBusy):
		fail(w, http.StatusConflict, "the node is busy (a run or another session holds it)")
		return
	case errors.Is(err, ErrRunnerRejected):
		fail(w, http.StatusUnprocessableEntity, "the node refused the session settings")
		return
	case errors.Is(err, ErrRunUnknown):
		fail(w, http.StatusServiceUnavailable, "sessions are not enabled on this node")
		return
	default:
		a.m.log.Warn("session start failed", "node", nodeID(n), "err", a.scrubbed(err))
		fail(w, http.StatusServiceUnavailable, "the node could not be reached")
		return
	}
	a.m.log.Info("session started", "node", nodeID(n), "session", s.ID, "user", authz.LoginFrom(r.Context()), "ttl_seconds", ttl)
	writeJSON(w, http.StatusCreated, buildSessionView(n, true, &s))
}

func (a *sessionAPI) stop(w http.ResponseWriter, r *http.Request) {
	n, addr, ok := a.node(w, r)
	if !ok {
		return
	}
	cur, err := a.c.Current(r.Context(), addr)
	if err != nil {
		a.m.log.Warn("session stop failed", "node", nodeID(n), "err", a.scrubbed(err))
		fail(w, http.StatusServiceUnavailable, "the node could not be reached")
		return
	}
	// Nothing running (or already ended): stopping is a no-op.
	if cur == nil || cur.State != "running" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch err := a.c.Stop(r.Context(), addr, cur.ID); {
	case err == nil, errors.Is(err, ErrRunUnknown):
	default:
		a.m.log.Warn("session stop failed", "node", nodeID(n), "err", a.scrubbed(err, cur.Token))
		fail(w, http.StatusServiceUnavailable, "the node could not be reached")
		return
	}
	a.m.log.Info("session stopped", "node", nodeID(n), "user", authz.LoginFrom(r.Context()), "session", cur.ID)
	w.WriteHeader(http.StatusNoContent)
}
