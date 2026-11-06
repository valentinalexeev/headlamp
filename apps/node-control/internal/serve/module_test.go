package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

type fakeNodes []headscale.Node

func (f fakeNodes) Nodes(context.Context) ([]headscale.Node, error) { return f, nil }

const (
	ipWeb    = "100.64.0.10" // tag:serve, tag:ns-cluster
	ipOther  = "100.64.0.11" // tag:serve, tag:ns-cluster (a second node)
	ipNoNS   = "100.64.0.12" // tag:serve only
	ipPlain  = "100.64.0.13" // a user, no tags
	ipSync   = "100.64.0.14" // tag:headscale
	ipOwner  = "100.64.0.15" // owner by policy
	baseName = "vpn.example.com"
)

func stateKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

type rig struct {
	t   *testing.T
	mux *core.Mux
	mod *Module
}

func newRig(t *testing.T, dir, key string) *rig {
	t.Helper()
	p, _ := authz.NewPolicy(authz.Config{Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	tn := authz.NewFakeTailnet(map[string]authz.Who{
		ipWeb:   {Login: "tagged-devices", Tags: []string{"tag:serve", "tag:ns-cluster"}},
		ipOther: {Login: "tagged-devices", Tags: []string{"tag:serve", "tag:ns-cluster"}},
		ipNoNS:  {Login: "tagged-devices", Tags: []string{"tag:serve"}},
		ipPlain: {Login: "plain@"},
		ipSync:  {Login: "tagged-devices", Tags: []string{"tag:headscale"}},
		ipOwner: {Login: "owner@"},
	})
	nodes := fakeNodes{
		{ID: 1, GivenName: "web", IPAddresses: []string{ipWeb}},
		{ID: 2, GivenName: "other", IPAddresses: []string{ipOther}},
		{ID: 3, GivenName: "nons", IPAddresses: []string{ipNoNS}},
	}
	env := core.MapEnv(map[string]string{
		"SERVE_DB": filepath.Join(dir, "serve.db"), "SERVE_BASE_DOMAIN": baseName, "NODE_CONTROL_STATE_KEY": key,
	})
	m := New(env, nodes).(*Module)
	mux := core.NewMux()
	if err := m.Register(&core.Routes{Admin: mux, Env: env, Tailnet: tn, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return &rig{t: t, mux: mux, mod: m}
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

func (r *rig) addNamespace() {
	r.t.Helper()
	if c, b := r.do("PUT", "/serve/namespaces/cluster", ipOwner, map[string]any{"suffixes": []string{"cluster"}}); c != 204 {
		r.t.Fatalf("put namespace: %d %s", c, b)
	}
}

func csrPEM(t *testing.T) string { return string(csrFor(t, "ignored", "evil.example.com")) }

func TestRegisterSignsAndPublishes(t *testing.T) {
	r := newRig(t, t.TempDir(), stateKey())
	r.addNamespace()
	c, b := r.do("POST", "/serve/v1/register", ipWeb, map[string]any{"names": []string{"Grafana.cluster.vpn.example.com"}, "csr": csrPEM(t)})
	if c != 200 {
		t.Fatalf("register: %d %s", c, b)
	}
	var resp registerResp
	json.Unmarshal(b, &resp)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(r.mod.ca.CertPEM())
	blk, _ := pem.Decode([]byte(resp.Cert))
	cert, _ := x509.ParseCertificate(blk.Bytes)
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "grafana.cluster.vpn.example.com", Roots: roots}); err != nil {
		t.Fatalf("cert: %v", err)
	}
	if len(cert.DNSNames) != 1 {
		t.Fatalf("the CSR's own names leaked into the cert: %v", cert.DNSNames)
	}
	c, b = r.do("GET", "/serve/v1/records", ipSync, nil)
	if c != 200 || !strings.Contains(string(b), `"name":"grafana.cluster.vpn.example.com","type":"A","value":"100.64.0.10"`) {
		t.Fatalf("records: %d %s", c, b)
	}
}

func TestRegisterRefusals(t *testing.T) {
	r := newRig(t, t.TempDir(), stateKey())
	r.addNamespace()
	if c, b := r.do("POST", "/serve/v1/register", ipWeb, map[string]any{"names": []string{"a.cluster.vpn.example.com"}, "csr": csrPEM(t)}); c != 200 {
		t.Fatalf("first claim: %d %s", c, b)
	}
	for _, tc := range []struct {
		name string
		ip   string
		body map[string]any
		want int
	}{
		{"another node's name", ipOther, map[string]any{"names": []string{"a.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 409},
		{"one taken name refuses the whole request", ipOther, map[string]any{"names": []string{"b.cluster.vpn.example.com", "a.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 409},
		{"outside the namespace zone", ipWeb, map[string]any{"names": []string{"a.lab.vpn.example.com"}, "csr": csrPEM(t)}, 422},
		{"a namespace the node is not in", ipWeb, map[string]any{"namespace": "lab", "names": []string{"a.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 403},
		{"a node name", ipWeb, map[string]any{"names": []string{"web.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 200},
		{"no ns tag", ipNoNS, map[string]any{"names": []string{"c.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 403},
		{"a user", ipPlain, map[string]any{"names": []string{"c.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 403},
		{"the sync agent", ipSync, map[string]any{"names": []string{"c.cluster.vpn.example.com"}, "csr": csrPEM(t)}, 403},
		{"bad CSR", ipWeb, map[string]any{"names": []string{"d.cluster.vpn.example.com"}, "csr": "nope"}, 422},
		{"unknown field", ipWeb, map[string]any{"names": []string{"d.cluster.vpn.example.com"}, "csr": csrPEM(t), "ip": "8.8.8.8"}, 400},
	} {
		if c, b := r.do("POST", "/serve/v1/register", tc.ip, tc.body); c != tc.want {
			t.Errorf("%s: %d %s, want %d", tc.name, c, b, tc.want)
		}
	}
	// web.cluster... is not a node name: node "web" is web.vpn.example.com. A real clash is refused:
	r.mod.nodes = append(r.mod.nodes.(fakeNodes), headscale.Node{ID: 9, GivenName: "x", IPAddresses: []string{"100.64.9.9"}})
	if c, _ := r.do("POST", "/serve/v1/register", ipWeb, map[string]any{"names": []string{"x.vpn.example.com"}, "csr": csrPEM(t)}); c != 422 {
		t.Errorf("a node's MagicDNS name: %d, want 422", c)
	}
	// Nothing was claimed by the refused requests: only the two accepted names are published.
	_, b := r.do("GET", "/serve/v1/records", ipSync, nil)
	var recs recordsResp
	json.Unmarshal(b, &recs)
	var got []string
	for _, rc := range recs.Records {
		got = append(got, rc.Name)
	}
	if strings.Join(got, ",") != "a.cluster.vpn.example.com,web.cluster.vpn.example.com" {
		t.Fatalf("published %v", got)
	}
}

func TestOnlyTheSyncAgentReadsRecords(t *testing.T) {
	r := newRig(t, t.TempDir(), stateKey())
	for _, ip := range []string{ipWeb, ipPlain, ipOwner} {
		if c, _ := r.do("GET", "/serve/v1/records", ip, nil); c != 403 {
			t.Errorf("records from %s: %d", ip, c)
		}
	}
	if c, _ := r.do("POST", "/serve/v1/state", ipSync, map[string]any{"version": 3}); c != 204 {
		t.Errorf("state report: %d", c)
	}
	c, b := r.do("GET", "/serve/state", ipOwner, nil)
	if c != 200 || !strings.Contains(string(b), `"version":3`) {
		t.Errorf("admin state shows the sync report: %d %s", c, b)
	}
}

func TestAdminRoutesNeedServeRights(t *testing.T) {
	r := newRig(t, t.TempDir(), stateKey())
	if c, _ := r.do("PUT", "/serve/namespaces/cluster", ipPlain, map[string]any{"suffixes": []string{"cluster"}}); c != 403 {
		t.Errorf("a user without serve.admin: %d", c)
	}
	if c, b := r.do("PUT", "/serve/namespaces/cluster", ipOwner, map[string]any{"suffixes": []string{"cluster.vpn.example.com"}}); c != 422 {
		t.Errorf("suffix with the base domain: %d %s", c, b)
	}
	if c, b := r.do("GET", "/serve/ca.crt", ipPlain, nil); c != 200 || !strings.Contains(string(b), "BEGIN CERTIFICATE") {
		t.Errorf("ca.crt must be public: %d", c)
	}
}

func TestSelfSignedCAPersistsAndNeedsItsKey(t *testing.T) {
	dir, key := t.TempDir(), stateKey()
	a := newRig(t, dir, key)
	fp := a.mod.ca.Fingerprint()
	a.mod.Stop(context.Background())
	b := newRig(t, dir, key)
	if b.mod.ca.Fingerprint() != fp {
		t.Fatal("a restart created a new CA: users would have to trust it again")
	}
	b.mod.Stop(context.Background())
	// A different state key cannot open the sealed CA key: start fails, the CA is not replaced.
	env := core.MapEnv(map[string]string{"SERVE_DB": filepath.Join(dir, "serve.db"), "SERVE_BASE_DOMAIN": baseName, "NODE_CONTROL_STATE_KEY": stateKey()})
	m := New(env, fakeNodes{}).(*Module)
	err := m.Register(&core.Routes{Admin: core.NewMux(), Env: env, Tailnet: authz.NewFakeTailnet(nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err == nil || !strings.Contains(err.Error(), "does not decrypt") {
		t.Fatalf("wrong key: %v", err)
	}
}

func TestStartChecks(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"no base":       {"SERVE_DB": "x"},
		"no state key":  {"SERVE_BASE_DOMAIN": baseName},
		"half operator": {"SERVE_BASE_DOMAIN": baseName, "SERVE_CA_CERT": "x"},
		"bad tag":       {"SERVE_BASE_DOMAIN": baseName, "SERVE_NODE_TAG": "serve"},
	} {
		if _, ok := env["SERVE_DB"]; !ok || env["SERVE_DB"] == "x" {
			env["SERVE_DB"] = filepath.Join(t.TempDir(), "s.db")
		}
		if name == "no base" {
			delete(env, "SERVE_BASE_DOMAIN")
		}
		m := New(core.MapEnv(env), fakeNodes{}).(*Module)
		if err := m.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(env), Tailnet: authz.NewFakeTailnet(nil)}); err == nil {
			t.Errorf("%s: started", name)
		}
	}
	// Off without SERVE_DB: no routes.
	mux := core.NewMux()
	if err := New(core.MapEnv(nil), fakeNodes{}).Register(&core.Routes{Admin: mux, Env: core.MapEnv(nil)}); err != nil || len(mux.Patterns()) != 0 {
		t.Fatalf("serve must be off without SERVE_DB: %v %v", err, mux.Patterns())
	}
	_ = http.StatusOK
}
