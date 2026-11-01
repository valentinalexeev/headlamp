package nodes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/ui"
)

//go:embed static/nodes.html
var pageHTML []byte

//go:embed static/nodes.js
var pageJS []byte

const nodeTimeout = 10 * time.Second

// Module serves the node API and the control page.
type Module struct {
	cfg       *Config
	src       headscale.NodeSource
	providers map[string]Provider
	timeout   time.Duration

	now     func() time.Time // nil means time.Now (tests)
	stMu    sync.Mutex       // guards stCache
	stCache map[string]cachedStatus

	audit func() core.AuditLog // nil, or returning nil, when there is no audit log
	tn    core.Tailnet         // names the caller in audit entries
	log   *slog.Logger

	mu       sync.Mutex
	lastGood []headscale.Node // last node list headscale returned successfully
	haveGood bool
}

// Option configures the module.
type Option func(*Module)

// WithAudit gives the module the audit log that records node actions. The log is fetched when an action runs, not
// here, because the enrolment store that backs it opens at Register, after New. A nil function, or one returning
// nil, writes a structured "audit" log line instead.
func WithAudit(f func() core.AuditLog) Option { return func(m *Module) { m.audit = f } }

// New returns the nodes module. providers maps a provider name (see
// Providers) to its implementation; src may be nil.
func New(cfg *Config, src headscale.NodeSource, providers map[string]Provider, opts ...Option) core.Module {
	m := &Module{cfg: cfg, src: src, providers: providers, timeout: nodeTimeout}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Module) Name() string                { return "nodes" }
func (m *Module) Start(context.Context) error { return nil }

func (m *Module) Register(r *core.Routes) error {
	m.tn, m.log = r.Tailnet, r.Log
	r.Admin.HandleFunc("GET /nodes", asset("text/html; charset=utf-8", pageHTML))
	r.Admin.HandleFunc("GET /nodes/nodes.js", asset("text/javascript; charset=utf-8", pageJS))
	r.Admin.HandleFunc("GET /nodes.json", m.nodesJSON)
	r.Admin.HandleFunc("GET /status", m.status)
	r.Admin.HandleFunc("GET /nodes/{id}/status", m.nodeStatus)
	r.Admin.HandleFunc("POST /nodes/{id}/start", m.act("start"))
	r.Admin.HandleFunc("POST /nodes/{id}/stop", m.act("stop"))
	return nil
}

// Targets is the current discovery result. If headscale cannot be read, the
// last good node list is used so a blip does not remove every discovered node;
// only when no read has ever succeeded are just the explicitly configured
// targets returned. Caching of fresh reads lives in the NodeSource.
func (m *Module) Targets(ctx context.Context) []Target {
	var ns []headscale.Node
	if m.src != nil {
		got, err := m.src.Nodes(ctx)
		m.mu.Lock()
		if err == nil {
			m.lastGood = got
		}
		ns = m.lastGood
		m.mu.Unlock()
	}
	return Discover(m.cfg, ns)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func detail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"detail": msg})
}

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script or
// style that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", ui.PageCSP)
		_, _ = w.Write(body)
	}
}

func (m *Module) nodesJSON(w http.ResponseWriter, req *http.Request) {
	writeJSON(w, 200, UIView(m.cfg, m.Targets(req.Context())))
}

// find resolves the {id} path value, writing the 404 itself.
func (m *Module) find(w http.ResponseWriter, req *http.Request) (Target, bool) {
	id := req.PathValue("id")
	t, ok := Find(m.cfg, m.Targets(req.Context()), id)
	if !ok {
		detail(w, 404, fmt.Sprintf("unknown node '%s'", id))
	}
	return t, ok
}

// statusOf is one node's status; provider problems become a status.
func (m *Module) statusOf(ctx context.Context, t Target) Status {
	if t.Provider == "" {
		return Status{"status": "unmanaged"}
	}
	p := m.providers[t.Provider]
	if p == nil {
		return Status{"status": "not_configured", "detail": "no " + t.Provider + " provider available"}
	}
	ctx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()
	type result struct {
		st  Status
		err error
	}
	ch := make(chan result, 1)
	go func() {
		var r result
		defer func() {
			if rec := recover(); rec != nil {
				r = result{nil, panicError(rec)}
			}
			ch <- r
		}()
		r.st, r.err = p.Status(ctx, t)
	}()
	select {
	case r := <-ch:
		return statusResult(r.st, r.err)
	case <-ctx.Done():
		return Status{"status": "error", "detail": "timed out: " + ctx.Err().Error()}
	}
}

