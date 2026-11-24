package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/adminoidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
	servemod "github.com/valentinalexeev/headlamp/apps/node-control/internal/serve"
)

// The admin console on HTTPS with an OIDC sign-in (#366). Off unless ADMIN_OIDC=1.
//
//	ADMIN_OIDC=1                  turn it on
//	ADMIN_CONSOLE_NAME            the console name, one label under SERVE_ZONE
//	                              (default console.<SERVE_ZONE>)
//	ADMIN_OIDC_ISSUER             the issuer (default OIDC_ISSUER)
//	ADMIN_OIDC_CLIENT_ID          the client id (required; a client of its own, with the
//	                              redirect URL below)
//	ADMIN_OIDC_CLIENT_SECRET      the client secret (optional for a public client)
//	ADMIN_OIDC_GROUPS_CLAIM       the groups claim (default OIDC_GROUPS_CLAIM, then "groups")
//	ADMIN_OIDC_SCOPES             the scopes (default "openid email profile groups")
//	ADMIN_OIDC_SESSION_HOURS      the session length, 1 to 24 (default 8)
//	ADMIN_OIDC_REQUIRED           1 (default): the console on HTTPS needs the sign-in as a second
//	                              factor (#448); 0: the sign-in only adds groups
//	ADMIN_OIDC_IDLE_MINUTES       required mode: end a session after this many minutes without a
//	                              request, 5 to 480 (default 30)
//
// The redirect URL is https://<ADMIN_CONSOLE_NAME>/auth/callback. It needs serve (SERVE_DB), whose
// certificate the console uses.

const adminOIDCTimeout = 30 * time.Second

type adminConsoleSetup struct {
	mod  *adminOIDCModule
	opts []core.Option
}

// adminOIDCModule adds the sign-in routes and sets the console name on serve when the node is up.
type adminOIDCModule struct {
	env                              core.Env
	serve                            *servemod.Module
	name                             string
	addrs                            func() []netip.Addr
	cfg                              adminoidc.Config
	issuer, clientID, secret, scopes string
	oidc                             *adminoidc.Module // set by Register
}

// gate wraps the handler of the console on HTTPS with the required sign-in (#448). Run calls it
// after Register, so m.oidc is set.
func (m *adminOIDCModule) gate(next http.Handler) http.Handler {
	if m.oidc == nil {
		panic("adminoidc: the gate is built before Register")
	}
	return m.oidc.Gate(next)
}

func (m *adminOIDCModule) Name() string { return "adminoidc" }

func (m *adminOIDCModule) Register(r *core.Routes) error {
	ctx, cancel := context.WithTimeout(context.Background(), adminOIDCTimeout)
	defer cancel()
	p, err := oidc.Discover(ctx, oidc.Config{Issuer: m.issuer, ClientID: m.clientID,
		ClientSecret: m.secret,
		RedirectURL:  "https://" + m.name + "/auth/callback", Scopes: strings.Fields(m.scopes),
		HTTP: &http.Client{Timeout: adminOIDCTimeout}})
	if err != nil {
		return fmt.Errorf("ADMIN_OIDC: issuer %s: %w", m.issuer, err)
	}
	m.cfg.Provider, m.cfg.Tailnet, m.cfg.Log = p, r.Tailnet, r.Log
	m.oidc = adminoidc.New(m.cfg)
	m.oidc.Register(r.Admin)
	return nil
}

func (m *adminOIDCModule) Start(context.Context) error {
	if err := m.serve.SetConsole(m.name, m.addrs()); err != nil {
		return fmt.Errorf("ADMIN_CONSOLE_NAME: %w", err)
	}
	return nil
}

