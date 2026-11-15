package main

import (
	"context"
	"net/http"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/adminoidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

// demoSignIn registers the console OIDC sign-in (#366) against the demo's fake issuer, so its
// routes
// are part of the demo and of the route check.
type demoSignIn struct{ idp *oidctest.Issuer }

// demoDiscoverTimeout bounds the discovery of the demo issuer.
const demoDiscoverTimeout = 10 * time.Second

func (*demoSignIn) Name() string { return "adminoidc" }

func (d *demoSignIn) Register(r *core.Routes) error {
	var p adminoidc.Provider = noProvider{}
	if d.idp != nil {
		ctx, cancel := context.WithTimeout(context.Background(), demoDiscoverTimeout)
		defer cancel()
		if dp, err := oidc.Discover(ctx, oidc.Config{Issuer: d.idp.URL, ClientID: "console-demo",
			RedirectURL: "https://console.demo.example.net/auth/callback",
			HTTP:        http.DefaultClient}); err == nil {
			p = dp
		}
	}
	adminoidc.New(adminoidc.Config{Provider: p, Tailnet: r.Tailnet, Log: r.Log}).Register(r.Admin)
	return nil
}

func (*demoSignIn) Start(context.Context) error { return nil }

// noProvider is used when the demo has no issuer: a sign-in then fails cleanly.
type noProvider struct{}

func (noProvider) AuthURL(oidc.Flow) string { return "/" }
func (noProvider) Exchange(context.Context, oidc.Flow, string, string, string) (
	oidc.Identity, error,
) {
	return oidc.Identity{}, oidc.ErrDenied
}