// panicError turns a recovered provider panic into an APIError so one
// provider cannot crash the process.
func panicError(rec any) error {
	return &APIError{Message: fmt.Sprintf("provider panicked: %v", rec)}
}

// safeAct runs a provider action, converting a panic into an error.
func safeAct(ctx context.Context, fn func(context.Context, Target) (string, string, error), t Target) (prev, cur string, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			prev, cur, err = "", "", panicError(rec)
		}
	}()
	return fn(ctx, t)
}

func statusResult(st Status, err error) Status {
	switch {
	case err == nil:
		if st == nil {
			st = Status{"status": "unknown"}
		}
		return st
	case errors.Is(err, ErrNotConfigured):
		return Status{"status": "not_configured", "detail": err.Error()}
	default:
		return Status{"status": "error", "detail": err.Error()}
	}
}

func (m *Module) status(w http.ResponseWriter, req *http.Request) {
	ts := m.Targets(req.Context())
	out := make([]Status, len(ts))
	var wg sync.WaitGroup
	for i, t := range ts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[i] = m.statusOf(req.Context(), t)
		}()
	}
	wg.Wait()
	res := make(map[string]Status, len(ts))
	for i, t := range ts {
		res[t.ID] = out[i]
	}
	writeJSON(w, 200, res)
}

func (m *Module) nodeStatus(w http.ResponseWriter, req *http.Request) {
	t, ok := m.find(w, req)
	if !ok {
		return
	}
	writeJSON(w, 200, m.statusOf(req.Context(), t))
}

// record writes the audit entry of a node action: to the audit log, or as one structured line when there is none or
// it fails. Detail names the node and the outcome ("ok" or the failure class), never the provider's error text. A
// failure to record never changes the response.
func (m *Module) record(req *http.Request, action, node, outcome string) {
	actor := "unknown"
	if m.tn != nil {
		if login, _, err := m.tn.WhoIs(req.Context(), req.RemoteAddr); err == nil && login != "" {
			actor = login
		}
	}
	detail := node + ": " + outcome
	log := m.log
	if log == nil {
		log = slog.Default()
	}
	var al core.AuditLog
	if m.audit != nil {
		al = m.audit()
	}
	if al == nil {
		log.Info("audit", "actor", actor, "action", action, "detail", detail)
		return
	}
	if err := al.Audit(actor, action, detail); err != nil {
		log.Error("audit log write failed", "actor", actor, "action", action, "detail", detail, "error_type", fmt.Sprintf("%T", err))
	}
}

// failureClass names why an action failed without copying the provider's message.
func failureClass(err error) string {
	switch {
	case errors.Is(err, ErrNotConfigured):
		return "failed (not_configured)"
	case errors.Is(err, context.DeadlineExceeded):
		return "failed (timeout)"
	case errors.Is(err, context.Canceled): // the caller went away; nothing says the provider failed
		return "failed (cancelled)"
	default:
		return "failed (provider_error)"
	}
}

func (m *Module) act(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		t, ok := m.find(w, req)
		if !ok {
			return
		}
		audit := "node." + action
		if t.Provider == "" {
			m.record(req, audit, t.ID, "failed (unmanaged)")
			detail(w, 409, "node has no cloud provider tag")
			return
		}
		p := m.providers[t.Provider]
		if p == nil {
			m.record(req, audit, t.ID, "failed (no_provider)")
			detail(w, 503, "no "+t.Provider+" provider available")
			return
		}
		ctx, cancel := context.WithTimeout(req.Context(), m.timeout)
		defer cancel()
		fn := p.Start
		if action == "stop" {
			fn = p.Stop
		}
		prev, cur, err := safeAct(ctx, fn, t)
		if err != nil {
			m.record(req, audit, t.ID, failureClass(err))
		} else {
			m.record(req, audit, t.ID, "ok")
		}
		switch {
		case errors.Is(err, ErrNotConfigured):
			detail(w, 503, err.Error())
		case err != nil:
			detail(w, 502, err.Error())
		default:
			writeJSON(w, 200, map[string]string{"node": t.ID, "action": action, "previous_status": prev, "current_status": cur})
		}
	}
}

// Nav puts the control page in the top bar.
func (m *Module) Nav() []core.NavItem {
	return []core.NavItem{{Label: "Nodes", Path: "/nodes", Order: 10, Group: "operate", Icon: "nodes", Source: "nodes"}}
}