// adminConsole reads the ADMIN_OIDC settings. It returns nil when ADMIN_OIDC is not 1.
func adminConsole(env core.Env, serve core.Module, addrs func() []netip.Addr) (
	*adminConsoleSetup, error,
) {
	if strings.TrimSpace(env.Getenv("ADMIN_OIDC")) != "1" {
		return nil, nil
	}
	sm, ok := serve.(*servemod.Module)
	if !ok || env.Getenv("SERVE_DB") == "" || env.Getenv("SERVE_ZONE") == "" {
		return nil, errors.New("ADMIN_OIDC=1 needs serve (SERVE_DB, SERVE_ZONE): " +
			"the console uses the serve certificate")
	}
	zone := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(env.Getenv("SERVE_ZONE"))), ".")
	name := strings.ToLower(strings.TrimSpace(env.Default("ADMIN_CONSOLE_NAME", "console."+zone)))
	issuer := env.Default("ADMIN_OIDC_ISSUER", env.Getenv("OIDC_ISSUER"))
	clientID := env.Getenv("ADMIN_OIDC_CLIENT_ID")
	if issuer == "" || clientID == "" {
		return nil, errors.New("ADMIN_OIDC=1 needs ADMIN_OIDC_CLIENT_ID and " +
			"ADMIN_OIDC_ISSUER (or OIDC_ISSUER)")
	}
	hours := 8
	if v := env.Getenv("ADMIN_OIDC_SESSION_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 24 {
			return nil, errors.New("ADMIN_OIDC_SESSION_HOURS must be a whole number from 1 to 24")
		}
		hours = n
	}
	required := true
	switch v := strings.TrimSpace(env.Getenv("ADMIN_OIDC_REQUIRED")); v {
	case "", "1":
	case "0":
		required = false
	default:
		return nil, errors.New("ADMIN_OIDC_REQUIRED must be 1 (the console needs the sign-in) " +
			"or 0 (the sign-in only adds groups)")
	}
	idle := 30
	if v := env.Getenv("ADMIN_OIDC_IDLE_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 5 || n > 480 {
			return nil, errors.New("ADMIN_OIDC_IDLE_MINUTES must be a whole number from 5 to 480")
		}
		idle = n
	}
	m := &adminOIDCModule{env: env, serve: sm, name: name, addrs: addrs,
		issuer: issuer, clientID: clientID, secret: env.Getenv("ADMIN_OIDC_CLIENT_SECRET"),
		scopes: env.Default("ADMIN_OIDC_SCOPES", "openid email profile groups"),
		cfg: adminoidc.Config{
			GroupsClaim: env.Default("ADMIN_OIDC_GROUPS_CLAIM",
				env.Default("OIDC_GROUPS_CLAIM", "groups")),
			Session:  time.Duration(hours) * time.Hour,
			Required: required,
			Idle:     time.Duration(idle) * time.Minute,
			TrustedEmailDomains: adminoidc.ParseDomains(
				env.Getenv("ADMIN_OIDC_TRUSTED_EMAIL_DOMAINS")),
		}}
	for _, d := range m.cfg.TrustedEmailDomains {
		slog.Warn("admin sign-in trusts email domain without email_verified", "domain", d)
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: sm.ConsoleCert}
	slog.Info("console on https", "name", name, "session_hours", hours, "required", required,
		"idle_minutes", idle)
	return &adminConsoleSetup{mod: m, opts: []core.Option{core.WithAdminTLS(tlsCfg, name),
		core.WithAdminTLSGate(m.gate)}}, nil
}

// serveModule finds the serve module in mods (nil when it is not there).
func serveModule(mods []core.Module) core.Module {
	for _, m := range mods {
		if _, ok := m.(*servemod.Module); ok {
			return m
		}
	}
	return nil
}

// insertAfterServe puts m right after the serve module, so serve has registered (its store is
// open) before m starts.
func insertAfterServe(mods []core.Module, m core.Module) []core.Module {
	for i, x := range mods {
		if _, ok := x.(*servemod.Module); ok {
			out := append([]core.Module{}, mods[:i+1]...)
			out = append(out, m)
			return append(out, mods[i+1:]...)
		}
	}
	return append(mods, m)
}
