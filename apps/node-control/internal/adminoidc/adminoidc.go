// Package adminoidc signs an admin in to the console with OIDC (#366), so stored grants to OIDC
// groups
// apply. It does not replace the tailnet check: every admin route still asks WhoIs, and a tagged
// caller is still refused. The sign-in only adds the groups of the ID token to the caller's tailnet
// login, and only when the person who signed in is that login (ADMIN_OIDC_MATCH=login): the
// verified
// email of the ID token must be the tailnet login, or, for a login without a domain ("alice@"), its
// local part.
//
// The console must be on HTTPS for this (the redirect URL and a Secure cookie): the serve module
// serves it at ADMIN_CONSOLE_NAME with the serve certificate.
package adminoidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

const (
	sessionCookie = "__Host-hl_admin"
	flowCookie    = "__Host-hl_admin_flow"
	flowTTL       = 10 * time.Minute
	// DefaultSession is the session length. There is no refresh: sign in again after it.
	DefaultSession = 8 * time.Hour
	maxSessions    = 1024
	maxFlows       = 256
	// tokenBytes is the length of a session id before encoding: 256 bits.
	tokenBytes = 32
)

// Provider is the part of an OIDC provider the sign-in uses (oidc.Provider; a fake in tests).
type Provider interface {
	AuthURL(f oidc.Flow) string
	Exchange(ctx context.Context, f oidc.Flow, gotState, code, groupsClaim string) (
		oidc.Identity, error)
}

// WhoIs answers who is calling: the tailnet login and tags of a remote address (core.Tailnet
// has it).
type WhoIs interface {
	WhoIs(ctx context.Context, remoteAddr string) (login string, tags []string, err error)
}

// Config is the sign-in configuration.
type Config struct {
	Provider    Provider
	GroupsClaim string
	Session     time.Duration
	Tailnet     WhoIs
	Log         *slog.Logger
	Now         func() time.Time
}

type session struct {
	login string
	until time.Time
}

// Module serves /auth/login, /auth/callback and /auth/logout on the console.
type Module struct {
	cfg      Config
	mu       sync.Mutex
	sessions map[string]session   // sha256(cookie) -> session
	flows    map[string]oidc.Flow // state -> flow, until used or expired
	flowAt   map[string]time.Time
}

// New returns the sign-in module.
func New(cfg Config) *Module {
	if cfg.Session <= 0 {
		cfg.Session = DefaultSession
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Module{cfg: cfg, sessions: map[string]session{}, flows: map[string]oidc.Flow{},
		flowAt: map[string]time.Time{}}
}

// Register adds the routes. They are tailnet-checked by the caller's WhoIs in the handlers, like
// every admin route: a tagged node or an unknown caller cannot sign in.
func (m *Module) Register(mux *core.Mux) {
	mux.HandleFunc("GET /auth/login", m.login)
	mux.HandleFunc("GET /auth/callback", m.callback)
	mux.HandleFunc("POST /auth/logout", m.logout)
	mux.HandleFunc("GET /auth/me", m.me)
}

// human is the normalised tailnet login of the caller, or "" for a tagged or unknown caller.
func (m *Module) human(r *http.Request) string {
	login, tags, err := m.cfg.Tailnet.WhoIs(r.Context(), r.RemoteAddr)
	l := authz.NormalizeLogin(login)
	if err != nil || len(tags) > 0 || l == "" || l == "tagged-devices" {
		return ""
	}
	return l
}

func random() string {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (m *Module) login(w http.ResponseWriter, r *http.Request) {
	if m.human(r) == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	f := oidc.NewFlow()
	now := m.cfg.Now()
	m.mu.Lock()
	for s, at := range m.flowAt {
		if now.Sub(at) > flowTTL {
			delete(m.flows, s)
			delete(m.flowAt, s)
		}
	}
	if len(m.flows) >= maxFlows {
		m.mu.Unlock()
		http.Error(w, "too many sign-ins in progress", http.StatusServiceUnavailable)
		return
	}
	m.flows[f.State], m.flowAt[f.State] = f, now
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: f.State, Path: "/", Secure: true,
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(flowTTL / time.Second)})
	http.Redirect(w, r, m.cfg.Provider.AuthURL(f), http.StatusFound)
}

// refusal is why the identity of the ID token is not the tailnet login (#446). The zero value means a match.
type refusal int

const (
	refusedNone refusal = iota
	refusedNoEmail
	refusedUnverified
	refusedOtherAccount
)

// reason is the log field value of r.
func (r refusal) reason() string {
	return [...]string{"", "no_email", "email_not_verified", "other_account"}[r]
}

// page is the text that the person sees. It never shows the email of the other account.
func (r refusal) page() string {
	switch r {
	case refusedNoEmail:
		return "sign-in refused: the ID token has no email address. Ask the IdP administrator to add " +
			"the email scope or claim to the console client."
	case refusedUnverified:
		return "sign-in refused: the IdP does not mark your email address as verified " +
			"(email_verified is false or missing). Ask the IdP administrator to send " +
			"email_verified=true for the console client."
	default:
		return "sign-in refused: the account you signed in with is not your tailnet login. " +
			"Sign out of the IdP and sign in with your tailnet account."
	}
}

