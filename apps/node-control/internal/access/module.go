package access

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

//go:embed static/access.html
var pageHTML []byte

//go:embed static/access.js
var pageJS []byte

// pollEvery is how often an instance checks the stored version, so a change on another instance applies without a
// restart.
const pollEvery = time.Second

// Module is the roles and grants page. It is on when ADMIN_DB is set, or when REDIS_URL is set (the store then shares
// the Redis of the tailnet state, under ADMIN_REDIS_KEY, default "<app>:admin").
type Module struct {
	env core.Env
	log *slog.Logger
	now func() time.Time

	st  Store
	pol *authz.Policy

	mu      sync.Mutex // serialises writes from this instance
	version int64
	stop    chan struct{}
	stopped sync.Once
}

// New returns the module. It reads the process-wide policy (authz.Default) at Register.
func New(env core.Env) core.Module { return &Module{env: env, now: time.Now} }

func (m *Module) Name() string { return "access" }

func redisPrefix(env core.Env) string {
	if k := env.Getenv("ADMIN_REDIS_KEY"); k != "" {
		return k
	}
	app := env.Getenv("FLY_APP_NAME")
	if app == "" {
		app = env.Default("TAILNET_HOSTNAME", "node-control")
	}
	return app + ":admin"
}

func openStore(env core.Env) (Store, error) {
	if path := env.Getenv("ADMIN_DB"); path != "" {
		return OpenSQLite(path)
	}
	if url := env.Getenv("REDIS_URL"); url != "" {
		rdb, _, err := statestore.OpenClient(url)
		if err != nil {
			return nil, fmt.Errorf("admin store: %w", err)
		}
		s, err := NewRedisStore(rdb, redisPrefix(env))
		if err != nil {
			rdb.Close()
		}
		return s, err
	}
	return nil, nil
}

func (m *Module) Register(r *core.Routes) error {
	m.log = r.Log
	if m.log == nil {
		m.log = slog.Default()
	}
	// main installs the process-wide policy before Register. Without one (tests, tools), a private empty policy keeps
	// the module self-contained; the process-wide one is never replaced here.
	m.pol = authz.Default()
	if m.pol == nil {
		m.pol, _ = authz.NewPolicy(authz.Config{})
	}
	st, err := openStore(m.env)
	if err != nil || st == nil {
		return err
	}
	m.st = st
	if err := m.sync(); err != nil {
		st.Close()
		m.st = nil
		return fmt.Errorf("admin store: the stored roles and grants do not load: %w", err)
	}
	m.stop = make(chan struct{})

	view := authz.Gate(r.Tailnet, authz.P(authz.FeatureAdmin, authz.LevelView), authz.Allowed{}, m.log)
	admin := authz.Gate(r.Tailnet, authz.P(authz.FeatureAdmin, authz.LevelAdmin), authz.Allowed{}, m.log)
	audit := authz.Gate(r.Tailnet, authz.P(authz.FeatureAudit, authz.LevelView), authz.Allowed{}, m.log)
	j := func(h http.HandlerFunc) http.Handler { return authz.RequireJSON(h) }
	r.Admin.Handle("GET /admin", view(asset("text/html; charset=utf-8", pageHTML)))
	r.Admin.Handle("GET /admin/static/access.js", view(asset("text/javascript; charset=utf-8", pageJS)))
	r.Admin.Handle("GET /admin/state", view(http.HandlerFunc(m.state)))
	r.Admin.Handle("GET /admin/export", view(http.HandlerFunc(m.export)))
	r.Admin.Handle("GET /admin/audit", audit(http.HandlerFunc(m.auditLog)))
	r.Admin.Handle("POST /admin/grants", admin(j(m.setGrant)))
	r.Admin.Handle("PUT /admin/roles/{name}", admin(j(m.putRole)))
	r.Admin.Handle("DELETE /admin/roles/{name}", admin(http.HandlerFunc(m.deleteRole)))
	r.Admin.Handle("POST /admin/import", admin(j(m.importCfg)))
	return nil
}

// Start polls the stored version, so a change made on another instance applies here within pollEvery.
func (m *Module) Start(ctx context.Context) error {
	if m.st == nil {
		return nil
	}
	go func() {
		t := time.NewTicker(pollEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stop:
				return
			case <-t.C:
				if err := m.sync(); err != nil {
					m.log.Warn("access: reload of roles and grants failed, the last good set stays", "error", err)
				}
			}
		}
	}()
	return nil
}

func (m *Module) Stop(context.Context) error {
	if m.st == nil {
		return nil
	}
	m.stopped.Do(func() {
		close(m.stop)
		m.st.Close()
	})
	return nil
}

