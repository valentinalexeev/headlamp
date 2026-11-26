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
		switch _, s, st := m.lookup(r, login); st {
		case stateLive:
			if m.sensitive(r) && !m.fresh(s) {
				m.askReauth(w, r, login)
				return
			}
			next.ServeHTTP(w, r)
		case stateOther:
			http.Error(w, "this session belongs to another tailnet login: "+
				"sign out and sign in again", http.StatusForbidden)
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

// sensitive tells whether r goes to a route that asks for a recent sign-in (#469).
func (m *Module) sensitive(r *http.Request) bool {
	if m.cfg.Reauth <= 0 || m.mux == nil || len(m.cfg.Sensitive) == 0 {
		return false
	}
	_, pattern := m.mux.Handler(r)
	return m.cfg.Sensitive[pattern]
}

// fresh tells whether the sign-in of s is recent enough for a sensitive route.
func (m *Module) fresh(s session) bool {
	return !s.authAt.IsZero() && m.cfg.Now().Sub(s.authAt) <= m.cfg.Reauth
}

// askReauth sends a page load to a new sign-in (prompt=login, max_age), and gives an API call 401
// with the sign-in link. The request is not run: after the sign-in, the person repeats it.
func (m *Module) askReauth(w http.ResponseWriter, r *http.Request, login string) {
	m.cfg.Log.Info("admin re-authentication required", "login", login,
		"method", r.Method, "path", r.URL.Path)
	if browserGet(r) {
		http.Redirect(w, r, "/auth/login?reauth=1&next="+url.QueryEscape(r.URL.RequestURI()),
			http.StatusFound)
		return
	}
	// An API call returns to the page it came from, when that is a page of this console.
	next := "/"
	if ref, err := url.Parse(r.Referer()); err == nil && ref.Host == r.Host {
		next = localNext(ref.RequestURI())
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":  "re-authentication required: sign in again, then repeat the action",
		"reauth": true,
		"login":  "/auth/login?reauth=1&next=" + url.QueryEscape(next),
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
