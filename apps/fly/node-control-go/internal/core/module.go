package core

import (
	"context"
	"log/slog"
	"net/http"
)

// Module is a unit of functionality plugged into the core.
type Module interface {
	Name() string
	// Register adds routes to r.Admin and, when non-nil, r.Public.
	Register(r *Routes) error
	// Start runs after Register and before listeners serve. It must return
	// promptly and spawn goroutines for background work, which should stop
	// with ctx.
	Start(ctx context.Context) error
}

// Routes is what the core hands to every module.
type Routes struct {
	Admin   *http.ServeMux
	Public  *http.ServeMux // nil unless ENROLL_DB is set
	Env     Env
	Tailnet Tailnet
	Log     *slog.Logger
}

// Stopper is an optional interface for modules holding resources that must
// outlive in-flight requests. Run calls Stop once on every module that
// implements it, after the HTTP servers have shut down (or immediately if
// Run fails before serving), with a bounded context.
type Stopper interface {
	Stop(ctx context.Context) error
}