// sync loads the stored layer when its version moved.
func (m *Module) sync() error {
	v, err := m.st.Version()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if v == m.version && v != 0 {
		return nil
	}
	cfg, v, err := m.st.Load()
	if err != nil {
		return err
	}
	if err := m.pol.SetDynamic(cfg); err != nil {
		return err
	}
	m.version = v
	return nil
}

// Nav shows the page to who may view it.
func (m *Module) Nav() []core.NavItem {
	if m.st == nil {
		return nil
	}
	return []core.NavItem{{Label: "Access", Path: "/admin", Order: 90, Group: "configure", Icon: "key",
		Allow: func(l string) bool {
			return m.pol.Can(authz.Subject{Login: l}, authz.P(authz.FeatureAdmin, authz.LevelView))
		}}}
}

// HasActivity reports whether the module keeps an audit log: it does when it is on.
func (m *Module) HasActivity() bool { return m.st != nil }

// Activity lists role and grant changes for the console's activity page, to who may view the audit.
func (m *Module) Activity(_ context.Context, login string, limit int) ([]core.ActivityEntry, error) {
	if m.st == nil {
		return nil, nil
	}
	if !m.pol.Can(authz.Subject{Login: login}, authz.P(authz.FeatureAudit, authz.LevelView)) {
		return nil, core.ErrNotVisible
	}
	es, err := m.st.Audit(limit)
	if err != nil {
		return nil, err
	}
	out := make([]core.ActivityEntry, 0, len(es))
	for _, e := range es {
		d := strings.TrimSpace(e.Target + " " + e.Detail)
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "access", Action: e.Action, Detail: d})
	}
	return out, nil
}

// --- handlers

type httpErr struct {
	code int
	msg  string
}

func (e httpErr) Error() string { return e.msg }

func fail(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	var he httpErr
	switch {
	case errors.As(err, &he):
		code = he.code
	case errors.Is(err, ErrConflict):
		code = http.StatusConflict
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": err.Error()})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}

func asset(contentType string, body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", ui.PageCSP)
		_, _ = w.Write(body)
	})
}

func readJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return httpErr{http.StatusBadRequest, "invalid JSON: " + err.Error()}
	}
	return nil
}

func (m *Module) actor(r *http.Request) (string, authz.Role) {
	l := authz.LoginFrom(r.Context())
	return l, m.pol.Role(authz.Subject{Login: l})
}

func (m *Module) state(w http.ResponseWriter, r *http.Request) {
	me, role := m.actor(r)
	d := m.pol.Describe()
	m.mu.Lock()
	v := m.version
	m.mu.Unlock()
	writeJSON(w, map[string]any{
		"version": v, "roles": d.Roles, "file_grants": d.FileGrants, "stored_grants": d.StoredGrants,
		"me": me, "my_perms": role.Perms(), "can_admin": role.Grants(authz.P(authz.FeatureAdmin, authz.LevelAdmin)),
		"features": authz.Features,
	})
}

func (m *Module) export(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Disposition", `attachment; filename="admin-config.json"`)
	writeJSON(w, m.pol.Dynamic())
}

func (m *Module) auditLog(w http.ResponseWriter, _ *http.Request) {
	es, err := m.st.Audit(200)
	if err != nil {
		fail(w, httpErr{http.StatusInternalServerError, "admin store: " + err.Error()})
		return
	}
	writeJSON(w, es)
}

// change applies edit to the current stored layer within the actor's rights and saves it. prev is the version the
// client saw; nil means the current one.
func (m *Module) change(r *http.Request, prev *int64, edit func(cfg *authz.Config) error, e Entry) error {
	actor, role := m.actor(r)
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev != nil && *prev != m.version {
		return ErrConflict
	}
	before := m.pol.Dynamic()
	after := m.pol.Dynamic()
	if err := edit(&after); err != nil {
		return err
	}
	after = authz.Normalize(after)
	if err := m.pol.CheckDynamic(after); err != nil {
		return httpErr{http.StatusBadRequest, err.Error()}
	}
	if err := m.allowed(actor, role, before, after); err != nil {
		return err
	}
	e.At, e.Actor = m.now().UTC(), actor
	v, err := m.st.Save(after, m.version, e)
	if err != nil {
		return err
	}
	if err := m.pol.SetDynamic(after); err != nil {
		return err
	}
	m.version = v
	return nil
}

