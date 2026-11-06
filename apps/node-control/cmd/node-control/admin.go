package main

import (
	"log/slog"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// buildPolicy reads who may do what on the admin listener (#122). ADMIN_CONFIG is the file of roles and grants.
// ADMIN_USERS makes each listed login an owner. POLICY_ADMIN_USERS, FUNNEL_ADMIN_USERS and TASKS_ADMIN_USERS keep
// working: each makes its logins admin of that one feature. All sources add up.
func buildPolicy(env core.Env) (*authz.Policy, error) {
	var p *authz.Policy
	var err error
	if path := env.Getenv("ADMIN_CONFIG"); path != "" {
		if p, err = authz.LoadPolicy(path); err != nil {
			return nil, err
		}
	} else if p, err = authz.NewPolicy(authz.Config{}); err != nil {
		return nil, err
	}
	if owners := authz.ParseAllowed(env.Getenv("ADMIN_USERS")); !owners.Empty() {
		p.AddOwners(owners)
	}
	for _, l := range []struct {
		f   authz.Feature
		env string
	}{
		{authz.FeaturePolicy, "POLICY_ADMIN_USERS"},
		{authz.FeatureFunnel, "FUNNEL_ADMIN_USERS"},
		{authz.FeatureTasks, "TASKS_ADMIN_USERS"},
	} {
		if a := authz.ParseAllowed(env.Getenv(l.env)); !a.Empty() {
			p.AddLegacy(l.f, a)
		}
	}
	return p, nil
}

// adminOpen is the transition mode of #122 decision 3: true keeps the console open to every tailnet user when no
// admin is configured, with a warning. It is set by the owner's answer on #122.
var adminOpen = true

// newAuthorizer builds the admin listener's Authorizer and logs the mode.
func newAuthorizer(p *authz.Policy, tn core.Tailnet, log *slog.Logger) *authz.Authorizer {
	if log == nil {
		log = slog.Default()
	}
	if p.Empty() {
		if adminOpen {
			log.Warn("no admin users configured: anyone the tailnet ACL admits can operate this console. " +
				"Set ADMIN_USERS or ADMIN_CONFIG (see the README, \"Admin roles\")")
		} else {
			log.Warn("no admin users configured: the console refuses everyone. " +
				"Set ADMIN_USERS or ADMIN_CONFIG (see the README, \"Admin roles\")")
		}
	}
	return &authz.Authorizer{Policy: p, Tailnet: tn, Log: log, Open: adminOpen}
}
