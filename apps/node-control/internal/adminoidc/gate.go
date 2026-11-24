package adminoidc

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Required mode (#448). With Config.Required, the sign-in is a second factor of the console on
// HTTPS: every route except /auth/* and /healthz needs the tailnet login (WhoIs: a human,
// untagged caller) and a live session of that same login. The plain listener (:80) is not wrapped:
// it is the break-glass way in, with ADMIN_USERS only, one factor.

// DefaultIdle is the idle timeout of a session in required mode.
const DefaultIdle = 30 * time.Minute

// state is the result of looking up the session of a request.
type state int

const (
	stateNone  state = iota // no cookie, unknown, expired or idle session
	stateOther              // a live session of another tailnet login
	stateLive
)

// Gate returns next behind the required sign-in. With Required off it returns next unchanged.
func (m *Module) Gate(next http.Handler) http.Handler {
	if !m.cfg.Required {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/healthz" || strings.HasPrefix(p, "/auth/") {
			next.ServeHTTP(w, r)
			return
		}
		login := m.human(r)
		if login == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch _, _, st := m.lookup(r, login); st {
		case stateLive:
			next.ServeHTTP(w, r)
		case stateOther:
			http.Error(w, "this session belongs to another tailnet login: sign out and sign in again",
				http.StatusForbidden)
		default:
			if browserGet(r) {
				http.Redirect(w, r, "/auth/login?next="+url.QueryEscape(r.URL.RequestURI()),
					http.StatusFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "sign-in required",
				"login": "/auth/login"})
		}
	})
}

// browserGet tells whether r is a page load of a browser (it can follow a redirect to sign in).
func browserGet(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	return r.Header.Get("Sec-Fetch-Mode") == "navigate" ||
		strings.Contains(r.Header.Get("Accept"), "text/html")
}

// localNext is next when it is a path on this console, else "/". It refuses an absolute URL, a
// scheme-relative "//host", a backslash (browsers read "/\host" as "//host"), control bytes and
// the sign-in routes themselves.
func localNext(next string) string {
	if next == "" || next[0] != '/' || strings.HasPrefix(next, "//") ||
		strings.Contains(next, "\\") {
		return "/"
	}
	for _, c := range next {
		if c < 0x20 || c == 0x7f {
			return "/"
		}
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil ||
		!strings.HasPrefix(u.Path, "/") || strings.HasPrefix(u.Path, "//") ||
		strings.HasPrefix(u.Path, "/auth/") {
		return "/"
	}
	return next
}
