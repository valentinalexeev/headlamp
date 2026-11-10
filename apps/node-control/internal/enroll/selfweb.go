package enroll

import (
	"context"
	_ "embed"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
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
//	POST /me/devices      enrols a device (form: name, csrf); shows the tailnet key once
//	                      and the app picker
//	GET  /me/devices/{id}/app/{app}  one app with Safari's Smart App Banner (#271)
//	GET  /me/devices/{id} the app picker of an own, active device: one import link per app
//	                      the site offers
//	POST /me/devices/{id}/revoke   revokes an own device (form: csrf)
//	POST /me/logout       ends the session (form: csrf)
//	GET  /me/icons/{app}  the app icons of the site (ENROLL_CLIENT_ICON_DIR)
//	GET  /i/{token}       an invitation: keeps the token in a short cookie and starts the
//	                      sign-in; the callback spends it when the verified email of the
//	                      sign-in is the invitation's
//
// The session is an HttpOnly, Secure, SameSite=Lax cookie with a random id; the server keeps only its hash. Every
// POST needs the session's CSRF token in the form. Pages are server-rendered with html/template; no script runs.
type SelfWeb struct {
	self *Self
	rp   interface {
		AuthURL(oidc.Flow) string
		Exchange(ctx context.Context, f oidc.Flow, state, code, groupsClaim string) (oidc.Identity, error)
	}
	link  func() IngressConfig
	icons map[string]Icon
}

const (
	sessionCookie = "__Host-hl_session"
	inviteCookie  = "__Host-hl_invite"
	flowCookie    = "__Host-hl_flow" // the state of the sign-in this browser started
	flowTTL       = 10 * time.Minute
)

//go:embed static/me.html
var meHTML string

//go:embed static/pick.html
var pickHTML string

//go:embed static/app.html
var appHTML string

var tmplFuncs = template.FuncMap{
	"date": func(ts int64) string { return time.Unix(ts, 0).UTC().Format("2006-01-02") },
}

var (
	meTmpl   = template.Must(template.New("me").Funcs(tmplFuncs).Parse(meHTML))
	pickTmpl = template.Must(template.New("pick").Funcs(tmplFuncs).Parse(pickHTML))
	appTmpl  = template.Must(template.New("app").Funcs(tmplFuncs).Parse(appHTML))
)

// Register adds the routes to the public mux, ahead of the enrolment catch-all.
func (s *SelfWeb) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /me", s.page)
	mux.HandleFunc("GET /oidc/login", s.login)
	mux.HandleFunc("GET /oidc/callback", s.callback)
	mux.HandleFunc("POST /me/devices", s.form(s.enrol))
	mux.HandleFunc("GET /me/devices/{id}", s.pickPage)
	mux.HandleFunc("GET /me/devices/{id}/app/{app}", s.appPage)
	mux.HandleFunc("POST /me/devices/{id}/revoke", s.form(s.revoke))
	mux.HandleFunc("POST /me/logout", s.form(s.logout))
	mux.HandleFunc("GET /me/icons/{app}", s.icon)
	mux.HandleFunc("GET /i/{token}", s.invite)
}

func secHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; "+
		"img-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
}

type meView struct {
	SignedIn   bool
	Email      string
	CSRF       string
	Devices    []meDevice
	Max        int
	InviteOnly bool
	Error      string
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
	v := meView{SignedIn: true, Email: u.Email, CSRF: csrf, Max: s.self.cfg.MaxDevices,
		InviteOnly: s.self.cfg.InviteOnly}
	for _, d := range devs {
		v.Devices = append(v.Devices, meDevice{d.ID, d.Name, d.Client, d.ExpiresAt, s.self.active(d)})
	}
	return v, nil
}

func (s *SelfWeb) page(w http.ResponseWriter, r *http.Request) {
	// The session cookie is host-only and made on link_host: /me on another front goes there
	// (#270).
	if s.onLinkHost(w, r, "/me") {
		return
	}
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

// onLinkHost sends the browser to the same path on link_host when the request came through a
// front with another host (#270). The OIDC redirect_uri is on link_host (OIDC_REDIRECT_URL), and
// the flow and invitation cookies are host-only (__Host-), so a sign-in must start on link_host,
// or the callback finds no flow cookie. It reports whether it redirected.
//
// The target is built from the configured link_host and the route's own path, never from the
// request URL: the redirect cannot be steered to another site.
func (s *SelfWeb) onLinkHost(w http.ResponseWriter, r *http.Request, path string) bool {
	link := s.link().LinkHost
	if link == "" || sameHost(r.Host, link) {
		return false
	}
	target := url.URL{Scheme: "https", Host: link, Path: path}
	secHeaders(w)
	http.Redirect(w, r, target.String(), http.StatusFound)
	return true
}

// sameHost compares a request Host with link_host. A port on either side counts only when both have
// one: a front proxies with the plain name, and link_host is a plain name in production.
func sameHost(reqHost, link string) bool {
	rh, rp, rerr := net.SplitHostPort(reqHost)
	lh, lp, lerr := net.SplitHostPort(link)
	if rerr != nil {
		rh = reqHost
	}
	if lerr != nil {
		lh = link
	}
	if rerr == nil && lerr == nil && rp != lp {
		return false
	}
	return strings.EqualFold(rh, lh)
}

// inviteTokenRe is the shape of an invitation token in /i/<token>.
var inviteTokenRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *SelfWeb) login(w http.ResponseWriter, r *http.Request) {
	if s.onLinkHost(w, r, "/oidc/login") {
		return
	}
	f := oidc.NewFlow()
	if err := s.self.users.SaveFlow(f.State, f.Nonce, f.Verifier, flowTTL); err != nil {
		s.fail(w, err)
		return
	}
	// The callback must come back to the browser that started the sign-in: a callback URL of
	// someone else's sign-in must not log this browser in to that account.
	http.SetCookie(w, &http.Cookie{Name: flowCookie, Value: f.State, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(flowTTL / time.Second)})
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
	// The invitation and the flow cookies are for this one callback, whatever its result.
	invite := ""
	if c, err := r.Cookie(inviteCookie); err == nil {
		invite = c.Value
		clearCookie(w, inviteCookie)
	}
	fc, ferr := r.Cookie(flowCookie)
	clearCookie(w, flowCookie)
	if ferr != nil || !constantEq(fc.Value, q.Get("state")) {
		s.render(w, http.StatusBadRequest, meView{Error: userError(oidc.ErrState)})
		return
	}
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
	sid, csrf, err := s.self.users.NewSession(id.Subject, s.self.cfg.SessionTTL)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: sid, Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: int(s.self.cfg.SessionTTL / time.Second)})
	if invite != "" {
		s.redeem(w, r, id, invite, csrf)
		return
	}
	secHeaders(w)
	http.Redirect(w, r, "/me", http.StatusSeeOther)
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", Secure: true, HttpOnly: true,
		SameSite: http.SameSiteLaxMode, MaxAge: -1})
}

