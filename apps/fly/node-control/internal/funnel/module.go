package funnel

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/policy"
)

// Headscale is the part of the headscale client the module uses.
type Headscale interface {
	ListNodes(ctx context.Context) ([]headscale.Node, error)
	ListUsers(ctx context.Context) ([]headscale.User, error)
	CreatePreAuthKey(ctx context.Context, userID uint64, tags []string, ttl time.Duration, ephemeral, reusable bool) (headscale.PreAuthKey, error)
	ListPreAuthKeys(ctx context.Context) ([]headscale.PreAuthKey, error)
	ExpirePreAuthKey(ctx context.Context, id uint64) error
}

// PolicyService is the part of the policy console's service the module uses: every ACL change goes through
// its confirm-or-revert.
type PolicyService interface {
	Current(ctx context.Context) (text, updatedAt string, err error)
	Change(ctx context.Context, actor string, transform func(current string) (string, error), reason string) (int64, time.Time, error)
	Outcome(id int64) policy.Outcome
}

// Module is the funnel admin surface: namespaces (with generated ACL entries), manual routes, blocks and
// publisher keys for humans, and the two endpoints the funnel-ingress app pulls from and reports to. It is
// active only when FUNNEL_DB is set; unset, it registers nothing.
type Module struct {
	env core.Env
	hs  Headscale
	pol func() PolicyService // nil result: the policy console is not active

	s   *Settings
	st  Store
	svc PolicyService
	log *slog.Logger
	now func() time.Time

	// mu serialises every namespace operation: read the namespaces, change the ACL, write the store.
	mu sync.Mutex
}

// New returns the module. pol is called at Register time, after the policy module registered.
func New(env core.Env, hs Headscale, pol func() PolicyService) core.Module {
	return &Module{env: env, hs: hs, pol: pol, now: time.Now}
}

func (m *Module) Name() string { return "funnel" }

// Nav puts the page in the admin top bar; an inactive module advertises nothing.
func (m *Module) Nav() []core.NavItem {
	if m.st == nil {
		return nil
	}
	return []core.NavItem{{Label: "Funnel", Path: "/funnel", Order: 40}}
}

func (m *Module) Register(r *core.Routes) error {
	s, err := SettingsFromEnv(m.env)
	if err != nil {
		return err
	}
	if s == nil {
		return nil
	}
	hosts := m.env.List("ENROLL_ALLOWED_HOSTS")
	if len(hosts) == 0 || slices.Contains(hosts, "*") {
		return errors.New("funnel enabled (FUNNEL_DB set) but ENROLL_ALLOWED_HOSTS is empty or contains \"*\"; " +
			"list the host names the admin listener answers to (the core's Host guard protects the page from DNS rebinding)")
	}
	for _, k := range []string{"HEADSCALE_LOGIN_SERVER", "HEADSCALE_API_KEY"} {
		if m.env.Getenv(k) == "" {
			return fmt.Errorf("funnel enabled (FUNNEL_DB set) but %s is not set", k)
		}
	}
	if r.Tailnet == nil {
		return errors.New("funnel enabled (FUNNEL_DB set) but the core has no tailnet node to identify callers")
	}
	var svc PolicyService
	if m.pol != nil {
		svc = m.pol()
	}
	if svc == nil {
		return errors.New("funnel enabled (FUNNEL_DB set) but the policy console is not active (set POLICY_DB): ACL changes go through it")
	}
	st, err := OpenSQLite(s.DBPath, nil)
	if err != nil {
		return err
	}
	m.s, m.st, m.svc = s, st, svc
	m.log = r.Log
	if m.log == nil {
		m.log = slog.Default()
	}

	// Two gates, no overlap: humans on /funnel/*, the ingress node on /ingress/v1/*.
	human := authz.UserGate(r.Tailnet, s.Admins, m.log)
	machine := authz.TagGate(r.Tailnet, s.IngressTag, m.log)
	(&ingress{st: st, s: s, outcome: m.outcome, now: m.now, log: m.log}).register(r.Admin, machine)
	m.registerHuman(r.Admin, human)
	return nil
}

// outcome adapts the policy service's outcome type to the string the config builder takes.
func (m *Module) outcome(id int64) string { return string(m.svc.Outcome(id)) }

func (m *Module) Start(context.Context) error { return nil }

// Stop closes the store after the HTTP servers have drained.
func (m *Module) Stop(context.Context) error {
	if m.st == nil {
		return nil
	}
	return m.st.Close()
}
