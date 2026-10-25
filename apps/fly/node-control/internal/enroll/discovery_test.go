package enroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"example.com/node-control/internal/headscale"
)

// All hosts, keys, short ids and tokens below are fake.
const (
	discKey   = "FAKEPUBKEY-ingress-3-0123456789abcdef"
	discSID   = "fakesid3-0123456789"
	discToken = "disc-token-value-3"
)

// infoAgent plays the agent of a discovered ingress: /v1/info, /v1/credentials and /v1/stats.
type infoAgent struct {
	srv *httptest.Server

	mu       sync.Mutex
	info     map[string]any
	status   int // 0: 200
	raw      string
	handler  http.HandlerFunc // replaces /v1/info when set
	infoHits int
	infoAuth []string
	puts     [][]Credential
}

func newInfoAgent(t *testing.T, name string) *infoAgent {
	a := &infoAgent{info: map[string]any{"name": name, "host": name + ".example.com", "port": 443,
		"sni": name + ".example.com", "public_key": discKey, "short_id": discSID}}
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/v1/info":
			a.infoHits++
			a.infoAuth = append(a.infoAuth, r.Header.Get("Authorization"))
			if a.handler != nil {
				a.handler(w, r)
				return
			}
			if a.status != 0 {
				w.WriteHeader(a.status)
				return
			}
			if a.raw != "" {
				io.WriteString(w, a.raw)
				return
			}
			json.NewEncoder(w).Encode(a.info)
		case r.Method == "PUT" && r.URL.Path == "/v1/credentials":
			var body struct {
				Credentials []Credential `json:"credentials"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			a.puts = append(a.puts, body.Credentials)
			w.WriteHeader(204)
		case r.Method == "GET" && r.URL.Path == "/v1/stats":
			w.Write([]byte("{}"))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *infoAgent) set(k string, v any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.info[k] = v
}

func (a *infoAgent) hits() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.infoHits
}

func (a *infoAgent) putCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.puts)
}

func (a *infoAgent) lastPut() []Credential {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.puts) == 0 {
		return nil
	}
	return a.puts[len(a.puts)-1]
}

// usePort points the agent port of discovered nodes at the fake server.
func usePort(t *testing.T, a *infoAgent) {
	t.Helper()
	_, port, _ := net.SplitHostPort(a.srv.Listener.Addr().String())
	old := ingressAgentPort
	ingressAgentPort = port
	t.Cleanup(func() { ingressAgentPort = old })
}

type nodeList struct {
	mu    sync.Mutex
	nodes []headscale.Node
	err   error
}

func (n *nodeList) Nodes(ctx context.Context) ([]headscale.Node, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]headscale.Node(nil), n.nodes...), n.err
}

func (n *nodeList) set(nodes ...headscale.Node) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nodes = nodes
}

func ingressNode(name string, online bool) headscale.Node {
	return headscale.Node{Name: name, Online: online, Tags: []string{"tag:ingress"}, IPAddresses: []string{"fd7a:115c::1", "127.0.0.1"}}
}

type clock struct{ t atomic.Int64 }

func (c *clock) now() time.Time          { return time.Unix(c.t.Load(), 0) }
func (c *clock) advance(d time.Duration) { c.t.Add(int64(d / time.Second)) }

type regFx struct {
	t      *testing.T
	reg    *Registry
	store  *Store
	nodes  *nodeList
	clk    *clock
	agent1 *stubAgent
	env    map[string]string
}

// newRegFx: one configured ingress "ingress-1" (stored credentials), a shared link host, and a
// discovered ingress "ingress-3" served by srv (derived credentials) once its node is listed.
func newRegFx(t *testing.T, mutate func(*RegistryConfig)) *regFx {
	t.Helper()
	f := &regFx{t: t, store: newStore(t, adminNow), nodes: &nodeList{}, clk: &clock{}, agent1: &stubAgent{},
		env: map[string]string{"AGENT_TOKEN_INGRESS_3": discToken}}
	f.clk.t.Store(adminNow)
	cfg := IngressConfig{ProfileName: "tunnel", LinkHost: "links.example.com", Ingresses: []Ingress{
		{Name: "ingress-1", Host: "ingress-1.example.com", PublicKey: "PUB1", ShortID: "aa11", SNI: "ingress-1.example.com", Port: 443, UUIDs: uuidsBase}}}.WithKey(testKey)
	rc := RegistryConfig{Cfg: cfg, Src: f.nodes, Getenv: func(k string) string { return f.env[k] }, Now: f.clk.now,
		LinkHostSet: true, Agents: map[string]Agent{"ingress-1": f.agent1}}
	if mutate != nil {
		mutate(&rc)
	}
	reg, err := NewRegistry(rc)
	if err != nil {
		t.Fatal(err)
	}
	reg.SetStore(f.store)
	f.reg = reg
	return f
}

func (f *regFx) view(name string) IngressView {
	f.t.Helper()
	for _, v := range f.reg.Current(context.Background(), true).Views {
		if v.Name == name {
			return v
		}
	}
	f.t.Fatalf("no view for %s", name)
	return IngressView{}
}

func (f *regFx) names() []string {
	var out []string
	for _, in := range f.reg.Current(context.Background(), true).Cfg.Ingresses {
		out = append(out, in.Name)
	}
	return out
}

func targetNames(c Current) []string {
	var out []string
	for _, t := range c.Targets {
		out = append(out, t.Name)
	}
	return out
}

func TestDiscoveredIngressNotUsedUntilApproved(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)

	t.Run("pending is neither in profiles nor synced", func(t *testing.T) {
		cur := f.reg.Current(context.Background(), true)
		if v := f.view("ingress-3"); v.Approval != stPending || v.Active {
			t.Fatalf("%+v", v)
		}
		if got := strings.Join(f.names(), ","); got != "ingress-1" {
			t.Fatalf("profile ingresses %s", got)
		}
		if got := strings.Join(targetNames(cur), ","); got != "ingress-1" {
			t.Fatalf("sync targets %s", got)
		}
		SyncAll(context.Background(), f.store, cur.Cfg, cur.Targets)
		if a.putCount() != 0 {
			t.Fatal("credentials were pushed to an unapproved ingress")
		}
		if len(cur.Pending) != 1 || cur.Pending[0] != "ingress-3" {
			t.Fatalf("pending %v", cur.Pending)
		}
	})
	t.Run("approved is used and receives its own derived set", func(t *testing.T) {
		fp := f.view("ingress-3").Fingerprint
		if err := f.reg.Approve(context.Background(), "ingress-3", fp); err != nil {
			t.Fatal(err)
		}
		cur := f.reg.Current(context.Background(), true)
		if got := strings.Join(f.names(), ","); got != "ingress-1,ingress-3" {
			t.Fatalf("profile ingresses %s", got)
		}
		if cfgIng := cur.Cfg.Ingresses[1]; cfgIng.UUIDs != uuidsDerived || cfgIng.Host != "ingress-3.example.com" {
			t.Fatalf("%+v", cfgIng)
		}
		if !SyncAll(context.Background(), f.store, cur.Cfg, cur.Targets) {
			t.Fatal("sync failed")
		}
		base, _ := f.store.ActiveCredentials()
		want, _ := cur.Cfg.CredentialsFor(cur.Cfg.Ingresses[1], base)
		got := a.lastPut()
		if len(got) != len(want) || len(got) == 0 {
			t.Fatalf("%v", got)
		}
		for i := range want {
			if got[i] != want[i] || got[i].UUID == base[i].UUID {
				t.Fatalf("the discovered ingress must get derived credentials: %v", got)
			}
		}
	})
}

func TestChangedParametersNeedReapproval(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)
	fp := f.view("ingress-3").Fingerprint
	if err := f.reg.Approve(context.Background(), "ingress-3", fp); err != nil {
		t.Fatal(err)
	}
	cur := f.reg.Current(context.Background(), true)
	SyncAll(context.Background(), f.store, cur.Cfg, cur.Targets)
	pushes := a.putCount()
	if pushes != 1 {
		t.Fatalf("pushes %d", pushes)
	}

	// the same name, different Reality parameters: only the name is the same
	a.set("public_key", "ANOTHER-KEY-0123456789")
	f.clk.advance(infoTTL + time.Second)
	cur = f.reg.Current(context.Background(), true)
	if v := f.view("ingress-3"); v.Approval != stChanged || v.Active {
		t.Fatalf("%+v", v)
	}
	if got := strings.Join(f.names(), ","); got != "ingress-1" {
		t.Fatalf("a changed ingress stays in profiles: %s", got)
	}
	SyncAll(context.Background(), f.store, cur.Cfg, cur.Targets)
	if a.putCount() != pushes {
		t.Fatal("a changed ingress still receives credentials")
	}
	if len(cur.Pending) != 1 {
		t.Fatalf("pending %v", cur.Pending)
	}

	// the old fingerprint cannot approve the new parameters; the new one can
	err := f.reg.Approve(context.Background(), "ingress-3", fp)
	var ae *ApprovalError
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("%v", err)
	}
	if err := f.reg.Approve(context.Background(), "ingress-3", f.view("ingress-3").Fingerprint); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.names(), ","); got != "ingress-1,ingress-3" {
		t.Fatal(got)
	}
}

func TestApproveRequiresMatchingFingerprint(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	fp := f.view("ingress-3").Fingerprint
	status := func(err error) int {
		var ae *ApprovalError
		if errors.As(err, &ae) {
			return ae.Status
		}
		if err != nil {
			t.Fatalf("unexpected %v", err)
		}
		return 200
	}
	ctx := context.Background()
	t.Run("wrong fingerprint", func(t *testing.T) {
		if s := status(f.reg.Approve(ctx, "ingress-3", strings.Repeat("0", 64))); s != 409 {
			t.Fatal(s)
		}
		if got, _ := f.store.Approvals(); len(got) != 0 {
			t.Fatal("approved on a mismatch")
		}
	})
	t.Run("parameters changed after the page was loaded: the cache is not trusted", func(t *testing.T) {
		a.set("short_id", "changedsid-999999")
		if s := status(f.reg.Approve(ctx, "ingress-3", fp)); s != 409 { // no clock advance
			t.Fatal(s)
		}
		fp = f.view("ingress-3").Fingerprint
	})
	t.Run("unknown name", func(t *testing.T) {
		if s := status(f.reg.Approve(ctx, "ingress-9", fp)); s != 404 {
			t.Fatal(s)
		}
	})
	t.Run("configured ingress", func(t *testing.T) {
		if s := status(f.reg.Approve(ctx, "ingress-1", fp)); s != 409 {
			t.Fatal(s)
		}
	})
	t.Run("matching fingerprint", func(t *testing.T) {
		fresh := f.view("ingress-3").Fingerprint
		if s := status(f.reg.Approve(ctx, "ingress-3", fresh)); s != 200 {
			t.Fatal(s)
		}
		if got, _ := f.store.Approvals(); got["ingress-3"] != fresh {
			t.Fatalf("%v", got)
		}
	})
}

func TestFingerprintIsTheCanonicalJSONHash(t *testing.T) {
	in := Ingress{Name: "ingress-3", Host: "ingress-3.example.com", Port: 443, SNI: "sni.example.com", PublicKey: "K<&>", ShortID: "ab"}
	canon := `{"name":"ingress-3","host":"ingress-3.example.com","port":443,"sni":"sni.example.com","public_key":"K<&>","short_id":"ab"}`
	sum := sha256.Sum256([]byte(canon))
	if got := fingerprint(in); got != hex.EncodeToString(sum[:]) {
		t.Fatalf("%s", got)
	}
	in2 := in
	in2.ShortID = "ac"
	if fingerprint(in2) == fingerprint(in) {
		t.Fatal("a changed value must change the fingerprint")
	}
}

func TestUnapproveStopsSyncAndProfiles(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)
	ctx := context.Background()
	if err := f.reg.Approve(ctx, "ingress-3", f.view("ingress-3").Fingerprint); err != nil {
		t.Fatal(err)
	}
	cur := f.reg.Current(ctx, true)
	SyncAll(ctx, f.store, cur.Cfg, cur.Targets)
	n := a.putCount()
	if err := f.reg.Unapprove(ctx, "ingress-3"); err != nil {
		t.Fatal(err)
	}
	cur = f.reg.Current(ctx, false) // no refresh needed: it takes effect at once
	if got := strings.Join(f.names(), ","); got != "ingress-1" {
		t.Fatal(got)
	}
	SyncAll(ctx, f.store, cur.Cfg, cur.Targets)
	if a.putCount() != n {
		t.Fatal("an un-approved ingress is still synced")
	}
	if v := f.view("ingress-3"); v.Approval != stPending {
		t.Fatalf("%+v", v)
	}
	t.Run("errors", func(t *testing.T) {
		var ae *ApprovalError
		if err := f.reg.Unapprove(ctx, "ingress-1"); !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatal(err)
		}
		if err := f.reg.Unapprove(ctx, "ingress-9"); !errors.As(err, &ae) || ae.Status != 404 {
			t.Fatal(err)
		}
	})
}

func TestPendingListShowsPrefixesOnly(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	v := f.view("ingress-3")
	if v.PublicKeyPref != discKey[:6] || v.ShortIDPref != discSID[:6] {
		t.Fatalf("%+v", v)
	}
	raw, _ := json.Marshal(f.reg.Current(context.Background(), true))
	for _, secret := range []string{discKey, discSID, discToken} {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("full value %q in the listing", secret)
		}
	}
	// the key material of the approved ingress is not listed in full either
	if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(f.reg.Current(context.Background(), true).Views)
	if strings.Contains(string(raw), discKey) || strings.Contains(string(raw), discSID) {
		t.Fatal("full key material in the listing")
	}
}

func TestInfoFetchBoundedAndNoRedirects(t *testing.T) {
	ctx := context.Background()
	t.Run("redirects are not followed", func(t *testing.T) {
		var hit atomic.Int32
		target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hit.Add(1)
			w.Write([]byte("{}"))
		}))
		defer target.Close()
		a := newInfoAgent(t, "ingress-3")
		a.handler = func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", true))
		if v := f.view("ingress-3"); v.Approval != stUnreachable || v.Active {
			t.Fatalf("%+v", v)
		}
		if hit.Load() != 0 {
			t.Fatal("the redirect was followed (and the token would have gone with it)")
		}
	})
	t.Run("a response over 64 KiB is invalid", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		a.raw = `{"name":"ingress-3","pad":"` + strings.Repeat("x", maxInfoBody) + `"}`
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", true))
		if v := f.view("ingress-3"); v.Approval != stInvalid {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("a slow agent is cut off", func(t *testing.T) {
		old := infoTimeout
		infoTimeout = 150 * time.Millisecond
		defer func() { infoTimeout = old }()
		release := make(chan struct{})
		a := newInfoAgent(t, "ingress-3")
		a.handler = func(w http.ResponseWriter, r *http.Request) { <-release }
		t.Cleanup(func() { close(release) })
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", true))
		start := time.Now()
		v := f.view("ingress-3")
		if time.Since(start) > 3*time.Second || v.Approval != stUnreachable {
			t.Fatalf("%v %+v", time.Since(start), v)
		}
	})
	t.Run("the request carries the ingress token and one fetch per minute", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", true))
		f.reg.Current(ctx, true)
		f.reg.Current(ctx, true)
		f.reg.Current(ctx, true)
		if a.hits() != 1 {
			t.Fatalf("fetches %d within a minute", a.hits())
		}
		if a.infoAuth[0] != "Bearer "+discToken {
			t.Fatalf("auth %q", a.infoAuth[0])
		}
		f.clk.advance(infoTTL + time.Second)
		f.reg.Current(ctx, true)
		if a.hits() != 2 {
			t.Fatalf("fetches %d after the interval", a.hits())
		}
	})
	t.Run("a node that is offline is not asked", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", false))
		if v := f.view("ingress-3"); v.Approval != stUnreachable || a.hits() != 0 {
			t.Fatalf("%+v hits=%d", v, a.hits())
		}
	})
}

func TestInfoValidation(t *testing.T) {
	cases := map[string]func(a *infoAgent){
		"name differs from the node":    func(a *infoAgent) { a.set("name", "ingress-4") },
		"invalid host":                  func(a *infoAgent) { a.set("host", "https://ingress-3.example.com/") },
		"empty public key":              func(a *infoAgent) { a.set("public_key", "") },
		"port out of range":             func(a *infoAgent) { a.set("port", 70000) },
		"not JSON":                      func(a *infoAgent) { a.raw = "<html>" },
		"host used by a configured one": func(a *infoAgent) { a.set("host", "ingress-1.example.com") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			a := newInfoAgent(t, "ingress-3")
			mutate(a)
			usePort(t, a)
			f := newRegFx(t, nil)
			f.nodes.set(ingressNode("ingress-3", true))
			v := f.view("ingress-3")
			if v.Approval != stInvalid || v.Active {
				t.Fatalf("%+v", v)
			}
			if strings.Contains(v.Detail, discKey) || strings.Contains(v.Detail, discSID) {
				t.Fatalf("detail echoes key material: %q", v.Detail)
			}
			var ae *ApprovalError
			if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint); err == nil || !errors.As(err, &ae) {
				t.Fatalf("an invalid ingress must not be approvable: %v", err)
			}
		})
	}
	t.Run("an agent cannot choose its own URL or credential mode", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		a.set("agent_url", "http://127.0.0.1:1")
		a.set("uuids", "base")
		a.set("token_env", "AGENT_TOKEN")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNode("ingress-3", true))
		f.reg.Approve(context.Background(), "ingress-3", f.view("ingress-3").Fingerprint)
		in := f.reg.Current(context.Background(), true).Cfg.Ingresses[1]
		if in.UUIDs != uuidsDerived || in.AgentURL != "" || in.TokenEnv != "" {
			t.Fatalf("%+v", in)
		}
	})
}

func TestApprovedIngressSurvivesATransientInfoFailure(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	if err := f.reg.Approve(context.Background(), "ingress-3", f.view("ingress-3").Fingerprint); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	a.status = 503
	a.mu.Unlock()
	f.clk.advance(infoTTL + time.Second)
	// a revocation must still reach the ingress when only its /v1/info is failing
	if v := f.view("ingress-3"); v.Approval != stApproved || !v.Active || v.Detail == "" {
		t.Fatalf("%+v", v)
	}
}

func TestDiscoveredIngressNeedsTokenVariable(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	delete(f.env, "AGENT_TOKEN_INGRESS_3")
	f.nodes.set(ingressNode("ingress-3", true))
	v := f.view("ingress-3")
	if v.Approval != stNoToken || v.Active {
		t.Fatalf("%+v", v)
	}
	if a.hits() != 0 {
		t.Fatal("an ingress without a token variable was asked")
	}
	if !strings.Contains(v.Detail, "no token") && v.Detail == "" {
		t.Fatalf("%q", v.Detail)
	}
	if err := f.reg.Approve(context.Background(), "ingress-3", strings.Repeat("0", 64)); err == nil {
		t.Fatal("approved an ingress without a token")
	}
}

func TestDiscoveryOffWithoutLinkHost(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, func(rc *RegistryConfig) { rc.LinkHostSet = false })
	f.nodes.set(ingressNode("ingress-3", true))
	cur := f.reg.Current(context.Background(), true)
	if cur.Discovery.Enabled || !strings.Contains(cur.Discovery.Reason, "link_host") {
		t.Fatalf("%+v", cur.Discovery)
	}
	if len(cur.Views) != 1 || a.hits() != 0 {
		t.Fatalf("views %d hits %d", len(cur.Views), a.hits())
	}
	var ae *ApprovalError
	if err := f.reg.Approve(context.Background(), "ingress-3", "x"); !errors.As(err, &ae) || ae.Status != 404 {
		t.Fatal(err)
	}
}

func TestOnlineFlagFromHeadscale(t *testing.T) {
	f := newRegFx(t, nil)
	ctx := context.Background()
	if v := f.reg.Current(ctx, true).Views[0]; v.Online != nil {
		t.Fatalf("no node: online must be null, got %v", *v.Online)
	}
	f.nodes.set(ingressNode("ingress-1", true))
	if v := f.reg.Current(ctx, true).Views[0]; v.Online == nil || !*v.Online {
		t.Fatalf("%v", v.Online)
	}
	f.nodes.set(ingressNode("ingress-1", false))
	if v := f.reg.Current(ctx, true).Views[0]; v.Online == nil || *v.Online {
		t.Fatalf("%v", v.Online)
	}
	// a node without the tag is not the ingress
	n := ingressNode("ingress-1", true)
	n.Tags = []string{"tag:exit"}
	f.nodes.set(n)
	if v := f.reg.Current(ctx, true).Views[0]; v.Online != nil {
		t.Fatal("an untagged node counts as the ingress")
	}
	// a failing node list keeps the last answer
	f.nodes.set(ingressNode("ingress-1", true))
	f.reg.Current(ctx, true)
	f.nodes.mu.Lock()
	f.nodes.err = errors.New("down")
	f.nodes.mu.Unlock()
	if v := f.reg.Current(ctx, true).Views[0]; v.Online == nil || !*v.Online {
		t.Fatal("a failing node list must not wipe the last known state")
	}
}

func TestAgentURLFromHeadscale(t *testing.T) {
	ctx := context.Background()
	a := newInfoAgent(t, "ingress-2")
	first := newInfoAgent(t, "ingress-1")
	usePort(t, a)
	nodes := &nodeList{}
	cfg := IngressConfig{LinkHost: "links.example.com", Ingresses: []Ingress{
		{Name: "ingress-1", Host: "ingress-1.example.com", PublicKey: "P1", ShortID: "a1", SNI: "ingress-1.example.com", Port: 443, UUIDs: uuidsBase, AgentURL: first.srv.URL},
		{Name: "ingress-2", Host: "ingress-2.example.com", PublicKey: "P2", ShortID: "b2", SNI: "ingress-2.example.com", Port: 443, UUIDs: uuidsDerived},
	}}.WithKey(testKey)
	env := map[string]string{"AGENT_TOKEN": "t1", "AGENT_TOKEN_INGRESS_2": "t2"}
	reg, err := NewRegistry(RegistryConfig{Cfg: cfg, Src: nodes, Getenv: func(k string) string { return env[k] }})
	if err != nil {
		t.Fatal(err)
	}
	store := newStore(t, adminNow)
	reg.SetStore(store)
	store.CreateDevice("n", "o", "sing-box", adminFuture)

	t.Run("no tagged node: that ingress is unreachable, the other is fine", func(t *testing.T) {
		cur := reg.Current(ctx, true)
		if SyncAll(ctx, store, cur.Cfg, cur.Targets) {
			t.Fatal("want false")
		}
		s1, s2 := cur.Targets[0].State.Snapshot(), cur.Targets[1].State.Snapshot()
		if s1.State != "ok" || s1.Pending || s2.State != "unreachable" || !s2.Pending {
			t.Fatalf("%+v %+v", s1, s2)
		}
	})
	t.Run("the node's tailnet IPv4 and port 9000 are used", func(t *testing.T) {
		nodes.set(ingressNode("ingress-2", true))
		cur := reg.Current(ctx, true)
		if !SyncAll(ctx, store, cur.Cfg, cur.Targets) {
			t.Fatal("want true")
		}
		if a.putCount() != 1 {
			t.Fatalf("puts %d", a.putCount())
		}
	})
}

func TestNewRegistryTokenErrorsNameTheVariableOnly(t *testing.T) {
	cfg := IngressConfig{LinkHost: "links.example.com", Ingresses: []Ingress{
		{Name: "ingress-1", Host: "ingress-1.example.com", PublicKey: "P1", ShortID: "a1", Port: 443, UUIDs: uuidsBase, AgentURL: "http://agent1.example.com"},
		{Name: "ingress-2", Host: "ingress-2.example.com", PublicKey: "P2", ShortID: "b2", Port: 443, UUIDs: uuidsDerived, AgentURL: "http://agent2.example.com", TokenEnv: "AGENT_TOKEN_TWO"},
	}}
	env := map[string]string{"AGENT_TOKEN": "first-token-value", "AGENT_TOKEN_INGRESS_2": "wrong-variable-value"}
	_, err := NewRegistry(RegistryConfig{Cfg: cfg, Getenv: func(k string) string { return env[k] }})
	want := "ingress ingress-2: token variable AGENT_TOKEN_TWO is not set"
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
	for _, v := range env {
		if strings.Contains(err.Error(), v) {
			t.Fatalf("value %q in the error", v)
		}
	}
	t.Run("the first ingress falls back to AGENT_TOKEN, the others do not", func(t *testing.T) {
		cfg.Ingresses[1].TokenEnv = ""
		delete(env, "AGENT_TOKEN_INGRESS_2")
		_, err := NewRegistry(RegistryConfig{Cfg: cfg, Getenv: func(k string) string { return env[k] }})
		if err == nil || err.Error() != "ingress ingress-2: token variable AGENT_TOKEN_INGRESS_2 is not set" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("no agent url and no node source", func(t *testing.T) {
		cfg.Ingresses[1].AgentURL = ""
		env["AGENT_TOKEN_INGRESS_2"] = "x"
		if _, err := NewRegistry(RegistryConfig{Cfg: cfg, Getenv: func(k string) string { return env[k] }}); err == nil || !strings.Contains(err.Error(), "ingress-2") {
			t.Fatalf("%v", err)
		}
	})
}
