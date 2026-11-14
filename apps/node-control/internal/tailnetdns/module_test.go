package tailnetdns

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"golang.org/x/net/dns/dnsmessage"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
)

const (
	base     = "vpn.example.com"
	zone     = "svc.example.com"
	ipOwner  = "100.64.0.1"
	ipViewer = "100.64.0.2"
	ipSelf   = "100.64.0.9"
	ipDevA   = "100.64.0.71"
	ipDevB   = "100.64.0.72"
	ipAlice  = "100.64.0.73"
)

type fakeHS struct {
	mu    sync.Mutex
	nodes []headscale.Node
	err   error
}

func (f *fakeHS) ListNodes(context.Context) ([]headscale.Node, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.nodes), f.err
}

// tailnet is the fake tailnet with a netmap DNS config.
type tailnet struct {
	*authz.FakeTailnet
	mu   sync.Mutex
	info core.TailnetDNS
}

func (t *tailnet) TailnetDNS(context.Context) (core.TailnetDNS, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.info, nil
}

type rig struct {
	t   *testing.T
	mux *core.Mux
	mod *Module
	hs  *fakeHS
	tn  *tailnet
	dns *servedns.Server
}

// backend is the store env of a rig: SQLite by default.
type backend func(t *testing.T) map[string]string

func sqliteEnv(t *testing.T) map[string]string {
	return map[string]string{"TAILNET_DNS_DB": filepath.Join(t.TempDir(), "dns.db")}
}

func redisEnv(t *testing.T) map[string]string {
	mr := miniredis.RunT(t)
	return map[string]string{"TAILNET_DNS_REDIS": "1", "REDIS_URL": "redis://" + mr.Addr()}
}

var backends = map[string]backend{"sqlite": sqliteEnv, "redis": redisEnv}

func newRig(t *testing.T) *rig { return newRigWith(t, sqliteEnv) }

func newRigWith(t *testing.T, be backend) *rig {
	t.Helper()
	p, _ := authz.NewPolicy(authz.Config{
		Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}, {Subject: "viewer@", Roles: []string{"viewer"}}},
	})
	old := authz.SetDefault(p)
	t.Cleanup(func() { authz.SetDefault(old) })
	tn := &tailnet{FakeTailnet: authz.NewFakeTailnet(map[string]authz.Who{
		ipOwner: {Login: "owner@"}, ipViewer: {Login: "viewer@"},
		ipDevA:  {Login: "tagged-devices", Tags: []string{"tag:dev-a"}},
		ipDevB:  {Login: "tagged-devices", Tags: []string{"tag:dev-b"}},
		ipAlice: {Login: "alice@example.com"},
	}), info: core.TailnetDNS{Suffix: base, Self4: netip.MustParseAddr(ipSelf), Extra: []core.ExtraRecord{
		{Name: "llm-proxy.example.com", Addr: netip.MustParseAddr("100.64.0.20")},
		{Name: "public.example.com", Addr: netip.MustParseAddr("192.0.2.1")}, // not a tailnet address: skipped
	}}}
	envMap := map[string]string{"SERVE_ZONE": zone, "TAILNET_DNS_LISTEN": "off"}
	for k, v := range be(t) {
		envMap[k] = v
	}
	env := core.MapEnv(envMap)
	hs := &fakeHS{nodes: []headscale.Node{
		{GivenName: "nas", IPAddresses: []string{"100.64.0.30", "fd7a:115c:a1e0::30"}, Online: true, Tags: []string{"tag:nas"}},
		{GivenName: "llm-a", IPAddresses: []string{"100.64.0.41"}, Online: true, Tags: []string{"tag:llm-proxy"}},
		{GivenName: "llm-b", IPAddresses: []string{"100.64.0.42"}, Online: false, Tags: []string{"tag:llm-proxy"}},
	}}
	dns := &servedns.Server{}
	m := New(env, hs, dns)
	mux := core.NewMux()
	if err := m.Register(&core.Routes{Admin: mux, Env: env, Tailnet: tn,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Stop(context.Background()) })
	if err := m.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, mux: mux, mod: m, hs: hs, tn: tn, dns: dns}
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

