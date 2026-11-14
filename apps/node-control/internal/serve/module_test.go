package serve

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

const (
	zone     = "svc.example.com"
	base     = "tailnet.example.com"
	ipOwner  = "100.64.0.1"
	ipViewer = "100.64.0.2"
	ipPlain  = "100.64.0.3"
	ipProxyA = "100.64.0.10" // joins with the first key of the first service
	ipProxyB = "100.64.0.11" // a second replica on the same key
	ipStray  = "100.64.0.12" // tag:serve, but its key belongs to no service
)

// fakeHS is headscale: keys it hands out, and nodes the test attaches to keys.
type fakeHS struct {
	mu       sync.Mutex
	nextKey  uint64
	keys     map[uint64]headscale.PreAuthKey
	expired  map[uint64]bool
	nodes    []headscale.Node
	failNext error
}

func newFakeHS() *fakeHS {
	return &fakeHS{nextKey: 100, keys: map[uint64]headscale.PreAuthKey{}, expired: map[uint64]bool{}}
}

func (f *fakeHS) ListNodes(context.Context) ([]headscale.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]headscale.Node(nil), f.nodes...), nil
}

func (f *fakeHS) ListUsers(context.Context) ([]headscale.User, error) {
	return []headscale.User{{ID: 7, Name: "_svcs"}}, nil
}

func (f *fakeHS) CreatePreAuthKey(_ context.Context, uid uint64, tags []string, ttl time.Duration, eph, reusable bool) (headscale.PreAuthKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.failNext; err != nil {
		f.failNext = nil
		return headscale.PreAuthKey{}, err
	}
	f.nextKey++
	k := headscale.PreAuthKey{ID: f.nextKey, Key: "hskey-secret-" + string(rune('a'+f.nextKey%26)), UserID: uid,
		Reusable: reusable, Ephemeral: eph, ACLTags: tags, Expiration: time.Now().Add(ttl)}
	f.keys[k.ID] = k
	return k, nil
}

func (f *fakeHS) ExpirePreAuthKey(_ context.Context, id uint64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.expired[id] = true
	return nil
}

func (f *fakeHS) attach(ip string, keyID uint64, online bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nodes = append(f.nodes, headscale.Node{ID: uint64(len(f.nodes) + 1), GivenName: "n" + ip, IPAddresses: []string{ip},
		Online: online, PreAuthKeyID: keyID})
}

func stateKey() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

type rig struct {
	t   *testing.T
	mux *core.Mux
	mod *Module
	hs  *fakeHS
}

func newRig(t *testing.T, extra map[string]string) *rig {
	t.Helper()
	return newRigOpts(t, extra)
}

func mergeEnv(ms ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

// tryRig registers a module with extra and returns the error, for refusal tests.
func tryRig(t *testing.T, extra map[string]string) error { return tryRigOpts(t, extra) }

func tryRigOpts(t *testing.T, extra map[string]string, opts ...Option) (err error) {
	t.Helper()
	ft := &failT{T: t}
	func() {
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(rigErr); ok {
					err = e.error
					return
				}
				panic(r)
			}
		}()
		newRigOpts(ft, extra, opts...)
	}()
	return err
}

type rigErr struct{ error }

// failT turns the rig's Fatal into a recoverable error.
type failT struct{ *testing.T }

func (f *failT) Fatal(a ...any) { panic(rigErr{fmt.Errorf("%v", a...)}) }

type tb interface {
	Helper()
	Fatal(...any)
	TempDir() string
	Cleanup(func())
}

