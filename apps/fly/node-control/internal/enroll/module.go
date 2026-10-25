package enroll

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"

	"example.com/node-control/internal/core"
)

// Module is the enrolment module: the operator API on the admin mux and the public handler on the
// public mux, active only when ENROLL_DB is set. Errors never carry secret values.
type Module struct {
	env   core.Env
	store *Store
	admin *Admin
	agent Agent

	loopDone chan struct{} // closed when the reconcile loop has returned; nil if never started
}

// New returns the enrolment module configured from env: ENROLL_DB, ENROLL_CONFIG,
// ENROLL_SECRET_KEY, AGENT_URL and AGENT_TOKEN.
func New(env core.Env) core.Module { return &Module{env: env} }

func (m *Module) Name() string { return "enroll" }

func (m *Module) Register(r *core.Routes) error {
	if m.env.Getenv("ENROLL_DB") == "" {
		return nil
	}
	var missing []string
	for _, k := range []string{"ENROLL_CONFIG", "ENROLL_SECRET_KEY", "AGENT_URL", "AGENT_TOKEN"} {
		if m.env.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("enrolment enabled (ENROLL_DB set) but missing: %s", strings.Join(missing, ", "))
	}
	if r.Public == nil {
		return errors.New("enrolment enabled (ENROLL_DB set) but the core has no public mux")
	}

	data, err := os.ReadFile(m.env.Getenv("ENROLL_CONFIG"))
	if err != nil {
		var pe *fs.PathError
		if errors.As(err, &pe) {
			err = pe.Err
		}
		return fmt.Errorf("ENROLL_CONFIG: cannot read the file: %w", err)
	}
	cfg, err := ParseIngressConfig(data)
	if err != nil {
		return err
	}
	if !cfg.Single() {
		return errors.New("ENROLL_CONFIG: several ingresses are not wired into the module yet")
	}
	key := m.env.Getenv("ENROLL_SECRET_KEY")
	if len(key) < 16 {
		return errors.New("ENROLL_SECRET_KEY must be at least 16 bytes")
	}
	cfg = cfg.WithKey([]byte(key))
	u, err := url.Parse(m.env.Getenv("AGENT_URL"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return errors.New("AGENT_URL must be an http(s) URL with a host and no credentials")
	}

	store, err := OpenStore(m.env.Getenv("ENROLL_DB"), []byte(key), nil)
	if err != nil {
		return err
	}
	var dial = HTTPClientFor(nil)
	if r.Tailnet != nil {
		dial = HTTPClientFor(r.Tailnet.Dial)
	}
	m.store = store
	m.agent = NewAgent(m.env.Getenv("AGENT_URL"), m.env.Getenv("AGENT_TOKEN"), dial)
	m.admin = NewAdmin(store, cfg, m.agent)
	m.admin.Register(r.Admin)
	NewPublic(store, cfg).Register(r.Public)
	return nil
}

// Start launches the reconcile loop and returns at once. The loop stops with ctx; the store stays
// open until Stop, so requests still in flight at shutdown never see a closed database.
func (m *Module) Start(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	m.loopDone = make(chan struct{})
	go func() {
		defer close(m.loopDone)
		ReconcileLoop(ctx, m.store, m.agent, m.admin.State())
	}()
	return nil
}

// Stop waits (bounded by ctx) for the reconcile loop to end, then closes the store. Core calls it
// after the HTTP servers are down. It is idempotent and safe without Start.
func (m *Module) Stop(ctx context.Context) error {
	if m.store == nil {
		return nil
	}
	if m.loopDone != nil {
		select {
		case <-m.loopDone:
		case <-ctx.Done():
		}
	}
	return m.store.Close()
}

// Nav puts the enrolment page in the top bar, but only when the module registered (ENROLL_DB set).
func (m *Module) Nav() []core.NavItem {
	if m.store == nil {
		return nil
	}
	return []core.NavItem{{Label: "Enrolment", Path: "/enroll", Order: 20}}
}
