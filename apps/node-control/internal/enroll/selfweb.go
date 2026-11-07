package enroll

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

// SelfWeb is the self-enrolment web surface on the public listener (#102):
//
//	GET  /me              the person's page: sign-in button, or their devices and the enrol form
//	GET  /oidc/login      starts the code flow (PKCE) and redirects to the issuer
//	GET  /oidc/callback   completes it and sets the session cookie
//	POST /me/devices      enrols a device (form: name, client, csrf); shows the result once
//	POST /me/devices/{id}/revoke   revokes an own device (form: csrf)
//	POST /me/logout       ends the session (form: csrf)
//
// The session is an HttpOnly, Secure, SameSite=Lax cookie with a random id; the server keeps only its hash. Every
// POST needs the session's CSRF token in the form. Pages are server-rendered with html/template; no script runs.
type SelfWeb struct {
	self *Self
	rp   interface {
		AuthURL(oidc.Flow) string
		Exchange(ctx context.Context, f oidc.Flow, state, code, groupsClaim string) (oidc.Identity, error)
	}
	link func() IngressConfig
}

const (
	sessionCookie = "__Host-hl_session"
	flowTTL       = 10 * time.Minute
)

//go:embed static/me.html
var meHTML string

var meTmpl = template.Must(template.New("me").Funcs(template.FuncMap{
	"date": func(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") },
}).Parse(meHTML))

// Register adds the routes to the public mux, ahead of the enrolment catch-all.
func (s *SelfWeb) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /me", s.page)
	mux.HandleFunc("GET /oidc/login", s.login)
	mux.HandleFunc("GET /oidc/callback", s.callback)
	mux.HandleFunc("POST /me/devices", s.form(s.enrol))
	mux.HandleFunc("POST /me/devices/{id}/revoke", s.form(s.revoke))
	mux.HandleFunc("POST /me/logout", s.form(s.logout))
}

func secHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

type meView struct {
	SignedIn  bool
	Email     string
	CSRF      string
	Devices   []meDevice
	Clients   []string
	Max       int
	Error     string
	Enrolment *Enrolment
	Name      string
}

type meDevice struct {
	ID      int64
	Name    string
	Client  string
	Expires int64
	Active  bool
}

func (s *SelfWeb) render(w http.ResponseWriter, status int, v meView) {
	secHeaders(w)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := meTmpl.Execute(w, v); err != nil {
		slog.Warn("self-enrolment page failed", "error_type", errType(err))
	}
}

// session returns the subject and CSRF token of the request's session.
func (s *SelfWeb) session(r *http.Request) (sub, csrf string, ok bool) {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return "", "", false
	}
	sub, csrf, ok, err = s.self.users.Session(c.Value)
	if err != nil || !ok {
		return "", "", false
	}
	// A locked person's sessions are deleted by the lock; this check also covers a lock made in another process.
	if u, err := s.self.users.User(sub); err != nil || u.Locked {
		return "", "", false
	}
	return sub, csrf, true
}

func (s *SelfWeb) view(sub, csrf string) (meView, error) {
	u, err := s.self.users.User(sub)
	if err != nil {
		return meView{}, err
	}
	devs, err := s.self.Devices(sub)
	if err != nil {
		return meView{}, err
	}
	v := meView{SignedIn: true, Email: u.Email, CSRF: csrf, Clients: Clients, Max: s.self.cfg.MaxDevices}
	for _, d := range devs {
		v.Devices = append(v.Devices, meDevice{d.ID, d.Name, d.Client, d.ExpiresAt, s.self.active(d)})
	}
	return v, nil
}