func newRigOpts(tt tb, extra map[string]string, opts ...Option) *rig {
	tt.Helper()
	var t *testing.T
	switch v := tt.(type) {
	case *testing.T:
		t = v
	case *failT:
		t = v.T
	}
	p, _ := authz.NewPolicy(authz.Config{
		Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}, {Subject: "viewer@", Roles: []string{"viewer"}}},
	})
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	tn := authz.NewFakeTailnet(map[string]authz.Who{
		ipOwner:  {Login: "owner@"},
		ipViewer: {Login: "viewer@"},
		ipPlain:  {Login: "plain@"},
		ipProxyA: {Login: "tagged-devices", Tags: []string{"tag:serve", "tag:ns-mon"}},
		ipProxyB: {Login: "tagged-devices", Tags: []string{"tag:serve", "tag:ns-mon"}},
		ipStray:  {Login: "tagged-devices", Tags: []string{"tag:serve", "tag:ns-mon"}},
	})
	env := map[string]string{
		"SERVE_DB": filepath.Join(t.TempDir(), "serve.db"), "SERVE_ZONE": zone, "SERVE_BASE_DOMAIN": base,
		"SERVE_USER": "_svcs", "NODE_CONTROL_STATE_KEY": stateKey(),
	}
	for k, v := range extra {
		env[k] = v
	}
	hs := newFakeHS()
	m := New(core.MapEnv(env), hs, opts...).(*Module)
	mux := core.NewMux()
	if err := m.Register(&core.Routes{Admin: mux, Env: core.MapEnv(env), Tailnet: tn, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		tt.Fatal(err)
	}
	t.Cleanup(func() { m.Stop(context.Background()) })
	return &rig{t: t, mux: mux, mod: m, hs: hs}
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

func (r *rig) create(name, ns string) keyResp {
	r.t.Helper()
	c, b := r.do("POST", "/serve/services", ipOwner, map[string]any{"name": name, "namespace": ns})
	if c != http.StatusCreated {
		r.t.Fatalf("create %s: %d %s", name, c, b)
	}
	var out keyResp
	_ = json.Unmarshal(b, &out)
	return out
}

func (r *rig) lookup(name string) (dnsmessage.RCode, []string) {
	r.t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	var p dnsmessage.Parser
	h, err := p.Start(r.mod.dns.Answer(q))
	if err != nil {
		r.t.Fatal(err)
	}
	_ = p.SkipAllQuestions()
	var ips []string
	for {
		if _, err := p.AnswerHeader(); err != nil {
			break
		}
		a, _ := p.AResource()
		ips = append(ips, netip.AddrFrom4(a.A).String())
	}
	return h.RCode, ips
}

func csrPEM(t *testing.T) string {
	return string(csrFor(t, "ignored", "evil.example.org", "*.svc.example.com"))
}

func TestServiceLifecycleAndDNS(t *testing.T) {
	r := newRig(t, nil)
	out := r.create("grafana", "mon")
	if out.Service.Name != "grafana."+zone || out.AuthKey == "" || len(out.Service.Keys) != 1 {
		t.Fatalf("create: %+v", out)
	}
	key := r.hs.keys[out.Service.Keys[0].ID]
	if !key.Reusable || !key.Ephemeral || key.UserID != 7 || strings.Join(key.ACLTags, ",") != "tag:serve,tag:ns-mon" {
		t.Fatalf("the key must be reusable, ephemeral, owned by SERVE_USER and tagged: %+v", key)
	}
	// the secret is shown once and never stored
	_, st := r.do("GET", "/serve/state", ipOwner, nil)
	if bytes.Contains(st, []byte(out.AuthKey)) {
		t.Fatal("the key secret leaked into the state")
	}

	// no node yet: the name exists with no address
	if rc, ips := r.lookup("grafana." + zone); rc != dnsmessage.RCodeSuccess || len(ips) != 0 {
		t.Fatalf("before any node: %v %v", rc, ips)
	}
	// two replicas on the key, one offline node on it, and a node on a key of no service
	kid := out.Service.Keys[0].ID
	r.hs.attach(ipProxyA, kid, true)
	r.hs.attach(ipProxyB, kid, true)
	r.hs.attach("100.64.0.99", kid, false)
	r.hs.attach(ipStray, 9999, true)
	r.mod.refreshDNS(context.Background())
	if rc, ips := r.lookup("grafana." + zone); rc != dnsmessage.RCodeSuccess || strings.Join(ips, ",") != ipProxyA+","+ipProxyB {
		t.Fatalf("two online replicas: %v %v", rc, ips)
	}
	if rc, _ := r.lookup("other." + zone); rc != dnsmessage.RCodeNameError {
		t.Fatalf("unknown name: %v", rc)
	}

	// the name is taken once, and a refused create leaves no key behind
	before := len(r.hs.keys)
	if c, _ := r.do("POST", "/serve/services", ipOwner, map[string]any{"name": "grafana." + zone, "namespace": "x"}); c != http.StatusConflict {
		t.Fatalf("duplicate: %d", c)
	}
	if len(r.hs.keys) != before {
		t.Fatal("a refused create minted a key")
	}

	// delete: keys expire in headscale, the name goes
	if c, b := r.do("DELETE", "/serve/services/"+out.Service.ID, ipOwner, nil); c != http.StatusNoContent {
		t.Fatalf("delete: %d %s", c, b)
	}
	if !r.hs.expired[kid] {
		t.Fatal("delete must expire the service key")
	}
	if rc, _ := r.lookup("grafana." + zone); rc != dnsmessage.RCodeNameError {
		t.Fatalf("after delete: %v", rc)
	}
}

func TestWildcardService(t *testing.T) {
	r := newRig(t, nil)
	out := r.create("*.apps", "mon")
	if out.Service.Name != "*.apps."+zone {
		t.Fatalf("name: %s", out.Service.Name)
	}
	r.hs.attach(ipProxyA, out.Service.Keys[0].ID, true)
	r.mod.refreshDNS(context.Background())
	for _, n := range []string{"a.apps." + zone, "x.y.apps." + zone} {
		if rc, ips := r.lookup(n); rc != dnsmessage.RCodeSuccess || len(ips) != 1 || ips[0] != ipProxyA {
			t.Fatalf("%s: %v %v", n, rc, ips)
		}
	}
	// the certificate is the wildcard: it covers one label, as X.509 says
	c, b := r.do("POST", "/serve/v1/cert", ipProxyA, map[string]any{"csr": csrPEM(t)})
	if c != 200 {
		t.Fatalf("cert: %d %s", c, b)
	}
	var cr certResp
	_ = json.Unmarshal(b, &cr)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(r.mod.ca.CertPEM())
	blk, _ := pem.Decode([]byte(cr.CertPEM))
	cert, _ := x509.ParseCertificate(blk.Bytes)
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "a.apps." + zone, Roots: roots}); err != nil {
		t.Fatalf("wildcard cert for a.apps: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{DNSName: "x.y.apps." + zone, Roots: roots}); err == nil {
		t.Fatal("a wildcard certificate must not cover two labels")
	}
}