// check tells whether the verified email of the ID token is the tailnet login, and if not, why.
func check(login string, id oidc.Identity) refusal {
	email := strings.ToLower(strings.TrimSpace(id.Email))
	if email == "" || !strings.Contains(email, "@") {
		return refusedNoEmail
	}
	if !id.EmailVerified {
		return refusedUnverified
	}
	if strings.Contains(login, "@") && !strings.HasSuffix(login, "@") {
		if login != email {
			return refusedOtherAccount
		}
		return refusedNone
	}
	local, _, _ := strings.Cut(email, "@")
	if strings.TrimSuffix(login, "@") != local {
		return refusedOtherAccount
	}
	return refusedNone
}

func (m *Module) callback(w http.ResponseWriter, r *http.Request) {
	login := m.human(r)
	if login == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	c, err := r.Cookie(flowCookie)
	state := r.URL.Query().Get("state")
	if err != nil || c.Value == "" ||
		subtle.ConstantTimeCompare([]byte(c.Value), []byte(state)) != 1 {
		http.Error(w, "sign-in expired or was started elsewhere: open /auth/login again",
			http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	f, ok := m.flows[state]
	at := m.flowAt[state]
	delete(m.flows, state)
	delete(m.flowAt, state)
	m.mu.Unlock()
	expire(w, flowCookie, false)
	if !ok || m.cfg.Now().Sub(at) > flowTTL {
		http.Error(w, "sign-in expired: open /auth/login again", http.StatusBadRequest)
		return
	}
	id, err := m.cfg.Provider.Exchange(r.Context(), f, state, r.URL.Query().Get("code"),
		m.cfg.GroupsClaim)
	if err != nil {
		m.cfg.Log.Warn("admin sign-in failed", "login", login, "err", err.Error())
		http.Error(w, "sign-in failed", http.StatusUnauthorized)
		return
	}
	if why := check(login, id); why != refusedNone {
		// No groups for this login. The log may name the other account; the page does not.
		attrs := []any{"login", login, "reason", why.reason()}
		if why == refusedOtherAccount {
			attrs = append(attrs, "email", strings.ToLower(strings.TrimSpace(id.Email)))
		}
		m.cfg.Log.Warn("admin sign-in refused", attrs...)
		http.Error(w, why.page(), http.StatusForbidden)
		return
	}
	sid := random()
	until := m.cfg.Now().Add(m.cfg.Session)
	m.mu.Lock()
	m.dropExpired()
	if len(m.sessions) >= maxSessions {
		m.mu.Unlock()
		http.Error(w, "too many sessions", http.StatusServiceUnavailable)
		return
	}
	m.sessions[hash(sid)] = session{login: login, until: until}
	m.mu.Unlock()
	authz.SetSessionGroups(login, id.Groups, until)
	m.cfg.Log.Info("admin signed in", "login", login, "groups", len(id.Groups),
		"until", until.UTC().Format(time.RFC3339))
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sid, Path: "/", Secure: true,
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: int(m.cfg.Session / time.Second)})
	http.Redirect(w, r, "/", http.StatusFound)
}

func (m *Module) dropExpired() {
	now := m.cfg.Now()
	for k, s := range m.sessions {
		if !s.until.After(now) {
			delete(m.sessions, k)
		}
	}
}

// sessionOf returns the live session of the request's cookie, bound to the caller's login.
func (m *Module) sessionOf(r *http.Request, login string) (string, session, bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return "", session{}, false
	}
	k := hash(c.Value)
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[k]
	if !ok || !s.until.After(m.cfg.Now()) || s.login != login {
		return "", session{}, false
	}
	return k, s, true
}

func (m *Module) logout(w http.ResponseWriter, r *http.Request) {
	login := m.human(r)
	if login == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	// Same-origin only: a cross-site form cannot sign an admin out.
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if k, _, ok := m.sessionOf(r, login); ok {
		m.mu.Lock()
		delete(m.sessions, k)
		m.mu.Unlock()
		authz.SetSessionGroups(login, nil, time.Time{})
		m.cfg.Log.Info("admin signed out", "login", login)
	}
	expire(w, sessionCookie, true)
	w.WriteHeader(http.StatusNoContent)
}

// me tells the console whether the caller is signed in, and until when.
func (m *Module) me(w http.ResponseWriter, r *http.Request) {
	login := m.human(r)
	if login == "" {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	out := meView{Login: login}
	if _, s, ok := m.sessionOf(r, login); ok {
		out.SignedIn, out.Until = true, s.until.UTC().Format(time.RFC3339)
		out.Groups = len(authz.SessionGroups(login))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

type meView struct {
	SignedIn bool   `json:"signed_in"`
	Login    string `json:"login"`
	Until    string `json:"until,omitempty"`
	Groups   int    `json:"groups,omitempty"`
}

// expire removes a cookie, with the same attributes it was set with.
func expire(w http.ResponseWriter, name string, strict bool) {
	if strict {
		http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true,
			HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}