// invite answers /i/{token}: it keeps the token in a short cookie and starts the sign-in, which
// spends it. A link preview gets an empty 404, as on the other public links; an unknown or spent
// invitation says so at once.
func (s *SelfWeb) invite(w http.ResponseWriter, r *http.Request) {
	token := r.PathValue("token")
	if token == "" || isPreview(r.Header.Get("User-Agent")) {
		notFound(w)
		return
	}
	// The invitation cookie must be on the host where the sign-in ends (#270). The token is 64 hex
	// characters (the route and the front allow nothing else), so the path stays a fixed shape.
	if !inviteTokenRe.MatchString(token) {
		notFound(w)
		return
	}
	if s.onLinkHost(w, r, "/i/"+token) {
		return
	}
	inv, err := s.self.users.InviteByToken(r.Context(), token)
	if err != nil && !errors.Is(err, ErrNotFound) {
		s.fail(w, err)
		return
	}
	if err != nil || inv.State != InvitePending {
		s.render(w, http.StatusNotFound, meView{Error: capitalize(ErrInviteInvalid.Error())})
		return
	}
	http.SetCookie(w, &http.Cookie{Name: inviteCookie, Value: token, Path: "/", Secure: true,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, MaxAge: int(flowTTL / time.Second)})
	secHeaders(w)
	http.Redirect(w, r, "/oidc/login", http.StatusFound)
}

// redeem spends the invitation after a sign-in and shows the new device's app picker with its
// tailnet key.
func (s *SelfWeb) redeem(
	w http.ResponseWriter, r *http.Request, id oidc.Identity, token, csrf string,
) {
	e, inv, err := s.self.Redeem(r.Context(), id, token, s.link())
	if err != nil {
		status := http.StatusConflict
		switch {
		case errors.Is(err, ErrInviteInvalid), errors.Is(err, ErrInviteOtherEmail),
			errors.Is(err, ErrEmailUnverified), errors.Is(err, ErrDeviceLimit),
			errors.Is(err, ErrLocked), errors.Is(err, ErrHSUserAmbiguous):
		default:
			s.fail(w, err)
			return
		}
		v, verr := s.view(id.Subject, csrf)
		if verr != nil {
			s.fail(w, verr)
			return
		}
		v.Error = capitalize(err.Error())
		s.render(w, status, v)
		return
	}
	d, err := s.self.store.GetDevice(e.DeviceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderPick(w, r, id.Subject, csrf, d, &e, inv.DeviceName)
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:] + "."
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
	// The form has no app field any more: the person picks the app on the next page. An older form
	// still sends one.
	client := r.PostForm.Get("client")
	if client == "" {
		client = defaultSelfClient
	}
	v, err := s.view(sub, csrf)
	if err != nil {
		s.fail(w, err)
		return
	}
	if s.self.cfg.InviteOnly {
		v.Error = "You need an invitation to add a device. Ask an administrator."
		s.render(w, http.StatusForbidden, v)
		return
	}
	_, valid := ClientByID(client)
	if name == "" || len(name) > 100 || !valid {
		v.Error = "Give the device a name (1 to 100 characters)."
		s.render(w, http.StatusUnprocessableEntity, v)
		return
	}
	e, err := s.self.Enrol(r.Context(), sub, name, client, s.link())
	switch {
	case errors.Is(err, ErrDeviceLimit), errors.Is(err, ErrLocked),
		errors.Is(err, ErrHSUserAmbiguous):
		v.Error = err.Error()
		s.render(w, http.StatusConflict, v)
		return
	case err != nil:
		s.fail(w, err)
		return
	}
	d, err := s.self.store.GetDevice(e.DeviceID)
	if err != nil {
		s.fail(w, err)
		return
	}
	s.renderPick(w, r, sub, csrf, d, &e, name)
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