func TestNameRules(t *testing.T) {
	r := newRig(t, nil)
	for _, bad := range []string{"*", "*." + zone, "a.*.b", "-x", "x..y", strings.Repeat("a", 64), "UPPER_case"} {
		if c, _ := r.do("POST", "/serve/services", ipOwner, map[string]any{"name": bad, "namespace": "mon"}); c != http.StatusUnprocessableEntity {
			t.Errorf("name %q: %d, want 422", bad, c)
		}
	}
	for _, bad := range []string{"", "Mon!", strings.Repeat("n", 33)} {
		if c, _ := r.do("POST", "/serve/services", ipOwner, map[string]any{"name": "ok", "namespace": bad}); c != http.StatusUnprocessableEntity {
			t.Errorf("namespace %q: %d, want 422", bad, c)
		}
	}
	// a name under some other domain is taken as a label path under the zone, never outside it
	if out := r.create("grafana.example.org", "mon"); out.Service.Name != "grafana.example.org."+zone {
		t.Fatalf("got %s", out.Service.Name)
	}
}

func TestCertOnlyForTheCallersService(t *testing.T) {
	r := newRig(t, nil)
	g := r.create("grafana", "mon")
	p := r.create("prom", "mon")
	r.hs.attach(ipProxyA, g.Service.Keys[0].ID, true)
	r.hs.attach(ipProxyB, p.Service.Keys[0].ID, true)
	r.hs.attach(ipStray, 9999, true)

	for ip, want := range map[string]string{ipProxyA: "grafana." + zone, ipProxyB: "prom." + zone} {
		c, b := r.do("POST", "/serve/v1/cert", ip, map[string]any{"csr": csrPEM(t)})
		if c != 200 {
			t.Fatalf("%s: %d %s", ip, c, b)
		}
		var cr certResp
		_ = json.Unmarshal(b, &cr)
		blk, _ := pem.Decode([]byte(cr.CertPEM))
		cert, _ := x509.ParseCertificate(blk.Bytes)
		if cr.Name != want || len(cert.DNSNames) != 1 || cert.DNSNames[0] != want {
			t.Fatalf("%s got %q %v, want only %s (the names in the CSR are ignored)", ip, cr.Name, cert.DNSNames, want)
		}
	}
	// a serve-tagged node on a key of no service, and humans, get nothing
	if c, _ := r.do("POST", "/serve/v1/cert", ipStray, map[string]any{"csr": csrPEM(t)}); c != http.StatusForbidden {
		t.Fatalf("stray: %d", c)
	}
	if c, _ := r.do("POST", "/serve/v1/cert", ipOwner, map[string]any{"csr": csrPEM(t)}); c != http.StatusForbidden {
		t.Fatalf("a human: %d", c)
	}
	if c, _ := r.do("POST", "/serve/v1/cert", ipProxyA, map[string]any{"csr": "junk"}); c != http.StatusUnprocessableEntity {
		t.Fatalf("bad csr: %d", c)
	}
}

