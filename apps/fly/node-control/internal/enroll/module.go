package enroll

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

// Module is the enrolment module: the operator API on the admin mux and the public handler on the
// public mux, active only when ENROLL_DB is set. Errors never carry secret values.
type Module struct {
	env   core.Env
	src   headscale.NodeSource
	now   func() time.Time
	store *Store
	admin *Admin
	reg   *Registry

	loopDone chan struct{} // closed when the reconcile loop has returned; nil if never started
}

// Option configures the module.
type Option func(*Module)

// WithNodeSource gives the module the headscale node list: it finds the agents of ingresses
// without an address, reports whether each ingress node is online and discovers new ingress nodes
// (which are never used before the operator approves them).
func WithNodeSource(src headscale.NodeSource) Option { return func(m *Module) { m.src = src } }

// withClock replaces the clock of the sync states and the discovery cache (tests).
func withClock(now func() time.Time) Option { return func(m *Module) { m.now = now } }

// New returns the enrolment module configured from env: ENROLL_DB, ENROLL_CONFIG,
// ENROLL_SECRET_KEY, AGENT_URL and AGENT_TOKEN, plus one token variable per further ingress.
func New(env core.Env, opts ...Option) core.Module {
	m := &Module{env: env}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Module) Name() string { return "enroll" }

// hasKey reports whether the JSON object data has the top-level key.
func hasKey(data []byte, key string) bool {
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return false
	}
	_, ok := raw[key]
	return ok
}

func (m *Module) Register(r *core.Routes) error {
	if m.env.Getenv("ENROLL_DB") == "" {
		return nil
	}
	// The configuration is read first only to learn its shape: the single-host file still needs
	// AGENT_URL and AGENT_TOKEN, the list shape names its tokens per ingress.
	var data []byte
	var readErr, parseErr error
	var cfg IngressConfig
	legacy, linkHostSet := true, false
	if path := m.env.Getenv("ENROLL_CONFIG"); path != "" {
		if data, readErr = os.ReadFile(path); readErr != nil {
			var pe *fs.PathError
			if errors.As(readErr, &pe) {
				readErr = pe.Err
			}
		} else if cfg, parseErr = ParseIngressConfig(data); parseErr == nil {
			legacy = !hasKey(data, "ingresses")
			linkHostSet = hasKey(data, "link_host")
		}
	}
	required := []string{"ENROLL_CONFIG", "ENROLL_SECRET_KEY"}
	if legacy {
		required = append(required, "AGENT_URL", "AGENT_TOKEN")
	}
	var missing []string
	for _, k := range required {
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
	if readErr != nil {
		return fmt.Errorf("ENROLL_CONFIG: cannot read the file: %w", readErr)
	}
	if parseErr != nil {
		return parseErr
	}
	key := m.env.Getenv("ENROLL_SECRET_KEY")
	if len(key) < 16 {
		return errors.New("ENROLL_SECRET_KEY must be at least 16 bytes")
	}
	cfg = cfg.WithKey([]byte(key))
	agentURL := m.env.Getenv("AGENT_URL")
	if agentURL != "" {
		u, err := url.Parse(agentURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return errors.New("AGENT_URL must be an http(s) URL with a host and no credentials")
		}
	}

	var dial = HTTPClientFor(nil)
	if r.Tailnet != nil {
		dial = HTTPClientFor(r.Tailnet.Dial)
	}
	reg, err := NewRegistry(RegistryConfig{Cfg: cfg, Src: m.src, Getenv: m.env.Getenv, HC: dial, Now: m.now,
		AgentURL: agentURL, LinkHostSet: linkHostSet})
	if err != nil {
		return err
	}
	store, err := OpenStore(m.env.Getenv("ENROLL_DB"), []byte(key), nil)
	if err != nil {
		return err
	}
	reg.SetStore(store)
	m.store, m.reg = store, reg
	m.admin = NewAdminFor(store, reg)
	m.admin.Register(r.Admin)
	NewPublicFor(store, reg).Register(r.Public)
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
		ReconcileLoopAll(ctx, m.store, m.reg.LinkConfig(), func() []SyncTarget {
			return m.reg.Current(ctx, true).Targets
		})
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
