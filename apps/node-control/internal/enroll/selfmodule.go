package enroll

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

// WithSelfHeadscale gives the module the headscale client that self-enrolment uses (#102).
func WithSelfHeadscale(hs SelfHeadscale) Option { return func(m *Module) { m.selfHS = hs } }

// selfEnabled says whether OIDC self-enrolment is configured.
func selfEnabled(env core.Env) bool { return env.Getenv("OIDC_ISSUER") != "" }

// selfConfig reads the OIDC_* settings.
func selfConfig(env core.Env) (oidc.Config, SelfConfig, string, error) {
	var missing []string
	for _, k := range []string{"OIDC_ISSUER", "OIDC_CLIENT_ID", "OIDC_REDIRECT_URL", "OIDC_DB"} {
		if env.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return oidc.Config{}, SelfConfig{}, "", fmt.Errorf("OIDC self-enrolment enabled (OIDC_ISSUER set) but missing: %s", strings.Join(missing, ", "))
	}
	redir, err := url.Parse(env.Getenv("OIDC_REDIRECT_URL"))
	if err != nil || redir.Scheme != "https" || redir.Host == "" || redir.Path != "/oidc/callback" {
		return oidc.Config{}, SelfConfig{}, "", errors.New("OIDC_REDIRECT_URL must be https://<link host>/oidc/callback")
	}
	rp := oidc.Config{Issuer: env.Getenv("OIDC_ISSUER"), ClientID: env.Getenv("OIDC_CLIENT_ID"),
		ClientSecret: env.Getenv("OIDC_CLIENT_SECRET"), RedirectURL: redir.String()}
	if s := env.Getenv("OIDC_SCOPES"); s != "" {
		rp.Scopes = strings.Fields(s)
	}
	sc := SelfConfig{AllowedGroups: env.List("OIDC_ALLOWED_GROUPS"), GroupsClaim: env.Getenv("OIDC_GROUPS_CLAIM")}
	ints := []struct {
		key    string
		lo, hi int
		set    func(int)
	}{
		{"OIDC_MAX_DEVICES", 1, 100, func(n int) { sc.MaxDevices = n }},
		{"OIDC_DEVICE_DAYS", 1, 3650, func(n int) { sc.DeviceTTL = time.Duration(n) * 24 * time.Hour }},
		{"OIDC_KEY_MINUTES", 5, 1440, func(n int) { sc.KeyTTL = time.Duration(n) * time.Minute }},
		{"OIDC_RECHECK_MINUTES", 5, 1440, func(n int) { sc.Recheck = time.Duration(n) * time.Minute }},
		{"OIDC_SESSION_MINUTES", 5, 1440, func(n int) { sc.SessionTTL = time.Duration(n) * time.Minute }},
	}
	for _, x := range ints {
		v := strings.TrimSpace(env.Getenv(x.key))
		if v == "" {
			continue
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < x.lo || n > x.hi {
			return oidc.Config{}, SelfConfig{}, "", fmt.Errorf("%s must be a whole number from %d to %d", x.key, x.lo, x.hi)
		}
		x.set(n)
	}
	switch v := strings.ToLower(strings.TrimSpace(env.Getenv("OIDC_INVITE_ONLY"))); v {
	case "", "false", "0", "no":
	case "true", "1", "yes":
		sc.InviteOnly = true
	default:
		return oidc.Config{}, SelfConfig{}, "", errors.New(errInviteOnlyValue)
	}
	switch v := strings.ToLower(strings.TrimSpace(env.Getenv("OIDC_INVITE_REQUIRE_VERIFIED"))); v {
	case "", "true", "1", "yes":
	case "false", "0", "no":
		sc.InviteAnyEmail = true
	default:
		return oidc.Config{}, SelfConfig{}, "", errors.New(errInviteVerifiedValue)
	}
	sc.defaults()
	return rp, sc, env.Getenv("OIDC_DB"), nil
}

// registerSelf sets up self-enrolment after the enrolment store is open. It is a no-op without OIDC_ISSUER.
func (m *Module) registerSelf(r *core.Routes, key []byte) error {
	if !selfEnabled(m.env) {
		return nil
	}
	rpCfg, sc, path, err := selfConfig(m.env)
	if err != nil {
		return err
	}
	if m.selfHS == nil {
		return errors.New("OIDC self-enrolment needs the headscale API (HEADSCALE_LOGIN_SERVER and HEADSCALE_API_KEY)")
	}
	if host := m.reg.LinkConfig().Link().Host; !strings.EqualFold(host, mustHost(rpCfg.RedirectURL)) {
		return fmt.Errorf("OIDC_REDIRECT_URL must be on the link host %s, where the public listener is reached", host)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rp, err := oidc.Discover(ctx, rpCfg)
	if err != nil {
		return err
	}
	icons, err := LoadIcons(m.env.Getenv("ENROLL_CLIENT_ICON_DIR"))
	if err != nil {
		return err
	}
	users, err := OpenSelfStore(path, key, m.now)
	if err != nil {
		return err
	}
	admin := m.admin
	m.self = &Self{cfg: sc, users: users, store: m.store, hs: m.selfHS, idp: rp, issuer: rp.Issuer(), push: func(ctx context.Context) {
		SyncAll(ctx, m.store, m.reg.Current(ctx, false).Cfg, m.reg.Current(ctx, false).SyncTargets)
	}}
	admin.self = m.self
	(&SelfWeb{self: m.self, rp: rp, link: m.reg.LinkConfig, icons: icons}).Register(r.Public)
	r.Admin.HandleFunc("GET /enroll/people", admin.listPeople)
	r.Admin.HandleFunc("POST /enroll/people/{sub}/lock", admin.jsonOnly(admin.lockPerson))
	r.Admin.HandleFunc("POST /enroll/people/{sub}/unlock", admin.jsonOnly(admin.unlockPerson))
	admin.registerInvites(r.Admin)
	return nil
}

// errInviteOnlyValue says what each value of OIDC_INVITE_ONLY does.
const errInviteOnlyValue = "OIDC_INVITE_ONLY must be true or false (true: only an invitation " +
	"can add a device; false, the default: the \"Add a device\" form of /me stays)"

// errInviteVerifiedValue says what each value of OIDC_INVITE_REQUIRE_VERIFIED does.
const errInviteVerifiedValue = "OIDC_INVITE_REQUIRE_VERIFIED must be true or false (true, the " +
	"default: an invitation needs email_verified true in the ID token; false: a token without " +
	"the claim is also accepted)"

// Invitation limits.
const (
	defaultInviteDays = 7
	maxInviteDays     = 90
	defaultInviteName = "Invited device"
	minEmailLen       = 3
	maxEmailLen       = 254
	maxDeviceName     = 100
	day               = 24 * time.Hour
)

func (a *Admin) registerInvites(mux *core.Mux) {
	mux.HandleFunc("GET /enroll/invites", a.listInvites)
	mux.HandleFunc("POST /enroll/invites", a.jsonOnly(a.createInvite))
	mux.HandleFunc("POST /enroll/invites/{id}/revoke", a.jsonOnly(a.revokeInvite))
}

// listInvites answers GET /enroll/invites: every invitation, newest first, never a token.
func (a *Admin) listInvites(w http.ResponseWriter, r *http.Request) {
	invs, err := a.self.users.Invites(r.Context())
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invites": invs})
}

// createInvite answers POST /enroll/invites {"email", "device_name"?, "days"?}: an invitation for
// one email address, with its link. The link is in this answer only.
func (a *Admin) createInvite(w http.ResponseWriter, r *http.Request) {
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var v validator
	email := strings.TrimSpace(v.str(obj, "email", minEmailLen, maxEmailLen))
	name := defaultInviteName
	if _, present := obj["device_name"]; present {
		name = strings.TrimSpace(v.str(obj, "device_name", 1, maxDeviceName))
	}
	days := int64(defaultInviteDays)
	if _, present := obj["days"]; present {
		days = v.integer(obj, "days", 1, maxInviteDays)
	}
	if len(v.errs) > 0 {
		invalid(w, v.errs...)
		return
	}
	at := strings.LastIndexByte(email, '@')
	if at < 1 || at == len(email)-1 || strings.ContainsAny(email, " \t\r\n<>\"") {
		detail(w, http.StatusUnprocessableEntity, "email must be an email address")
		return
	}
	if name == "" {
		detail(w, http.StatusUnprocessableEntity, "device_name must not be blank")
		return
	}
	ttl := time.Duration(days) * day
	inv, token, err := a.self.users.NewInvite(r.Context(), email, name, a.actor(r), ttl)
	if err != nil {
		a.internal(w, err)
		return
	}
	a.audit(r, "invite.create", fmt.Sprintf("invitation %d for %s, until %s",
		inv.ID, inv.Email, expiryText(inv.ExpiresAt)))
	link := "https://" + a.reg.LinkConfig().Link().Host + "/i/" + token
	writeJSON(w, http.StatusOK, map[string]any{"id": inv.ID, "email": inv.Email,
		"device_name": inv.DeviceName, "expires_at": inv.ExpiresAt, "url": link})
}

// revokeInvite answers POST /enroll/invites/{id}/revoke: an unused invitation can no longer be
// spent.
func (a *Admin) revokeInvite(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		detail(w, http.StatusNotFound, "unknown invitation")
		return
	}
	switch err := a.self.users.RevokeInvite(r.Context(), id); {
	case errors.Is(err, ErrNotFound):
		detail(w, http.StatusNotFound, "unknown invitation")
	case errors.Is(err, ErrInviteUsed):
		detail(w, http.StatusConflict, "the invitation made a device; revoke the device instead")
	case err != nil:
		a.internal(w, err)
	default:
		a.audit(r, "invite.revoke", fmt.Sprintf("invitation %d", id))
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func mustHost(u string) string {
	p, _ := url.Parse(u)
	if p == nil {
		return ""
	}
	return p.Hostname()
}

// listPeople answers GET /enroll/people: the self-enrolled people, never a token.
func (a *Admin) listPeople(w http.ResponseWriter, r *http.Request) {
	users, err := a.self.users.Users()
	if err != nil {
		a.internal(w, err)
		return
	}
	devs, err := a.store.ListDevices()
	if err != nil {
		a.internal(w, err)
		return
	}
	active := map[string]int{}
	for _, d := range devs {
		if sub, ok := subjectOfOwner(d.Owner); ok && a.self.active(d) {
			active[sub]++
		}
	}
	type row struct {
		Subject     string `json:"subject"`
		Owner       string `json:"owner"`
		Email       string `json:"email"`
		Locked      bool   `json:"locked"`
		LockedBy    string `json:"locked_by,omitempty"`
		LockReason  string `json:"lock_reason,omitempty"`
		Devices     int    `json:"active_devices"`
		LastLogin   int64  `json:"last_login"`
		LastCheck   int64  `json:"last_check"`
		CheckFailed int64  `json:"check_failed,omitempty"`
	}
	out := make([]row, 0, len(users))
	for _, u := range users {
		out = append(out, row{u.Subject, ownerOf(u.Subject), u.Email, u.Locked, u.LockedBy, u.LockReason, active[u.Subject],
			u.LastLogin, u.LastCheck, u.CheckFailed})
	}
	writeJSON(w, http.StatusOK, map[string]any{"people": out})
}

func (a *Admin) lockPerson(w http.ResponseWriter, r *http.Request) {
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var reason string
	if _, present := obj["reason"]; present {
		var v validator
		reason = v.str(obj, "reason", 0, 200)
		if len(v.errs) > 0 {
			invalid(w, v.errs...)
			return
		}
	}
	err := a.self.Lock(context.WithoutCancel(r.Context()), r.PathValue("sub"), a.actor(r), reason)
	switch {
	case errors.Is(err, ErrNotFound):
		detail(w, http.StatusNotFound, "unknown person")
	case err != nil:
		// The lock is recorded and the relay devices are revoked; a headscale step failed. Say so: the operator
		// must retry, or expire the nodes by hand.
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "detail": "locked, but a tailnet step failed: " + trimErr(err)})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func (a *Admin) unlockPerson(w http.ResponseWriter, r *http.Request) {
	switch err := a.self.Unlock(r.PathValue("sub"), a.actor(r)); {
	case errors.Is(err, ErrNotFound):
		detail(w, http.StatusNotFound, "unknown person")
	case err != nil:
		a.internal(w, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	}
}

func trimErr(err error) string {
	s := strings.ReplaceAll(err.Error(), "\n", "; ")
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