func TestRotation(t *testing.T) {
	r := newRig(t, nil)
	g := r.create("grafana", "mon")
	oldKey := g.Service.Keys[0].ID
	r.hs.attach(ipProxyA, oldKey, true)
	c, b := r.do("POST", "/serve/services/"+g.Service.ID+"/keys", ipOwner, nil)
	if c != http.StatusCreated {
		t.Fatalf("add key: %d %s", c, b)
	}
	var rot keyResp
	_ = json.Unmarshal(b, &rot)
	newKey := rot.Service.Keys[0].ID
	if len(rot.Service.Keys) != 2 || newKey == oldKey || rot.AuthKey == "" {
		t.Fatalf("rotation: %+v", rot)
	}
	if c, _ := r.do("POST", "/serve/services/"+g.Service.ID+"/keys", ipOwner, nil); c != http.StatusConflict {
		t.Fatalf("a third key: %d", c)
	}
	// during the rotation, nodes on either key are the service
	r.hs.attach(ipProxyB, newKey, true)
	r.mod.refreshDNS(context.Background())
	if _, ips := r.lookup("grafana." + zone); len(ips) != 2 {
		t.Fatalf("both keys during rotation: %v", ips)
	}
	if c, _ := r.do("POST", "/serve/v1/cert", ipProxyA, map[string]any{"csr": csrPEM(t)}); c != 200 {
		t.Fatalf("old key still valid during rotation: %d", c)
	}
	// retire the old key: its node drops out of DNS and gets no certificate
	if c, b := r.do("DELETE", "/serve/services/"+g.Service.ID+"/keys/"+strconv.FormatUint(oldKey, 10), ipOwner, nil); c != http.StatusNoContent {
		t.Fatalf("retire: %d %s", c, b)
	}
	if !r.hs.expired[oldKey] {
		t.Fatal("retire must expire the key in headscale")
	}
	if _, ips := r.lookup("grafana." + zone); len(ips) != 1 || ips[0] != ipProxyB {
		t.Fatalf("after retire: %v", ips)
	}
	if c, _ := r.do("POST", "/serve/v1/cert", ipProxyA, map[string]any{"csr": csrPEM(t)}); c != http.StatusForbidden {
		t.Fatalf("retired key: %d", c)
	}
}

func TestPermissions(t *testing.T) {
	r := newRig(t, nil)
	g := r.create("grafana", "mon")
	cases := []struct {
		method, path, ip string
		body             any
		want             int
	}{
		{"GET", "/serve/state", ipViewer, nil, 200},
		{"GET", "/serve/state", ipPlain, nil, 403},
		{"POST", "/serve/services", ipViewer, map[string]any{"name": "x", "namespace": "mon"}, 403},
		{"DELETE", "/serve/services/" + g.Service.ID, ipViewer, nil, 403},
		{"POST", "/serve/services/" + g.Service.ID + "/keys", ipViewer, nil, 403},
		{"GET", "/serve/state", ipProxyA, nil, 403}, // a proxy is not a human
		{"GET", "/serve/ca.crt", ipPlain, nil, 200}, // public
	}
	for _, c := range cases {
		if got, _ := r.do(c.method, c.path, c.ip, c.body); got != c.want {
			t.Errorf("%s %s from %s: %d, want %d", c.method, c.path, c.ip, got, c.want)
		}
	}
}