func (s *SelfWeb) page(w http.ResponseWriter, r *http.Request) {
	sub, csrf, ok := s.session(r)
	if !ok {
		s.render(w, http.StatusOK, meView{}) // no text from the URL: a crafted link must not put words on the page
		return
	}
	v, err := s.view(sub, csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.render(w, http.StatusOK, v)
}

func (s *SelfWeb) fail(w http.ResponseWriter, err error) {
	slog.Warn("self-enrolment request failed", "error_type", errType(err))
	s.render(w, http.StatusInternalServerError, meView{Error: "Something went wrong. Try again later."})
}

func (s *SelfWeb) login(w http.ResponseWriter, r *http.Request) {
	f := oidc.NewFlow()
	if err := s.self.users.SaveFlow(f.State, f.Nonce, f.Verifier, flowTTL); err != nil {
		s.fail(w, err)
		return
	}
	secHeaders(w)
	http.Redirect(w, r, s.rp.AuthURL(f), http.StatusFound)
}

// userError is the text shown for a refused login; other errors are not detailed.
func userError(err error) string {
	switch {
	case errors.Is(err, ErrLocked), errors.Is(err, ErrNotInGroup), errors.Is(err, oidc.ErrState):
		return err.Error()
	case errors.Is(err, oidc.ErrDenied):
		return "The sign-in was cancelled or refused."
	}
	return "The sign-in failed. Try again."
}

func (s *SelfWeb) callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nonce, verifier, ok, err := s.self.users.TakeFlow(q.Get("state"))
	if err != nil {
		s.fail(w, err)
		return
	}
	if !ok {
		s.render(w, http.StatusBadRequest, meView{Error: userError(oidc.ErrState)})
		return
	}
	if q.Get("error") != "" {
		s.render(w, http.StatusForbidden, meView{Error: userError(oidc.ErrDenied)})
		return
	}
	id, err := s.rp.Exchange(r.Context(), oidc.Flow{State: q.Get("state"), Nonce: nonce, Verifier: verifier}, q.Get("state"), q.Get("code"), s.self.cfg.GroupsClaim)
	if err == nil {
		err = s.self.Login(id)
	}
	if err != nil {
		if !errors.Is(err, ErrLocked) && !errors.Is(err, ErrNotInGroup) {
			slog.Warn("oidc login failed", "error_type", errType(err), "err", err.Error())
		}
		s.render(w, http.StatusForbidden, meView{Error: userError(err)})
		return
	}
	sid, _, err := s.self.users.NewSession(id.Subject, s.self.cfg.SessionTTL)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sid, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.self.cfg.SessionTTL / time.Second)})
	secHeaders(w)
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

// form wraps a POST: a session, a form body of bounded size and the session's CSRF token are required.
func (s *SelfWeb) form(h func(w http.ResponseWriter, r *http.Request, sub, csrf string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sub, csrf, ok := s.session(r)
		if !ok {
			secHeaders(w)
			http.Redirect(w, r, "/me", http.StatusSeeOther)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4096)
		if err := r.ParseForm(); err != nil || !constantEq(r.PostForm.Get("csrf"), csrf) {
			s.render(w, http.StatusForbidden, meView{Error: "The form expired. Reload the page and try again."})
			return
		}
		h(w, r, sub, csrf)
	}
}

func constantEq(a, b string) bool {
	return a != "" && len(a) == len(b) && hashID(a) == hashID(b)
}

func (s *SelfWeb) enrol(w http.ResponseWriter, r *http.Request, sub, csrf string) {
	name := strings.TrimSpace(r.PostForm.Get("name"))
	client := r.PostForm.Get("client")
	v, err := s.view(sub, csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	valid := false
	for _, c := range Clients {
		valid = valid || c == client
	}
	if name == "" || len(name) > 100 || !valid {
		v.Error = "Give the device a name (1 to 100 characters) and choose a client."
		s.render(w, http.StatusUnprocessableEntity, v)
		return
	}
	e, err := s.self.Enrol(r.Context(), sub, name, client, s.link())
	switch {
	case errors.Is(err, ErrDeviceLimit), errors.Is(err, ErrLocked):
		v.Error = err.Error()
		s.render(w, http.StatusConflict, v)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	if v, err = s.view(sub, csrf); err != nil {
		s.fail(w, err)
		return
	}
	v.Enrolment, v.Name = &e, name
	s.render(w, http.StatusOK, v)
}

func (s *SelfWeb) revoke(w http.ResponseWriter, r *http.Request, sub, _ string) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err == nil {
		err = s.self.RevokeOwn(r.Context(), sub, id)
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, err)
		return
	}
	secHeaders(w)
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func (s *SelfWeb) logout(w http.ResponseWriter, r *http.Request, _, _ string) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		_ = s.self.users.EndSession(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: -1})
	secHeaders(w)
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}