func (r *rig) lookup(name string) (dnsmessage.RCode, []string) {
	r.t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: dnsmessage.TypeA,
		Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	var p dnsmessage.Parser
	h, err := p.Start(r.dns.Answer(q))
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

func TestMirrorsExtraRecordsAndMagicDNS(t *testing.T) {
	r := newRig(t)
	if rc, ips := r.lookup("llm-proxy.example.com"); rc != dnsmessage.RCodeSuccess || !slices.Equal(ips, []string{"100.64.0.20"}) {
		t.Errorf("extra record: %v %v", rc, ips)
	}
	if rc, _ := r.lookup("public.example.com"); rc != dnsmessage.RCodeRefused {
		t.Errorf("an extra record with a public address must not be answered: %v", rc)
	}
	if rc, ips := r.lookup("nas." + base); rc != dnsmessage.RCodeSuccess || !slices.Equal(ips, []string{"100.64.0.30"}) {
		t.Errorf("MagicDNS: %v %v", rc, ips)
	}
	if rc, _ := r.lookup("nobody." + base); rc != dnsmessage.RCodeNameError {
		t.Errorf("unknown MagicDNS name: %v", rc)
	}
	if rc, _ := r.lookup("www.example.org"); rc != dnsmessage.RCodeRefused {
		t.Errorf("a public name must be refused, never recursed: %v", rc)
	}
}

func TestRecordLifecycle(t *testing.T) {
	for name, be := range backends {
		t.Run(name, func(t *testing.T) { testRecordLifecycle(t, newRigWith(t, be)) })
	}
}

func testRecordLifecycle(t *testing.T, r *rig) {
	// a viewer may not write
	if c, _ := r.do("POST", "/dns/records", ipViewer, map[string]any{"name": "x.example.com", "addresses": []string{"100.64.0.5"}}); c != http.StatusForbidden {
		t.Errorf("viewer create: %d", c)
	}
	c, b := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "Grafana.Example.com.", "addresses": []string{"100.64.0.5"}})
	if c != http.StatusCreated {
		t.Fatalf("create: %d %s", c, b)
	}
	if rc, ips := r.lookup("grafana.example.com"); rc != dnsmessage.RCodeSuccess || !slices.Equal(ips, []string{"100.64.0.5"}) {
		t.Errorf("fixed: %v %v", rc, ips)
	}
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "grafana.example.com", "node": "nas"}); c != http.StatusConflict {
		t.Errorf("duplicate: %d", c)
	}
	// a node target follows the node
	if c, b := r.do("PUT", "/dns/records/grafana.example.com", ipOwner, map[string]any{"node": "nas"}); c != http.StatusOK {
		t.Fatalf("replace: %d %s", c, b)
	}
	if _, ips := r.lookup("grafana.example.com"); !slices.Equal(ips, []string{"100.64.0.30"}) {
		t.Errorf("node target: %v", ips)
	}
	// a managed record wins over an extra record of the same name; a tag target gives the online nodes
	if c, b := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "llm-proxy.example.com", "tag": "tag:llm-proxy"}); c != http.StatusCreated {
		t.Fatalf("tag: %d %s", c, b)
	}
	if _, ips := r.lookup("llm-proxy.example.com"); !slices.Equal(ips, []string{"100.64.0.41"}) {
		t.Errorf("tag target: %v", ips)
	}
	// state: answers, split lines for headscale, the bridge view
	c, b = r.do("GET", "/dns/state", ipViewer, nil)
	if c != http.StatusOK {
		t.Fatalf("state: %d", c)
	}
	var st stateResp
	_ = json.Unmarshal(b, &st)
	if st.CanAdmin || st.BaseDomain != base || len(st.Records) != 2 || !slices.Equal(st.Split["grafana.example.com"], []string{ipSelf}) {
		t.Errorf("state: %+v", st)
	}
	if c, _ := r.do("DELETE", "/dns/records/grafana.example.com", ipOwner, nil); c != http.StatusNoContent {
		t.Errorf("delete: %d", c)
	}
	if rc, _ := r.lookup("grafana.example.com"); rc != dnsmessage.RCodeRefused {
		t.Errorf("deleted name still answered: %v", rc)
	}
	if c, _ := r.do("DELETE", "/dns/records/grafana.example.com", ipOwner, nil); c != http.StatusNotFound {
		t.Errorf("delete twice: %d", c)
	}
	es, _ := r.mod.st.AuditLog(10)
	if len(es) != 4 || es[0].Action != "record.delete" || !strings.HasPrefix(es[0].Actor, "owner") {
		t.Errorf("audit: %+v", es)
	}
}

func TestRecordValidation(t *testing.T) {
	r := newRig(t)
	for _, body := range []map[string]any{
		{"name": "x.example.com", "addresses": []string{"192.0.2.1"}},  // not a tailnet address
		{"name": "x.example.com", "addresses": []string{"10.0.0.1"}},   // subnet routes: out of scope
		{"name": "x.example.com"},                                      // no target
		{"name": "x.example.com", "node": "nas", "tag": "tag:nas"},     // two targets
		{"name": "x.example.com", "tag": "nas"},                        // not a tag
		{"name": "nas." + base, "addresses": []string{"100.64.0.5"}},   // under the base domain
		{"name": "x." + zone, "addresses": []string{"100.64.0.5"}},     // in the serve zone
		{"name": "*.example.com", "addresses": []string{"100.64.0.5"}}, // no wildcards
		{"name": "example", "addresses": []string{"100.64.0.5"}},       // one label
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "note": strings.Repeat("n", 201)},
	} {
		if c, b := r.do("POST", "/dns/records", ipOwner, body); c != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d %s", body, c, b)
		}
	}
	if c, _ := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "ttl": 5}); c != http.StatusBadRequest {
		t.Errorf("unknown field: %d", c)
	}
	if c, _ := r.do("PUT", "/dns/records/x.example.com", ipOwner, map[string]any{"name": "y.example.com", "node": "nas"}); c != http.StatusUnprocessableEntity {
		t.Errorf("rename: %d", c)
	}
	if c, _ := r.do("PUT", "/dns/records/x.example.com", ipOwner, map[string]any{"node": "nas"}); c != http.StatusNotFound {
		t.Errorf("replace missing: %d", c)
	}
}

