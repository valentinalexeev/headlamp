package policy

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
)

const (
	defaultRevertSeconds = 120
	minRevertSeconds     = 30
	maxRevertSeconds     = 900
	defaultMaxBytes      = 262144
	minMaxBytes          = 1024
	maxMaxBytes          = 4194304
	watchInterval        = 5 * time.Second
)

// Module is the policy console: the admin API and page for editing the headscale policy, active
// only when POLICY_DB is set. Errors never carry secret values.
type Module struct {
	env   core.Env
	api   API
	store *Store
	svc   *Service

	watchDone chan struct{} // closed when the watcher has returned; nil if never started
}

// New returns the policy module configured from env: POLICY_DB, POLICY_ADMIN_USERS,
// POLICY_REVERT_SECONDS and POLICY_MAX_BYTES. api is the headscale client.
func New(env core.Env, api API) core.Module { return &Module{env: env, api: api} }

func (m *Module) Name() string { return "policy" }

// Service returns the policy service once Register has set it up, nil while the console is inactive. Other
// modules (funnel) submit their ACL changes through it.
func (m *Module) Service() *Service { return m.svc }

// Nav puts the console in the admin top bar; an inactive module advertises nothing.
func (m *Module) Nav() []core.NavItem {
	if m.svc == nil {
		return nil
	}
	return []core.NavItem{{Label: "Policy", Path: "/policy", Order: 30}}
}

// intEnv reads an integer variable with a default and an inclusive range.
func intEnv(env core.Env, key string, def, lo, hi int) (int, error) {
	v := env.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
	}
	return n, nil
}

func (m *Module) Register(r *core.Routes) error {
	path := m.env.Getenv("POLICY_DB")
	if path == "" {
		return nil
	}
	allowed := authz.ParseAllowed(m.env.Getenv("POLICY_ADMIN_USERS"))
	if !allowed.Has(firstLogin(m.env.Getenv("POLICY_ADMIN_USERS"))) {
		return errors.New("policy console enabled (POLICY_DB set) but POLICY_ADMIN_USERS names no login")
	}
	hosts := m.env.List("ENROLL_ALLOWED_HOSTS")
	if len(hosts) == 0 || slices.Contains(hosts, "*") {
		return errors.New("policy console enabled (POLICY_DB set) but ENROLL_ALLOWED_HOSTS is empty or contains \"*\"; " +
			"list the host names the admin listener answers to (the core's Host guard protects the console from DNS rebinding)")
	}
	for _, k := range []string{"HEADSCALE_LOGIN_SERVER", "HEADSCALE_API_KEY"} {
		if m.env.Getenv(k) == "" {
			return fmt.Errorf("policy console enabled (POLICY_DB set) but %s is not set", k)
		}
	}
	revert, err := intEnv(m.env, "POLICY_REVERT_SECONDS", defaultRevertSeconds, minRevertSeconds, maxRevertSeconds)
	if err != nil {
		return err
	}
	maxBytes, err := intEnv(m.env, "POLICY_MAX_BYTES", defaultMaxBytes, minMaxBytes, maxMaxBytes)
	if err != nil {
		return err
	}
	if r.Tailnet == nil {
		return errors.New("policy console enabled (POLICY_DB set) but the core has no tailnet node to identify users")
	}
	store, err := OpenStore(path, nil)
	if err != nil {
		return err
	}
	m.store = store
	m.svc = NewService(m.api, store, time.Duration(revert)*time.Second, maxBytes, nil)
	gate := authz.UserGate(r.Tailnet, allowed, r.Log)
	newConsole(m.svc, store, maxBytes).register(r.Admin, gate)
	return nil
}

// firstLogin returns the first non-blank item of a csv list, so a list of only blanks is caught.
func firstLogin(csv string) string {
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return ""
}

// Start launches the revert watcher and returns at once. The watcher stops with ctx; the store
// stays open until Stop, so requests still in flight at shutdown never see a closed database.
func (m *Module) Start(ctx context.Context) error {
	if m.svc == nil {
		return nil
	}
	m.watchDone = make(chan struct{})
	go func() {
		defer close(m.watchDone)
		m.svc.RunWatcher(ctx, watchInterval)
	}()
	return nil
}

// Stop waits (bounded by ctx) for the watcher to end, then closes the store. It is safe without
// Start and when the module is inactive.
func (m *Module) Stop(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	if m.watchDone != nil {
		select {
		case <-m.watchDone:
		case <-ctx.Done():
		}
	}
	return m.store.Close()
}
