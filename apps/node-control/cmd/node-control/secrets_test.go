package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/credcheck"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/secretref"
)

// The modules get the resolved value, and the fingerprint line is of the value, not the reference (#361).
func TestResolveEnv(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"payload":{"data":"` + base64.StdEncoding.EncodeToString([]byte("the-key")) + `"}}`))
	}))
	defer srv.Close()
	r := &secretref.Resolver{Base: srv.URL, HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return "tok", nil }}
	env := core.MapEnv(map[string]string{"HEADSCALE_API_KEY": "gcpsm://projects/p-test1/secrets/k"})
	got, err := resolveEnv(context.Background(), env, r)
	if err != nil || got.Getenv("HEADSCALE_API_KEY") != "the-key" {
		t.Fatalf("%q %v", got.Getenv("HEADSCALE_API_KEY"), err)
	}
	if fp := credcheck.Fingerprint(got.Getenv("HEADSCALE_API_KEY")); fp != credcheck.Fingerprint("the-key") {
		t.Fatal(fp)
	}

	bad := core.MapEnv(map[string]string{"REDIS_CA_FILE": "/nonexistent/ca.pem"})
	if _, err := resolveEnv(context.Background(), bad, r); err == nil || !strings.Contains(err.Error(), "REDIS_CA_FILE") {
		t.Fatalf("a missing CA file must stop the start: %v", err)
	}
}