func TestBridgeDNS(t *testing.T) {
	r := newRig(t)
	if _, ok := r.mod.BridgeDNS(); ok {
		t.Error("a bridge resolver while the listener is off")
	}
	r.mod.cur.Lock()
	r.mod.cur.listening = true
	r.mod.cur.Unlock()
	if c, b := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "grafana.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatalf("%d %s", c, b)
	}
	b, ok := r.mod.BridgeDNS()
	if !ok || b.Server != "tcp://"+ipSelf+":53" {
		t.Fatalf("bridge: %+v %v", b, ok)
	}
	want := []string{"domain:" + zone, "domain:" + base, "full:grafana.example.com", "full:llm-proxy.example.com"}
	if !slices.Equal(b.Domains, want) {
		t.Errorf("domains: %v", b.Domains)
	}
}

// A headscale failure keeps the old answers.
func TestHeadscaleFailureKeepsAnswers(t *testing.T) {
	r := newRig(t)
	r.hs.mu.Lock()
	r.hs.err = context.DeadlineExceeded
	r.hs.mu.Unlock()
	r.mod.Refresh(t.Context())
	if _, ips := r.lookup("nas." + base); !slices.Equal(ips, []string{"100.64.0.30"}) {
		t.Errorf("answers lost: %v", ips)
	}
	if c, b := r.do("GET", "/dns/state", ipViewer, nil); c != 200 || !strings.Contains(string(b), "headscale") {
		t.Errorf("state does not report the error: %s", b)
	}
}

// The backend is chosen by TAILNET_DNS_DB or TAILNET_DNS_REDIS=1; REDIS_URL alone does not turn the module on.
func TestBackendSelection(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://" + mr.Addr()
	for _, c := range []struct {
		env    map[string]string
		on     bool
		errSub string
	}{
		{map[string]string{"REDIS_URL": url}, false, ""},
		{map[string]string{"REDIS_URL": url, "TAILNET_DNS_REDIS": "0"}, false, ""},
		{map[string]string{"REDIS_URL": url, "TAILNET_DNS_REDIS": "1"}, true, ""},
		{map[string]string{"TAILNET_DNS_REDIS": "1"}, false, "needs REDIS_URL"},
		{map[string]string{"TAILNET_DNS_REDIS": "yes", "REDIS_URL": url}, false, "must be 1 or 0"},
		{map[string]string{"TAILNET_DNS_REDIS": "1", "REDIS_URL": url,
			"TAILNET_DNS_DB": filepath.Join(t.TempDir(), "x.db")}, false, "not both"},
	} {
		p, _ := authz.NewPolicy(authz.Config{Grants: []authz.Grant{{Subject: "owner@", Roles: []string{"owner"}}}})
		old := authz.SetDefault(p)
		m := New(core.MapEnv(c.env), &fakeHS{}, nil)
		err := m.Register(&core.Routes{Admin: core.NewMux(), Env: core.MapEnv(c.env),
			Tailnet: authz.NewFakeTailnet(nil), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
		authz.SetDefault(old)
		if c.errSub != "" {
			if err == nil || !strings.Contains(err.Error(), c.errSub) {
				t.Errorf("%v: err %v", c.env, err)
			}
			continue
		}
		if err != nil || m.Enabled() != c.on {
			t.Errorf("%v: on=%v err=%v", c.env, m.Enabled(), err)
		}
		_ = m.Stop(context.Background())
	}
}

func TestRedisKeys(t *testing.T) {
	mr := miniredis.RunT(t)
	r := newRigWith(t, func(*testing.T) map[string]string {
		return map[string]string{"TAILNET_DNS_REDIS": "1", "REDIS_URL": "redis://" + mr.Addr(), "FLY_APP_NAME": "nc"}
	})
	if c, b := r.do("POST", "/dns/records", ipOwner, map[string]any{"name": "x.example.com", "node": "nas"}); c != http.StatusCreated {
		t.Fatalf("%d %s", c, b)
	}
	if !mr.Exists("nc:dns:records") || !mr.Exists("nc:dns:audit") {
		t.Errorf("keys: %v", mr.Keys())
	}
}
