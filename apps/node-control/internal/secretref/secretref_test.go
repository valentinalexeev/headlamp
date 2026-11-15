package secretref

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// fakeSM is the Secret Manager v1 access API with a few secrets of project p-test1.
func fakeSM(t *testing.T, calls *atomic.Int32) *httptest.Server {
	t.Helper()
	secrets := map[string]string{
		"/v1/projects/p-test1/secrets/hs-key/versions/latest:access": "hskey-value\n",
		"/v1/projects/p-test1/secrets/hs-key/versions/3:access":      "hskey-v3",
		"/v1/projects/p-test1/secrets/redis/versions/latest:access":  "rediss://:pw@10.0.0.3:6378",
		"/v1/projects/p-test1/secrets/empty/versions/latest:access":  "",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"code":401,"message":"Request had invalid authentication credentials."}}`))
			return
		}
		v, ok := secrets[r.URL.Path]
		if !ok {
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"code":404,"message":"Secret [projects/p-test1/secrets/nope] not found or has no versions."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"name":"x","payload":{"data":"` + base64.StdEncoding.EncodeToString([]byte(v)) + `"}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func resolver(srv *httptest.Server, tok string) *Resolver {
	return &Resolver{Base: srv.URL + "/v1", HTTP: srv.Client(),
		Token: func(context.Context) (string, error) { return tok, nil }}
}

func TestResolve(t *testing.T) {
	var calls atomic.Int32
	srv := fakeSM(t, &calls)
	ctx := context.Background()

	t.Run("no reference: nothing is read, env is the same", func(t *testing.T) {
		calls.Store(0)
		r := &Resolver{HTTP: srv.Client(), Token: func(context.Context) (string, error) {
			t.Fatal("no token may be asked for")
			return "", nil
		}}
		env := core.MapEnv(map[string]string{"HEADSCALE_API_KEY": "plain", "REDIS_URL": "redis://h:6379"})
		got, names, err := r.Resolve(ctx, env)
		if err != nil || len(names) != 0 || calls.Load() != 0 || got.Getenv("HEADSCALE_API_KEY") != "plain" {
			t.Fatalf("%v %v calls=%d", names, err, calls.Load())
		}
	})

	t.Run("references are replaced, plain values and other keys stay", func(t *testing.T) {
		env := core.MapEnv(map[string]string{
			"HEADSCALE_API_KEY":  "gcpsm://projects/p-test1/secrets/hs-key",
			"ENROLL_SECRET_KEY":  "gcpsm://projects/p-test1/secrets/hs-key/versions/3",
			"REDIS_URL":          " gcpsm://projects/p-test1/secrets/redis ",
			"TAILSCALE_AUTH_KEY": "plain-key",
			"NODES_CONFIG":       "gcpsm://projects/p-test1/secrets/hs-key", // not a secret variable
			"OPENSTACK_CLOUDS":   "SEL=selectel",
			"SEL_PASSWORD":       "gcpsm://projects/p-test1/secrets/hs-key",
		})
		got, names, err := resolver(srv, "tok").Resolve(ctx, env)
		if err != nil {
			t.Fatal(err)
		}
		for k, want := range map[string]string{
			"HEADSCALE_API_KEY":  "hskey-value", // the trailing newline is cut
			"ENROLL_SECRET_KEY":  "hskey-v3",
			"REDIS_URL":          "rediss://:pw@10.0.0.3:6378",
			"TAILSCALE_AUTH_KEY": "plain-key",
			"NODES_CONFIG":       "gcpsm://projects/p-test1/secrets/hs-key",
			"SEL_PASSWORD":       "hskey-value",
		} {
			if got.Getenv(k) != want {
				t.Errorf("%s = %q, want %q", k, got.Getenv(k), want)
			}
		}
		if strings.Join(names, ",") != "ENROLL_SECRET_KEY,HEADSCALE_API_KEY,SEL_PASSWORD,REDIS_URL" {
			t.Errorf("names %v", names)
		}
	})

	for name, c := range map[string]struct {
		val, tok, want string
	}{
		"not found":     {"gcpsm://projects/p-test1/secrets/nope", "tok", "not found"},
		"bad token":     {"gcpsm://projects/p-test1/secrets/hs-key", "wrong-secret", "invalid authentication"},
		"empty version": {"gcpsm://projects/p-test1/secrets/empty", "tok", "empty"},
		"bad form":      {"gcpsm://p-test1/hs-key", "tok", "not a valid reference"},
		"path escape":   {"gcpsm://projects/p-test1/secrets/../../x", "tok", "not a valid reference"},
	} {
		t.Run(name, func(t *testing.T) {
			env := core.MapEnv(map[string]string{"HEADSCALE_API_KEY": c.val})
			_, _, err := resolver(srv, c.tok).Resolve(ctx, env)
			if err == nil || !strings.Contains(err.Error(), c.want) || !strings.Contains(err.Error(), "HEADSCALE_API_KEY") ||
				strings.Contains(err.Error(), "wrong-secret") || strings.Contains(err.Error(), "hskey") {
				t.Fatalf("%v", err)
			}
		})
	}

	t.Run("no credentials", func(t *testing.T) {
		r := &Resolver{Base: srv.URL + "/v1", HTTP: srv.Client(),
			Token: func(context.Context) (string, error) { return "", errors.New("could not find default credentials") }}
		env := core.MapEnv(map[string]string{"HEADSCALE_API_KEY": "gcpsm://projects/p-test1/secrets/hs-key"})
		if _, _, err := r.Resolve(ctx, env); err == nil || !strings.Contains(err.Error(), "GCP credentials") {
			t.Fatal(err)
		}
	})

	t.Run("AWS_SECRET_ACCESS_KEY cannot be a reference", func(t *testing.T) {
		env := core.MapEnv(map[string]string{"AWS_SECRET_ACCESS_KEY": "gcpsm://projects/p-test1/secrets/hs-key"})
		if _, _, err := resolver(srv, "tok").Resolve(ctx, env); err == nil || !strings.Contains(err.Error(), "AWS SDK") {
			t.Fatal(err)
		}
	})
}
