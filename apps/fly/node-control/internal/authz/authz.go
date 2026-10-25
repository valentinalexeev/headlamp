// Package authz gates HTTP handlers by the tailnet identity of the caller (WhoIs).
package authz

import (
	"context"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"example.com/node-control/internal/core"
)

type ctxKey int

const (
	loginKey ctxKey = iota
	tagsKey
)

// NormalizeLogin lower-cases and trims s and removes one trailing "@".
// A "name@domain" login keeps its domain.
func NormalizeLogin(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), "@")
}

// Allowed is a set of normalised logins.
type Allowed struct{ set map[string]bool }

// ParseAllowed reads a comma-separated list of logins; empty items are dropped.
func ParseAllowed(csv string) Allowed {
	a := Allowed{set: map[string]bool{}}
	for _, p := range strings.Split(csv, ",") {
		if l := NormalizeLogin(p); l != "" {
			a.set[l] = true
		}
	}
	return a
}

// Has reports whether login is in the set. An empty set allows nobody.
func (a Allowed) Has(login string) bool {
	l := NormalizeLogin(login)
	return l != "" && a.set[l]
}

func deny(w http.ResponseWriter, log *slog.Logger, reason string) {
	if log == nil {
		log = slog.Default()
	}
	log.Warn("authz refused", "reason", reason)
	w.WriteHeader(http.StatusForbidden)
}

// UserGate lets only listed human users through (403, empty body, otherwise).
// Tagged nodes are never users. The normalised login is stored in the context.
func UserGate(tn core.Tailnet, a Allowed, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(a.set) == 0 {
				deny(w, log, "empty_allowlist")
				return
			}
			login, tags, err := tn.WhoIs(r.Context(), r.RemoteAddr)
			switch l := NormalizeLogin(login); {
			case err != nil:
				deny(w, log, "whois_error")
			case len(tags) > 0 || l == "tagged-devices":
				deny(w, log, "tagged")
			case !a.Has(l):
				deny(w, log, "not_allowed")
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), loginKey, l)))
			}
		})
	}
}

// TagGate lets only nodes carrying exactly tag through (403, empty body, otherwise).
// The node's tags are stored in the context.
func TagGate(tn core.Tailnet, tag string, log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, tags, err := tn.WhoIs(r.Context(), r.RemoteAddr)
			switch {
			case err != nil:
				deny(w, log, "whois_error")
			case !slices.Contains(tags, tag):
				deny(w, log, "tag_missing")
			default:
				next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), tagsKey, tags)))
			}
		})
	}
}

// LoginFrom returns the login stored by UserGate ("" if none).
func LoginFrom(ctx context.Context) string {
	s, _ := ctx.Value(loginKey).(string)
	return s
}

// NodeTagsFrom returns the tags stored by TagGate (nil if none).
func NodeTagsFrom(ctx context.Context) []string {
	t, _ := ctx.Value(tagsKey).([]string)
	return t
}

// RequireJSON refuses state-changing requests that are not application/json (422), which a
// cross-site HTML form cannot send. GET and HEAD pass untouched.
func RequireJSON(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
			ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
			if strings.ToLower(strings.TrimSpace(ct)) != "application/json" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnprocessableEntity)
				w.Write([]byte(`{"detail":"Content-Type must be application/json"}`))
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
