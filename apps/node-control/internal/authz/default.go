package authz

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// The process-wide policy. main sets it once, before the modules register: from ADMIN_CONFIG, with each legacy
// *_ADMIN_USERS list folded in as admin of its feature (AddLegacy). Module code asks Can instead of reading its
// own list, so a grant in ADMIN_CONFIG counts everywhere: routes, attention, search and navigation.
var current atomic.Pointer[Policy]

// SetDefault installs p as the process-wide policy. Tests may call it; they must restore the old value.
func SetDefault(p *Policy) (old *Policy) { return current.Swap(p) }

// Default returns the process-wide policy, nil if none is set.
func Default() *Policy { return current.Load() }

// Can reports whether login holds perm under the process-wide policy. Without a policy, it falls back to legacy:
// the caller passes the legacy list of its feature, and a login on it is admin of that feature.
func Can(login string, perm Perm, legacy Allowed) bool {
	if p := current.Load(); p != nil {
		return p.Can(Subject{Login: login}, perm)
	}
	return legacy.Has(login) && perm.Level <= LevelAdmin
}

// AnyoneCan reports whether at least one subject holds perm under the process-wide policy, or, without one,
// whether the legacy list names anyone. A module uses it to decide whether it can be operated at all.
func AnyoneCan(perm Perm, legacy Allowed) bool {
	if p := current.Load(); p != nil {
		return AnyoneCanIn(p, perm)
	}
	return len(legacy.set) > 0
}

// NavAllow is the NavItem.Allow of a page that needs perm. With no policy, or one that grants nothing (the open
// transition mode), it is nil: the page shows to everyone who can reach the console, as before. Otherwise the
// page shows only to who holds perm, matching what its route lets through.
func NavAllow(perm Perm) func(string) bool {
	if p := current.Load(); p == nil || p.Empty() {
		return nil
	}
	return func(login string) bool { return Can(login, perm, Allowed{}) }
}

// Gate is the module-level user gate: it lets through a human who holds perm under the process-wide policy, or,
// with no policy set, one on the legacy list. Tagged nodes and WhoIs errors never pass. The normalised login is
// stored in the context for LoginFrom.
func Gate(tn core.Tailnet, perm Perm, legacy Allowed, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			login, tags, err := tn.WhoIs(r.Context(), r.RemoteAddr)
			switch l := NormalizeLogin(login); {
			case err != nil:
				deny(w, log, "whois_error")
			case len(tags) > 0 || l == "tagged-devices" || l == "":
				deny(w, log, "tagged")
			case !Can(l, perm, legacy):
				deny(w, log, "not_allowed")
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), loginKey, l)))
			}
		})
	}
}
