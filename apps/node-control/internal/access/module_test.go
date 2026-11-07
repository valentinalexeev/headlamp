package access

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

const (
	ipOwner  = "100.64.0.1"
	ipBob    = "100.64.0.2"
	ipCarol  = "100.64.0.3"
	ipTagged = "100.64.0.4"
	ipOps    = "100.64.0.5"
)

type rig struct {
	t   *testing.T
	mux *core.Mux
	mod *Module
	pol *authz.Policy
}

// newRig builds the module the way main does: a policy with one file owner, installed as the default, and the
// routes sealed by the real Authorizer, so every check below goes through the production route table.
func newRig(t *testing.T, env map[string]string) *rig {
	t.Helper()
	p, err := authz.NewPolicy(authz.Config{
		Roles:  map[string][]string{"netops": {"*.view", "nodes.operate", "admin.admin"}},
		Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}, {Subject: "ops@", Roles: []string{"netops"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	if env == nil {
		env = map[string]string{"ADMIN_DB": filepath.Join(t.TempDir(), "admin.db")}
	}
	tn := authz.NewFakeTailnet(map[string]authz.Who{
		ipOwner:  {Login: "owner@"},
		ipBob:    {Login: "bob@"},
		ipCarol:  {Login: "carol@"},
		ipOps:    {Login: "ops@"},
		ipTagged: {Login: "tagged-devices", Tags: []string{"tag:x"}},
	})
	m := New(core.MapEnv(env)).(*Module)
	m.now = func() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }
	mux := core.NewMux()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := m.Register(&core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: tn, Log: log}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	// a stand-in for a page of another module, so the test can check what a grant allows
	mux.HandleFunc("GET /nodes.json", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("POST /nodes/{id}/start", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if err := mux.Seal(&authz.Authorizer{Policy: p, Tailnet: tn, Log: log}); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, mux: mux, mod: m, pol: p}
}

func (r *rig) do(method, path, ip string, body any) (int, []byte) {
	r.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func (r *rig) must(method, path, ip string, body any, want int) []byte {
	r.t.Helper()
	code, b := r.do(method, path, ip, body)
	if code != want {
		r.t.Fatalf("%s %s from %s = %d, want %d: %s", method, path, ip, code, want, b)
	}
	return b
}

// The acceptance criterion of #123, with a login grant: the owner grants viewer in the UI, the user can then view
// nodes but not start one, and removing the grant takes effect without a restart.
func TestAcceptanceGrantViewerThenRevoke(t *testing.T) {
	r := newRig(t, nil)
	r.must("GET", "/nodes.json", ipBob, nil, 403)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"viewer"}}, 200)
	r.must("GET", "/nodes.json", ipBob, nil, 200)
	r.must("POST", "/nodes/n1/start", ipBob, nil, 403)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{}}, 200)
	r.must("GET", "/nodes.json", ipBob, nil, 403)
}

// The same with a group grant: the policy applies it to a subject that carries the group.
func TestAcceptanceGroupGrant(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "group:staff", "roles": []string{"viewer"}}, 200)
	member := authz.Subject{Login: "dave@", Groups: []string{"staff"}}
	if !r.pol.Can(member, authz.P(authz.FeatureNodes, authz.LevelView)) || r.pol.Can(member, authz.P(authz.FeatureNodes, authz.LevelOperate)) {
		t.Fatal("a group member must view nodes and not start them")
	}
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "group:staff", "roles": []string{}}, 200)
	if r.pol.Can(member, authz.P(authz.FeatureNodes, authz.LevelView)) {
		t.Fatal("a removed group grant must stop at once")
	}
}

func TestPageNeedsAdminView(t *testing.T) {
	r := newRig(t, nil)
	for _, p := range []string{"/admin", "/admin/state", "/admin/export"} {
		r.must("GET", p, ipOwner, nil, 200)
		r.must("GET", p, ipBob, nil, 403)
		r.must("GET", p, ipTagged, nil, 403)
	}
	// a viewer reads nodes but not who holds what
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"viewer"}}, 200)
	r.must("GET", "/admin/state", ipBob, nil, 403)
	r.must("GET", "/admin/audit", ipBob, nil, 200) // audit.view is in "*.view"
}

