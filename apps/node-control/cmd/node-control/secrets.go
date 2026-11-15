package main

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/secretref"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// secretrefTimeout bounds all Secret Manager reads at start; secretrefHTTPTimeout one call.
const (
	secretrefTimeout     = time.Minute
	secretrefHTTPTimeout = 30 * time.Second
)

// startEnv resolves the gcpsm:// secret references of env (#361) and applies REDIS_CA_FILE. With
// no reference it reads nothing and returns env as it is.
func startEnv(ctx context.Context, env core.Env) (core.Env, error) {
	return resolveEnv(ctx, env, newSecretResolver(env))
}

func resolveEnv(ctx context.Context, env core.Env, r *secretref.Resolver) (core.Env, error) {
	ctx, cancel := context.WithTimeout(ctx, secretrefTimeout)
	defer cancel()
	env, names, err := r.Resolve(ctx, env)
	if err != nil {
		return env, err
	}
	if len(names) > 0 {
		slog.Info("secrets read from Secret Manager", "names", strings.Join(names, ","))
	}
	if err := statestore.SetCAFile(strings.TrimSpace(env.Getenv("REDIS_CA_FILE"))); err != nil {
		return env, err
	}
	return env, nil
}

func newSecretResolver(env core.Env) *secretref.Resolver {
	hc := &http.Client{Timeout: secretrefHTTPTimeout}
	var src oauth2.TokenSource
	return &secretref.Resolver{
		Base: env.Getenv("SECRETREF_GCP_URL"),
		HTTP: hc,
		Token: func(ctx context.Context) (string, error) {
			if src == nil {
				ctx = context.WithValue(ctx, oauth2.HTTPClient, hc)
				c, err := google.FindDefaultCredentials(ctx, secretref.Scope)
				if err != nil {
					return "", err
				}
				src = oauth2.ReuseTokenSource(nil, c.TokenSource)
			}
			t, err := src.Token()
			if err != nil {
				return "", err
			}
			return t.AccessToken, nil
		},
	}
}
