package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func writeAdminConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "admin.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestBuildPolicySourcesAddUp(t *testing.T) {
	cfg := writeAdminConfig(t, `{"grants":[{"subject":"vera@","roles":["viewer"]}]}`)
	p, err := buildPolicy(core.MapEnv(map[string]string{
		"ADMIN_CONFIG":       cfg,
		"ADMIN_USERS":        "olga@",
		"POLICY_ADMIN_USERS": "pat@",
		"FUNNEL_ADMIN_USERS": "fred@",
		"TASKS_ADMIN_USERS":  "tess@",
	}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		login string
		perm  authz.Perm
		want  bool
	}{
		{"vera@", authz.P(authz.FeatureNodes, authz.LevelView), true},
		{"vera@", authz.P(authz.FeatureNodes, authz.LevelOperate), false},
		{"olga@", authz.P(authz.FeaturePolicy, authz.LevelAdmin), true},
		{"pat@", authz.P(authz.FeaturePolicy, authz.LevelAdmin), true},
		{"pat@", authz.P(authz.FeatureNodes, authz.LevelView), false},
		{"fred@", authz.P(authz.FeatureFunnel, authz.LevelAdmin), true},
		{"tess@", authz.P(authz.FeatureTasks, authz.LevelAdmin), true},
		{"nobody@", authz.P(authz.FeatureOverview, authz.LevelView), false},
	} {
		if got := p.Can(authz.Subject{Login: tc.login}, tc.perm); got != tc.want {
			t.Errorf("%s %s = %v, want %v", tc.login, tc.perm, got, tc.want)
		}
	}
}

func TestBuildPolicyRefusesBadConfig(t *testing.T) {
	for _, body := range []string{`{"grant":[]}`, `{"grants":[{"subject":"a@","roles":["god"]}]}`, `not json`} {
		if _, err := buildPolicy(core.MapEnv(map[string]string{"ADMIN_CONFIG": writeAdminConfig(t, body)})); err == nil ||
			!strings.Contains(err.Error(), "ADMIN_CONFIG") {
			t.Errorf("%s: err = %v, want an ADMIN_CONFIG error", body, err)
		}
	}
	if _, err := buildPolicy(core.MapEnv(map[string]string{"ADMIN_CONFIG": "/nonexistent/admin.json"})); err == nil {
		t.Error("a missing ADMIN_CONFIG must stop the start")
	}
}

// End to end through core.Run with the real modules: the caller ("local" on the test tailnet) is a viewer, then an
// operator. A viewer sees the nodes but gets 403 on stop; an operator passes the check.
func TestAdminListenerEnforcesRoles(t *testing.T) {
	for _, tc := range []struct {
		role       string
		wantStatus int // GET /nodes.json
		wantStop   int // POST /nodes/x/stop
	}{
		{"viewer", 200, 403},
		{"operator", 200, 404}, // passes the check; the node does not exist
	} {
		t.Run(tc.role, func(t *testing.T) {
			env := core.MapEnv(map[string]string{
				"NODES_CONFIG": filepath.Join(t.TempDir(), "absent.json"),
				"ADMIN_CONFIG": writeAdminConfig(t, `{"grants":[{"subject":"local","roles":["`+tc.role+`"]}]}`),
			})
			pol, err := buildPolicy(env)
			if err != nil {
				t.Fatal(err)
			}
			old := authz.SetDefault(pol)
			t.Cleanup(func() { authz.SetDefault(old) })
			mods, err := buildModules(env, http.DefaultClient)
			if err != nil {
				t.Fatal(err)
			}
			addr := serveWith(t, env, mods, newAuthorizer(pol, core.NewLocal("127.0.0.1"), nil))
			if code, _ := fetch(t, "http://"+addr+"/nodes.json"); code != tc.wantStatus {
				t.Errorf("GET /nodes.json = %d, want %d", code, tc.wantStatus)
			}
			if code := post(t, "http://"+addr+"/nodes/x/stop"); code != tc.wantStop {
				t.Errorf("POST /nodes/x/stop = %d, want %d", code, tc.wantStop)
			}
			if code, _ := fetch(t, "http://"+addr+"/healthz"); code != 200 {
				t.Errorf("healthz = %d", code)
			}
		})
	}
}

// With no admin configured and the console closed (decision 3, option A), every gated route refuses.
func TestAdminListenerClosedWithoutAdmins(t *testing.T) {
	old := adminOpen
	adminOpen = false
	t.Cleanup(func() { adminOpen = old })
	env := core.MapEnv(map[string]string{"NODES_CONFIG": filepath.Join(t.TempDir(), "absent.json")})
	pol, err := buildPolicy(env)
	if err != nil {
		t.Fatal(err)
	}
	oldp := authz.SetDefault(pol)
	t.Cleanup(func() { authz.SetDefault(oldp) })
	mods, err := buildModules(env, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	addr := serveWith(t, env, mods, newAuthorizer(pol, core.NewLocal("127.0.0.1"), nil))
	if code, _ := fetch(t, "http://"+addr+"/nodes.json"); code != 403 {
		t.Errorf("closed mode, no admins: GET /nodes.json = %d, want 403", code)
	}
}

// serveWith is serve with an Authorizer. The authorizer's own tailnet is only used for WhoIs, which the test
// tailnet answers the same way.
func serveWith(t *testing.T, env core.Env, mods []core.Module, a core.Authorizer) string {
	t.Helper()
	tn := core.NewLocal("127.0.0.1")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- core.Run(ctx, mods, env, tn, core.WithAuthorizer(a)) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("Run did not stop")
		}
	})
	return tn.AdminAddr()
}

func post(t *testing.T, url string) int {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}