func TestHeadscaleFailureLeavesNothing(t *testing.T) {
	r := newRig(t, nil)
	r.hs.failNext = errors.New("down")
	if c, _ := r.do("POST", "/serve/services", ipOwner, map[string]any{"name": "grafana", "namespace": "mon"}); c != http.StatusBadGateway {
		t.Fatalf("headscale down: %d", c)
	}
	if svcs, _ := r.mod.st.Services(); len(svcs) != 0 {
		t.Fatal("a failed create left a service")
	}
}

func TestZoneRules(t *testing.T) {
	for _, c := range []struct{ zone, base, want string }{
		{"svc.tailnet.example.com", "tailnet.example.com", "under the MagicDNS base_domain"},
		{"tailnet.example.com", "tailnet.example.com", "under the MagicDNS base_domain"},
		{"example.com", "tailnet.example.com", "contains the MagicDNS base_domain"},
		{"internal", "tailnet.example.com", "at least two labels"},
		{"", "tailnet.example.com", "not set"},
		{"svc.example.com", "", "SERVE_BASE_DOMAIN"},
		{"svc.example.com", "tailnet.example.com", ""},
		{"svc.internal", "tailnet.example.com", ""},
	} {
		err := checkZone(c.zone, c.base)
		if (c.want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.want)) {
			t.Errorf("zone %q base %q: %v, want %q", c.zone, c.base, err, c.want)
		}
	}
}

func TestRegisterRefusals(t *testing.T) {
	good := map[string]string{"SERVE_ZONE": zone, "SERVE_BASE_DOMAIN": base, "SERVE_USER": "_svcs", "NODE_CONTROL_STATE_KEY": stateKey()}
	for name, mod := range map[string]map[string]string{
		"no user":          {"SERVE_USER": ""},
		"zone under base":  {"SERVE_ZONE": "svc." + base},
		"no state key":     {"NODE_CONTROL_STATE_KEY": ""},
		"half an operator": {"SERVE_CA_CERT": "x"},
		"bad tag":          {"SERVE_NODE_TAG": "serve"},
		"bad ttl":          {"SERVE_KEY_TTL": "1s"},
	} {
		env := map[string]string{"SERVE_DB": filepath.Join(t.TempDir(), "s.db")}
		for k, v := range good {
			env[k] = v
		}
		for k, v := range mod {
			env[k] = v
		}
		m := New(core.MapEnv(env), newFakeHS())
		err := m.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(env), Tailnet: authz.NewFakeTailnet(nil),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		if err == nil {
			t.Errorf("%s: serve started", name)
		}
	}
}

func TestCAPersistsAndKeyIsSealed(t *testing.T) {
	dir := t.TempDir()
	key := stateKey()
	env := map[string]string{"SERVE_DB": filepath.Join(dir, "serve.db"), "SERVE_ZONE": zone, "SERVE_BASE_DOMAIN": base,
		"SERVE_USER": "_svcs", "NODE_CONTROL_STATE_KEY": key}
	start := func(e map[string]string) (*Module, error) {
		m := New(core.MapEnv(e), newFakeHS()).(*Module)
		err := m.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(e), Tailnet: authz.NewFakeTailnet(nil),
			Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		return m, err
	}
	m1, err := start(env)
	if err != nil {
		t.Fatal(err)
	}
	fp := m1.ca.Fingerprint()
	sealed, _ := m1.st.GetSecret(caKeyKey)
	if bytes.Contains(sealed, []byte("PRIVATE KEY")) {
		t.Fatal("the CA key is stored in clear")
	}
	m1.Stop(context.Background())
	m2, err := start(env)
	if err != nil || m2.ca.Fingerprint() != fp {
		t.Fatalf("restart: %v", err)
	}
	m2.Stop(context.Background())
	env["NODE_CONTROL_STATE_KEY"] = stateKey()
	if _, err := start(env); err == nil {
		t.Fatal("a wrong state key must stop serve")
	}
}