func TestStateShowsLayers(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"viewer"}}, 200)
	var st struct {
		Version      int64            `json:"version"`
		FileGrants   []authz.Grant    `json:"file_grants"`
		StoredGrants []authz.Grant    `json:"stored_grants"`
		Roles        []authz.RoleInfo `json:"roles"`
		Me           string           `json:"me"`
		MyPerms      []string         `json:"my_perms"`
		CanAdmin     bool             `json:"can_admin"`
	}
	_ = json.Unmarshal(r.must("GET", "/admin/state", ipOwner, nil, 200), &st)
	if st.Version != 1 || len(st.FileGrants) != 2 || len(st.StoredGrants) != 1 || st.StoredGrants[0].Subject != "bob" || st.Me != "owner" || !st.CanAdmin {
		t.Fatalf("state: %+v", st)
	}
}

// No escalation: ops holds admin.admin but not owner, so it can grant viewer, not owner, and cannot make a role
// with more than it holds.
func TestNoEscalation(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/grants", ipOps, map[string]any{"subject": "bob@", "roles": []string{"viewer"}}, 200)
	r.must("POST", "/admin/grants", ipOps, map[string]any{"subject": "carol@", "roles": []string{"owner"}}, 403)
	r.must("PUT", "/admin/roles/starter", ipOps, map[string]any{"perms": []string{"nodes.operate"}}, 200)
	r.must("PUT", "/admin/roles/policy-boss", ipOps, map[string]any{"perms": []string{"policy.admin"}}, 403)
	// the owner gives bob owner; ops cannot take it away (it would have to hold owner to touch that grant)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"owner"}}, 200)
	r.must("POST", "/admin/grants", ipOps, map[string]any{"subject": "bob@", "roles": []string{}}, 403)
	// nor edit a role beyond its rights after the owner widened it
	r.must("PUT", "/admin/roles/starter", ipOwner, map[string]any{"perms": []string{"policy.admin"}}, 200)
	r.must("PUT", "/admin/roles/starter", ipOps, map[string]any{"perms": []string{"nodes.view"}}, 403)
}

func TestNoSelfGrant(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/grants", ipOps, map[string]any{"subject": "ops@", "roles": []string{"viewer"}}, 403)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "Owner@", "roles": []string{"viewer"}}, 403)
}

func TestRoleRules(t *testing.T) {
	r := newRig(t, nil)
	r.must("PUT", "/admin/roles/owner", ipOwner, map[string]any{"perms": []string{"nodes.view"}}, 400)
	r.must("PUT", "/admin/roles/netops", ipOwner, map[string]any{"perms": []string{"nodes.view"}}, 400) // file role
	r.must("PUT", "/admin/roles/bad", ipOwner, map[string]any{"perms": []string{"rockets.view"}}, 400)
	r.must("PUT", "/admin/roles/r1", ipOwner, map[string]any{"perms": []string{"nodes.operate"}}, 200)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"r1"}}, 200)
	r.must("DELETE", "/admin/roles/r1", ipOwner, nil, 409) // in use
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{}}, 200)
	r.must("DELETE", "/admin/roles/r1", ipOwner, nil, 200)
	r.must("DELETE", "/admin/roles/r1", ipOwner, nil, 404)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"r1"}}, 400)
}

// The file grants are break-glass: the store cannot remove them, they are not part of the export.
func TestBreakGlassOwnerSurvives(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/import", ipOwner, map[string]any{"grants": []any{}}, 200)
	r.must("GET", "/admin/state", ipOwner, nil, 200)
	if !r.pol.Can(authz.Subject{Login: "owner@"}, authz.P(authz.FeatureAdmin, authz.LevelAdmin)) {
		t.Fatal("the file owner must keep owner")
	}
}

func TestImportExport(t *testing.T) {
	r := newRig(t, nil)
	cfg := map[string]any{
		"roles":  map[string][]string{"starter": {"nodes.operate"}},
		"grants": []map[string]any{{"subject": "Bob@", "roles": []string{"starter", "viewer"}}, {"subject": "group:staff", "roles": []string{"viewer"}}},
	}
	r.must("POST", "/admin/import", ipOwner, cfg, 200)
	r.must("POST", "/nodes/n1/start", ipBob, nil, 200)
	var got authz.Config
	if err := json.Unmarshal(r.must("GET", "/admin/export", ipOwner, nil, 200), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Grants) != 2 || got.Grants[0].Subject != "bob" || strings.Join(got.Grants[0].Roles, ",") != "starter,viewer" || len(got.Roles["starter"]) != 1 {
		t.Fatalf("export: %+v", got)
	}
	// the export loads as an ADMIN_CONFIG file
	if _, err := authz.NewPolicy(got); err != nil {
		t.Fatalf("export is not a valid ADMIN_CONFIG: %v", err)
	}
	// invalid content and unknown fields are refused, and nothing changes
	r.must("POST", "/admin/import", ipOwner, map[string]any{"grants": []map[string]any{{"subject": "x@", "roles": []string{"god"}}}}, 400)
	r.must("POST", "/admin/import", ipOwner, map[string]any{"grant": []any{}}, 400)
	r.must("POST", "/nodes/n1/start", ipBob, nil, 200)
	// ops may not import a set that holds more than it has, nor change its own grants
	r.must("POST", "/admin/import", ipOps, map[string]any{"grants": []map[string]any{{"subject": "carol@", "roles": []string{"owner"}}}}, 403)
	r.must("POST", "/admin/import", ipOps, map[string]any{"grants": []map[string]any{{"subject": "ops@", "roles": []string{"viewer"}}}}, 403)
}

