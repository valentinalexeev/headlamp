package nodes

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

//go:embed static/index.html
var indexHTML []byte

const nodeTimeout = 10 * time.Second

// Module serves the node API and the control page.
type Module struct {
	cfg       *Config
	src       headscale.NodeSource
	providers map[string]Provider
	timeout   time.Duration

	mu       sync.Mutex
	lastGood []headscale.Node // last node list headscale returned successfully
	haveGood bool
}

// New returns the nodes module. providers maps a provider name (see
// Providers) to its implementation; src may be nil.
func New(cfg *Config, src headscale.NodeSource, providers map[string]Provider) core.Module {
	return &Module{cfg: cfg, src: src, providers: providers, timeout: nodeTimeout}
}

func (m *Module) Name() string                { return "nodes" }
func (m *Module) Start(context.Context) error { return nil }

func (m *Module) Register(r *core.Routes) error {
	r.Admin.HandleFunc("GET /{$}", m.index)
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

func (m *Module) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(indexHTML)
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

func (m *Module) act(action string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		t, ok := m.find(w, req)
		if !ok {
			return
		}
		if t.Provider == "" {
			detail(w, 409, "node has no cloud provider tag")
			return
		}
		p := m.providers[t.Provider]
		if p == nil {
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
