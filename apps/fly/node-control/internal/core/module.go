package core

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
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

// AttentionLevel ranks an attention item: "timed", "critical", "waiting" or "info", most urgent first.
type AttentionLevel string

const (
	LevelTimed    AttentionLevel = "timed"
	LevelCritical AttentionLevel = "critical"
	LevelWaiting  AttentionLevel = "waiting"
	LevelInfo     AttentionLevel = "info"
)

// AttentionItem is one thing that needs the operator, or one notice. All text is scrubbed by the
// core before it leaves, so a module cannot bypass the limits. Zero times are omitted from JSON.
type AttentionItem struct {
	ID          string         `json:"id"`
	Source      string         `json:"source"`
	Level       AttentionLevel `json:"level"`
	Title       string         `json:"title"`
	Detail      string         `json:"detail,omitempty"`
	ActionLabel string         `json:"action_label,omitempty"`
	ActionPath  string         `json:"action_path,omitempty"` // a local path, optionally with a query
	Since       time.Time      `json:"since,omitzero"`        // zero when not applicable
	Deadline    time.Time      `json:"deadline,omitzero"`     // set for "timed"
	AppliedAt   time.Time      `json:"applied_at,omitzero"`   // set for a policy change
	ChangeID    int64          `json:"change_id,omitempty"`   // set for a policy change
}

// SystemLine is the one-line health of a module. State is "ok", "waiting", "critical", "unknown" or "off".
type SystemLine struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	State   string `json:"state"`
	Summary string `json:"summary"`
}

// ErrNotVisible is returned by a source that has nothing to show this caller. The core then leaves
// the module out and tells the page that some systems are hidden.
var ErrNotVisible = errors.New("not visible to this user")

// AttentionSource is an optional interface for modules that report what needs attention.
type AttentionSource interface {
	Attention(ctx context.Context, login string) ([]AttentionItem, []SystemLine, error)
}

// ActivityEntry is one line of the cross-module activity log.
type ActivityEntry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Source string    `json:"source"`
	Action string    `json:"action"`
	Detail string    `json:"detail"`
}

// ActivitySource is an optional interface for modules that keep an activity log.
type ActivitySource interface {
	Activity(ctx context.Context, login string, limit int) ([]ActivityEntry, error)
}

// SearchItem is one entry of the console search: a visible label, a kind and a local path.
type SearchItem struct {
	Label string `json:"label"`
	Kind  string `json:"kind"`
	Path  string `json:"path"`
}

// SearchSource is an optional interface for modules that contribute search entries.
type SearchSource interface {
	Search(ctx context.Context, login string) ([]SearchItem, error)
}