func TestAuditRecordsChanges(t *testing.T) {
	r := newRig(t, nil)
	r.must("PUT", "/admin/roles/r1", ipOwner, map[string]any{"perms": []string{"nodes.operate"}}, 200)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"r1"}}, 200)
	r.must("POST", "/admin/grants", ipOps, map[string]any{"subject": "carol@", "roles": []string{"owner"}}, 403)
	var es []Entry
	_ = json.Unmarshal(r.must("GET", "/admin/audit", ipOwner, nil, 200), &es)
	if len(es) != 2 || es[0].Action != "grant" || es[0].Actor != "owner" || es[0].Target != "bob" || es[0].Detail != "r1" || es[1].Action != "role" {
		t.Fatalf("audit: %+v", es)
	}
	act, err := r.mod.Activity(context.Background(), "owner", 10)
	if err != nil || len(act) != 2 || act[0].Source != "access" {
		t.Fatalf("activity: %+v %v", act, err)
	}
	if _, err := r.mod.Activity(context.Background(), "nobody", 10); err == nil {
		t.Fatal("activity must be hidden from who cannot view the audit")
	}
}

func TestStaleVersionConflicts(t *testing.T) {
	r := newRig(t, nil)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"viewer"}, "version": 0}, 200)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "carol@", "roles": []string{"viewer"}, "version": 0}, 409)
	r.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "carol@", "roles": []string{"viewer"}, "version": 1}, 200)
}

func TestRequiresJSON(t *testing.T) {
	r := newRig(t, nil)
	req := httptest.NewRequest("POST", "/admin/grants", strings.NewReader(`subject=bob`))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.RemoteAddr = ipOwner + ":1"
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("form post = %d", rec.Code)
	}
}

// Two instances share one Redis: a change on one reaches the other by the version poll, without a restart.
func TestReloadAcrossInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	env := map[string]string{"REDIS_URL": "redis://" + mr.Addr(), "ADMIN_REDIS_KEY": "nc:admin"}
	a := newRig(t, env)
	b := New(core.MapEnv(env)).(*Module)
	if err := b.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(env), Tailnet: authz.NewFakeTailnet(nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Stop(context.Background()) })
	a.must("POST", "/admin/grants", ipOwner, map[string]any{"subject": "bob@", "roles": []string{"viewer"}}, 200)
	if err := b.sync(); err != nil {
		t.Fatal(err)
	}
	if !b.pol.Can(authz.Subject{Login: "bob"}, authz.P(authz.FeatureNodes, authz.LevelView)) {
		t.Fatal("the second instance must pick up the grant")
	}
}

func TestDisabledWithoutStore(t *testing.T) {
	m := New(core.MapEnv(map[string]string{})).(*Module)
	mux := core.NewMux()
	if err := m.Register(&core.Routes{Admin: mux, Env: core.MapEnv(nil), Tailnet: authz.NewFakeTailnet(nil)}); err != nil {
		t.Fatal(err)
	}
	if len(mux.Patterns()) != 0 || m.Nav() != nil || m.HasActivity() {
		t.Fatal("without ADMIN_DB or REDIS_URL the page must be off")
	}
}

func TestBadStoredDataStopsStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "admin.db")
	s, _ := OpenSQLite(path)
	_, _ = s.Save(authz.Config{Grants: []authz.Grant{{Subject: "bob", Roles: []string{"gone"}}}}, 0, Entry{Action: "import"})
	s.Close()
	p, _ := authz.NewPolicy(authz.Config{})
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	m := New(core.MapEnv(map[string]string{"ADMIN_DB": path})).(*Module)
	err := m.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(nil), Tailnet: authz.NewFakeTailnet(nil)})
	if err == nil || !strings.Contains(err.Error(), "gone") {
		t.Fatalf("a stored grant of a role that is gone must stop the start: %v", err)
	}
}
