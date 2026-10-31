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
	Public  *http.ServeMux // nil unless enrolment is enabled (ENROLL_DB or ENROLL_REDIS=1)
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

// NavItem is one entry of the console sidebar. Path is an absolute local path.
type NavItem struct {
	Label string `json:"label"`
	Path  string `json:"path"`
	Order int    `json:"order"`
	Group string `json:"group,omitempty"` // "", "operate" or "configure"
	Icon  string `json:"icon,omitempty"`  // a ui.Icon name
	// Allow reports whether the caller may see the entry; nil means anyone who can reach the listener.
	Allow func(login string) bool `json:"-"`
	// Source is the attention source (usually the module name) whose items are counted beside the
	// entry; empty means no count.
	Source string `json:"-"`
}

// Navigator is an optional interface for modules that have an admin page. A module that is
// disabled at run time returns no entries. Entries are collected once, after every Register.
type Navigator interface {
	Nav() []NavItem
}

// NavAction is a persistent top-bar action, such as "Enrol device".
type NavAction struct{ Label, Path, Icon string }

// ActionProvider is an optional interface for modules that add a top-bar action. A module that is
// inactive returns none. Actions are collected once, after every Register.
type ActionProvider interface {
	Actions() []NavAction
}