// allowed enforces #123: no self-grant and no escalation. Every role that a changed grant adds or removes, and every
// role that is created, changed or deleted (old and new permissions), must be covered by the actor's own role.
func (m *Module) allowed(actor string, have authz.Role, before, after authz.Config) error {
	deny := func(format string, a ...any) error { return httpErr{http.StatusForbidden, fmt.Sprintf(format, a...)} }
	roleOf := func(cfg authz.Config, name string) authz.Role {
		if ps, ok := cfg.Roles[name]; ok {
			r := authz.Role{}
			for _, s := range ps {
				pp, _ := authz.ParsePerm(s)
				for _, p := range pp {
					if r[p.Feature] < p.Level {
						r[p.Feature] = p.Level
					}
				}
			}
			return r
		}
		r, _ := m.pol.RoleNamed(name)
		return r
	}
	b, a := grantsBy(before), grantsBy(after)
	for _, sub := range union(keys(b), keys(a)) {
		if slices.Equal(b[sub], a[sub]) {
			continue
		}
		if sub == authz.NormalizeLogin(actor) {
			return deny("you cannot change your own grants")
		}
		for _, rn := range symDiff(b[sub], a[sub]) {
			if !authz.Covers(have, roleOf(before, rn)) || !authz.Covers(have, roleOf(after, rn)) {
				return deny("you cannot grant or revoke %q: it holds more than you do", rn)
			}
		}
	}
	for _, rn := range union(keys(before.Roles), keys(after.Roles)) {
		if slices.Equal(before.Roles[rn], after.Roles[rn]) {
			continue
		}
		if !authz.Covers(have, roleOf(before, rn)) || !authz.Covers(have, roleOf(after, rn)) {
			return deny("you cannot change role %q: it would hold more than you do", rn)
		}
	}
	return nil
}

func grantsBy(c authz.Config) map[string][]string {
	out := map[string][]string{}
	for _, g := range c.Grants {
		out[g.Subject] = g.Roles
	}
	return out
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func union(a, b []string) []string {
	out := slices.Clone(a)
	for _, x := range b {
		if !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	slices.Sort(out)
	return out
}

func symDiff(a, b []string) []string {
	var out []string
	for _, x := range union(a, b) {
		if slices.Contains(a, x) != slices.Contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func normSubject(s string) string {
	s = strings.TrimSpace(s)
	if g, ok := strings.CutPrefix(s, "group:"); ok {
		return "group:" + strings.TrimSpace(g)
	}
	return authz.NormalizeLogin(s)
}

// setGrant replaces the stored roles of one subject. An empty list removes the grant.
func (m *Module) setGrant(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Subject string   `json:"subject"`
		Roles   []string `json:"roles"`
		Version *int64   `json:"version"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	sub := normSubject(req.Subject)
	if sub == "" || sub == "group:" {
		fail(w, httpErr{http.StatusBadRequest, "subject is empty"})
		return
	}
	roles := slices.Clone(req.Roles)
	slices.Sort(roles)
	roles = slices.Compact(roles)
	action := "grant"
	if len(roles) == 0 {
		action = "revoke"
	}
	err := m.change(r, req.Version, func(cfg *authz.Config) error {
		out := cfg.Grants[:0:0]
		for _, g := range cfg.Grants {
			if g.Subject != sub {
				out = append(out, g)
			}
		}
		if len(roles) > 0 {
			out = append(out, authz.Grant{Subject: sub, Roles: roles})
		}
		cfg.Grants = out
		return nil
	}, Entry{Action: action, Target: sub, Detail: strings.Join(roles, ",")})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// putRole creates or replaces a stored role.
func (m *Module) putRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var req struct {
		Perms   []string `json:"perms"`
		Version *int64   `json:"version"`
	}
	if err := readJSON(r, &req); err != nil {
		fail(w, err)
		return
	}
	err := m.change(r, req.Version, func(cfg *authz.Config) error {
		cfg.Roles[name] = req.Perms
		return nil
	}, Entry{Action: "role", Target: name, Detail: strings.Join(req.Perms, ",")})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// deleteRole removes a stored role that no stored grant uses.
func (m *Module) deleteRole(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	err := m.change(r, nil, func(cfg *authz.Config) error {
		if _, ok := cfg.Roles[name]; !ok {
			return httpErr{http.StatusNotFound, "no stored role " + name}
		}
		for _, g := range cfg.Grants {
			if slices.Contains(g.Roles, name) {
				return httpErr{http.StatusConflict, fmt.Sprintf("role %q is granted to %s: revoke it first", name, g.Subject)}
			}
		}
		delete(cfg.Roles, name)
		return nil
	}, Entry{Action: "role-delete", Target: name})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// importCfg replaces the stored layer with an ADMIN_CONFIG document.
func (m *Module) importCfg(w http.ResponseWriter, r *http.Request) {
	var in authz.Config
	if err := readJSON(r, &in); err != nil {
		fail(w, err)
		return
	}
	if in.Roles == nil {
		in.Roles = map[string][]string{}
	}
	n := in
	err := m.change(r, nil, func(cfg *authz.Config) error {
		*cfg = n
		return nil
	}, Entry{Action: "import", Detail: fmt.Sprintf("%d roles, %d grants", len(in.Roles), len(in.Grants))})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}
