// Package mcp serves a Model Context Protocol endpoint (Streamable HTTP, JSON responses only) on
// node-control, so an AI agent can manage headlamp. Only admins connect: a human tailnet login, or
// an OIDC access token, that holds "mcp.admin". Each tool runs one admin route in this process as
// that caller, so the route's own permission, checks and audit apply as in the console.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// Module is the MCP endpoint. It is off unless MCP_ENABLED=1.
type Module struct {
	env      core.Env
	cfg      settings
	log      *slog.Logger
	tailnet  core.Tailnet
	admin    http.Handler
	verifier *verifier // nil without OIDC
	// registered reports whether an admin route pattern exists; nil means every route does (tests).
	registered func(pattern string) bool
}

// New returns the MCP module.
func New(env core.Env) core.Module { return &Module{env: env} }

// Name implements core.Module.
func (m *Module) Name() string { return "mcp" }

// Register implements core.Module.
func (m *Module) Register(r *core.Routes) error {
	cfg, err := readSettings(m.env)
	if err != nil || !cfg.enabled {
		return err
	}
	if cfg.public && r.Public == nil {
		return errors.New("MCP_PUBLIC=1 needs the public listener: turn on enrolment " +
			"(ENROLL_DB or ENROLL_REDIS=1), or set MCP_PUBLIC=0 and reach /mcp through the tailnet")
	}
	m.cfg, m.log, m.tailnet, m.admin = cfg, r.Log, r.Tailnet, r.Admin
	if m.log == nil {
		m.log = slog.Default()
	}
	m.registered = func(p string) bool { return slices.Contains(r.Admin.Patterns(), p) }
	r.Admin.Handle("POST /mcp", m.handler(false))
	if cfg.oidc != nil {
		m.verifier = newVerifier(cfg.oidc)
		r.Admin.HandleFunc("GET /.well-known/oauth-protected-resource", m.metadata)
		r.Admin.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", m.metadata)
	}
	if cfg.public {
		r.Public.Handle("POST /mcp", m.handler(true))
		r.Public.HandleFunc("GET /.well-known/oauth-protected-resource", m.metadata)
		r.Public.HandleFunc("GET /.well-known/oauth-protected-resource/mcp", m.metadata)
	}
	if !authz.AnyoneCanIn(authz.Default(), permConnect) {
		m.log.Warn("mcp: MCP_ENABLED=1, but nobody holds mcp.admin, " +
			"so the endpoint refuses everyone. " +
			"Grant the owner role (ADMIN_USERS) or mcp.admin (ADMIN_CONFIG)")
	}
	m.log.Info("mcp: endpoint on", "path", "/mcp", "oidc", cfg.oidc != nil, "public", cfg.public)
	return nil
}

// Start implements core.Module.
func (m *Module) Start(context.Context) error { return nil }

// handler is POST /mcp. The route is KindSelf in authz.Routes: the check is here, because a bearer
// token, not the tailnet peer, can name the caller.
func (m *Module) handler(public bool) http.Handler {
	return authz.RequireJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(r) {
			m.log.Warn("mcp refused", "reason", "cross_origin")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		caller, err := m.authenticate(r, public)
		var aerr authError
		switch {
		case errors.As(err, &aerr):
			m.log.Warn("mcp refused", "reason", aerr.reason, "public", public)
			if aerr.status == http.StatusUnauthorized {
				m.challenge(w, aerr.reason)
				return
			}
			w.WriteHeader(aerr.status)
			return
		case err != nil:
			w.WriteHeader(http.StatusForbidden)
			return
		}
		m.serveRPC(w, r, caller)
	}))
}

// metadata is the RFC 9728 protected resource metadata: it tells an MCP client which issuer gives
// tokens for this endpoint.
func (m *Module) metadata(w http.ResponseWriter, _ *http.Request) {
	o := m.cfg.oidc
	doc := map[string]any{
		"resource":                 o.resource,
		"authorization_servers":    []string{o.issuer},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "headlamp node-control",
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(doc); err != nil {
		return // the client went away
	}
}
