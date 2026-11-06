package main

import (
	"net/http"
	"slices"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// Every admin route that a full build registers must have a permission in authz.Routes, and every entry in
// authz.Routes must name a real route. The "attention" scenario turns on every module. A new route without a
// permission fails here, and also stops the server at start (core.Mux.Seal).
func TestEveryAdminRouteHasAPermission(t *testing.T) {
	w, err := newWorld(scenarios["attention"])
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.close)
	mods, env, err := w.modules()
	if err != nil {
		t.Fatal(err)
	}
	r := &core.Routes{Admin: core.NewMux(), Public: http.NewServeMux(), Env: env, Tailnet: core.NewLocal("127.0.0.1")}
	for _, m := range mods {
		if err := m.Register(r); err != nil {
			t.Fatalf("register %s: %v", m.Name(), err)
		}
		if s, ok := m.(core.Stopper); ok {
			t.Cleanup(func() { _ = s.Stop(t.Context()) })
		}
	}
	if _, _, err := core.RegisterCoreRoutes(r.Admin, mods, r.Tailnet); err != nil {
		t.Fatal(err)
	}
	got := r.Admin.Patterns()
	for _, p := range got {
		if _, ok := authz.Routes[p]; !ok {
			t.Errorf("admin route %q has no permission in authz.Routes", p)
		}
	}
	for p := range authz.Routes {
		if !slices.Contains(got, p) {
			t.Errorf("authz.Routes has %q, but no module registers it", p)
		}
	}
	if err := r.Admin.Seal(&authz.Authorizer{Tailnet: r.Tailnet}); err != nil {
		t.Fatal(err)
	}
}
