package enroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	status   int // 0: 200 (for /v1/info)
	putCode  int // 0: 204 (for PUT /v1/credentials; a failing answer records nothing)
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
			if a.putCode != 0 {
				w.WriteHeader(a.putCode)
				return
			}
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

// ingressNode is the node of "ingress-N" with the headscale id N.
func ingressNode(name string, online bool) headscale.Node {
	return ingressNodeID(name, uint64(name[len(name)-1]-'0'), online)
}

// mkey is the machine key of the fake node with this id.
func mkey(id uint64) string { return fmt.Sprintf("mkey:%064x", id) }

// renamedNode is a node the administrator renamed (`headscale nodes rename`): its given name no
// longer follows the hostname the machine reports.
func renamedNode(name string, id uint64, ip string) headscale.Node {
	return headscale.Node{ID: id, MachineKey: mkey(id), Name: fmt.Sprintf("box-%d", id), GivenName: name, Online: true,
		Tags: []string{"tag:ingress"}, IPAddresses: []string{ip}}
}

func ingressNodeID(name string, id uint64, online bool) headscale.Node {
	return headscale.Node{ID: id, MachineKey: mkey(id), Name: name, GivenName: name, Online: online, Tags: []string{"tag:ingress"}, IPAddresses: []string{"fd7a:115c::1", "127.0.0.1"}}
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

// approve approves what the ingress publishes now, on the node that publishes it.
func (f *regFx) approve(name string) error {
	f.t.Helper()
	v := f.view(name)
	return f.reg.Approve(context.Background(), name, v.Fingerprint, v.NodeID)
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
		if err := f.reg.Approve(context.Background(), "ingress-3", fp, 3); err != nil {
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
	if err := f.reg.Approve(context.Background(), "ingress-3", fp, 3); err != nil {
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
	err := f.reg.Approve(context.Background(), "ingress-3", fp, 3)
	var ae *ApprovalError
	if !errors.As(err, &ae) || ae.Status != 409 {
		t.Fatalf("%v", err)
	}
	if err := f.approve("ingress-3"); err != nil {
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
		if s := status(f.reg.Approve(ctx, "ingress-3", strings.Repeat("0", 64), 3)); s != 409 {
			t.Fatal(s)
		}
		if got, _ := f.store.Approvals(); len(got) != 0 {
			t.Fatal("approved on a mismatch")
		}
	})
	t.Run("parameters changed after the page was loaded: the cache is not trusted", func(t *testing.T) {
		a.set("short_id", "changedsid-999999")
		if s := status(f.reg.Approve(ctx, "ingress-3", fp, 3)); s != 409 { // no clock advance
			t.Fatal(s)
		}
		fp = f.view("ingress-3").Fingerprint
	})
	t.Run("unknown name", func(t *testing.T) {
		if s := status(f.reg.Approve(ctx, "ingress-9", fp, 9)); s != 404 {
			t.Fatal(s)
		}
	})
	t.Run("configured ingress", func(t *testing.T) {
		if s := status(f.reg.Approve(ctx, "ingress-1", fp, 1)); s != 409 {
			t.Fatal(s)
		}
	})
	t.Run("matching fingerprint", func(t *testing.T) {
		fresh := f.view("ingress-3").Fingerprint
		if s := status(f.reg.Approve(ctx, "ingress-3", fresh, 3)); s != 200 {
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
	if err := f.approve("ingress-3"); err != nil {
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
	if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint, v.NodeID); err != nil {
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
			if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint, v.NodeID); err == nil || !errors.As(err, &ae) {
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
		f.approve("ingress-3")
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
	if err := f.approve("ingress-3"); err != nil {
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
	if err := f.reg.Approve(context.Background(), "ingress-3", strings.Repeat("0", 64), 3); err == nil {
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
	if err := f.reg.Approve(context.Background(), "ingress-3", "x", 3); !errors.As(err, &ae) || ae.Status != 404 {
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
		nodes.set(renamedNode("ingress-2", 2, "127.0.0.1"))
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
	want := "bridge ingress-2: token variable AGENT_TOKEN_TWO is not set"
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
		if err == nil || err.Error() != "bridge ingress-2: token variable AGENT_TOKEN_INGRESS_2 is not set" {
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

// swapInfo serves per-token info documents from one fake server (every fake node has the same
// address, so the token tells the ingresses apart).
func swapInfo(a *infoAgent, byToken map[string]map[string]any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handler = func(w http.ResponseWriter, r *http.Request) {
		doc, ok := byToken[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(doc)
	}
}

func approvedIngress3(t *testing.T) (*regFx, *infoAgent) {
	t.Helper()
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)
	if err := f.approve("ingress-3"); err != nil {
		t.Fatal(err)
	}
	cur := f.reg.Current(context.Background(), true)
	if !SyncAll(context.Background(), f.store, cur.Cfg, cur.Targets) || a.putCount() != 1 {
		t.Fatal("setup: the approved ingress was not synced")
	}
	return f, a
}

func TestApprovalPinsTheNode(t *testing.T) {
	ctx := context.Background()
	t.Run("a replacement node whose info fails is not synced", func(t *testing.T) {
		f, a := approvedIngress3(t)
		a.mu.Lock()
		a.status = 503
		a.mu.Unlock()
		f.nodes.set(ingressNodeID("ingress-3", 30, true))
		f.clk.advance(infoTTL + time.Second)
		cur := f.reg.Current(ctx, true)
		if v := f.view("ingress-3"); v.Active || v.Approval != stInvalid || v.Fingerprint != "" {
			t.Fatalf("the old node's parameters must not be shown for the new node: %+v", v)
		}
		SyncAll(ctx, f.store, cur.Cfg, cur.Targets)
		if a.putCount() != 1 {
			t.Fatal("credentials were pushed to a node that replaced the approved one")
		}
	})
	t.Run("identical parameters on another node id are not even asked for", func(t *testing.T) {
		f, a := approvedIngress3(t)
		hits := a.hits()
		f.nodes.set(ingressNodeID("ingress-3", 30, true))
		f.clk.advance(infoTTL + time.Second)
		cur := f.reg.Current(ctx, true)
		if v := f.view("ingress-3"); v.Approval != stInvalid || v.Active {
			t.Fatalf("%+v", v)
		}
		if a.hits() != hits {
			t.Fatal("the token went to a node the approval is not pinned to")
		}
		SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
		if a.putCount() != 1 {
			t.Fatal("a node that replaced the approved one was synced")
		}
		// only the operator's explicit removal of the approval lets the new node be reviewed
		if err := f.reg.Unapprove(ctx, "ingress-3"); err != nil {
			t.Fatal(err)
		}
		if v := f.view("ingress-3"); v.Approval != stPending || v.NodeID != 30 {
			t.Fatalf("%+v", v)
		}
		if err := f.approve("ingress-3"); err != nil {
			t.Fatal(err)
		}
		if v := f.view("ingress-3"); v.Approval != stApproved || !v.Active {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("last good parameters are kept only for the same node", func(t *testing.T) {
		f, a := approvedIngress3(t)
		a.mu.Lock()
		a.status = 503
		a.mu.Unlock()
		f.clk.advance(infoTTL + time.Second)
		if v := f.view("ingress-3"); !v.Active || v.Approval != stApproved {
			t.Fatalf("same node: %+v", v)
		}
	})
	t.Run("two nodes with one name, none of them the approved one, are invalid", func(t *testing.T) {
		f, a := approvedIngress3(t)
		f.nodes.set(ingressNodeID("ingress-3", 30, true), ingressNodeID("ingress-3", 31, true))
		f.clk.advance(infoTTL + time.Second)
		hits := a.hits()
		cur := f.reg.Current(ctx, true)
		if v := f.view("ingress-3"); v.Approval != stInvalid || v.Active {
			t.Fatalf("%+v", v)
		}
		if a.hits() != hits {
			t.Fatal("the token was sent to a node of an ambiguous name")
		}
		SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
		if a.putCount() != 1 {
			t.Fatal("an ambiguous name was synced")
		}
		var ae *ApprovalError
		if err := f.reg.Approve(ctx, "ingress-3", strings.Repeat("0", 64), 3); !errors.As(err, &ae) {
			t.Fatal(err)
		}
	})
	t.Run("the agent is found by the pinned node id", func(t *testing.T) {
		f, _ := approvedIngress3(t)
		f.nodes.set(ingressNodeID("ingress-3", 3, true))
		res := f.reg.resolverFor("ingress-3", 3, mkey(3))
		if u, err := res(ctx); err != nil || !strings.HasPrefix(u, "http://127.0.0.1:") {
			t.Fatalf("%q %v", u, err)
		}
		if _, err := f.reg.resolverFor("ingress-3", 30, mkey(30))(ctx); err == nil {
			t.Fatal("a node with another id was resolved")
		}
	})
}

func TestUnapprovedNodeCannotInvalidateAnApprovedOne(t *testing.T) {
	ctx := context.Background()
	a := newInfoAgent(t, "ingress-5")
	usePort(t, a)
	doc := func(name, host string) map[string]any {
		return map[string]any{"name": name, "host": host, "port": 443, "sni": host, "public_key": discKey, "short_id": discSID}
	}
	swapInfo(a, map[string]map[string]any{
		discToken: doc("ingress-5", "ingress-5.example.com"),
		"tok4":    doc("ingress-4", "ingress-5.example.com"), // claims the approved ingress's host
	})
	f := newRegFx(t, nil)
	f.env["AGENT_TOKEN_INGRESS_5"] = discToken
	f.env["AGENT_TOKEN_INGRESS_4"] = "tok4"
	f.nodes.set(ingressNode("ingress-5", true))
	if err := f.approve("ingress-5"); err != nil {
		t.Fatal(err)
	}
	f.nodes.set(ingressNode("ingress-5", true), ingressNode("ingress-4", true)) // sorts first
	f.clk.advance(infoTTL + time.Second)
	f.reg.Current(ctx, true)
	if v := f.view("ingress-5"); v.Approval != stApproved || !v.Active {
		t.Fatalf("an unapproved node invalidated an approved one: %+v", v)
	}
	if v := f.view("ingress-4"); v.Approval != stInvalid || v.Active {
		t.Fatalf("%+v", v)
	}
}

func TestLegacyFormUsesOnlyAgentToken(t *testing.T) {
	cfg := IngressConfig{LinkHost: "h.example.com", Ingresses: []Ingress{
		{Name: "default", Host: "h.example.com", PublicKey: "P", ShortID: "a1", Port: 443, UUIDs: uuidsBase}}}
	env := map[string]string{"AGENT_TOKEN_DEFAULT": "wrong-variable"}
	_, err := NewRegistry(RegistryConfig{Cfg: cfg, Legacy: true, AgentURL: "http://a.example.com", Getenv: func(k string) string { return env[k] }})
	if err == nil || !strings.Contains(err.Error(), "AGENT_TOKEN is not set") || strings.Contains(err.Error(), "wrong-variable") {
		t.Fatalf("%v", err)
	}
	env["AGENT_TOKEN"] = "ok"
	if _, err := NewRegistry(RegistryConfig{Cfg: cfg, Legacy: true, AgentURL: "http://a.example.com", Getenv: func(k string) string { return env[k] }}); err != nil {
		t.Fatal(err)
	}
}

func TestFirstIngressTokenErrorNamesTheFallback(t *testing.T) {
	cfg := IngressConfig{LinkHost: "h.example.com", Ingresses: []Ingress{
		{Name: "ingress-1", Host: "h.example.com", PublicKey: "P", ShortID: "a1", Port: 443, UUIDs: uuidsBase, AgentURL: "http://a.example.com"}}}
	_, err := NewRegistry(RegistryConfig{Cfg: cfg, Getenv: func(string) string { return "" }})
	want := "bridge ingress-1: token variable AGENT_TOKEN_INGRESS_1 is not set (nor AGENT_TOKEN)"
	if err == nil || err.Error() != want {
		t.Fatalf("%v", err)
	}
}

func TestNodeIDZeroNeverPins(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNodeID("ingress-3", 0, true))
	v := f.view("ingress-3")
	if v.Approval != stInvalid || v.Active {
		t.Fatalf("%+v", v)
	}
	if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint, v.NodeID); err == nil {
		t.Fatal("approved a node without an id")
	}
	// a stored approval without a pin never matches, whatever the node id
	f.nodes.set(ingressNode("ingress-3", true))
	f.clk.advance(infoTTL + time.Second)
	fp := f.view("ingress-3").Fingerprint
	f.store.ApproveNode("ingress-3", fp, 0, "")
	if v := f.view("ingress-3"); v.Active || v.Approval == stApproved {
		t.Fatalf("%+v", v)
	}
}

func TestApprovalPredatingPinningNeedsReapproval(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	f.store.Approve("ingress-3", f.view("ingress-3").Fingerprint) // a row from before node pinning
	v := f.view("ingress-3")
	if v.Approval != stChanged || v.Active || v.Detail != "approval predates node pinning, approve again" {
		t.Fatalf("%+v", v)
	}
	if err := f.reg.Approve(context.Background(), "ingress-3", v.Fingerprint, v.NodeID); err != nil {
		t.Fatal(err)
	}
	if v := f.view("ingress-3"); v.Approval != stApproved || !v.Active {
		t.Fatalf("%+v", v)
	}
}

func TestSyncReachesApprovedIngressBeforeTheFirstProbe(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T, rowNode uint64, nodes ...headscale.Node) (*regFx, *infoAgent) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(nodes...)
		f.store.ApproveNode("ingress-3", "fingerprint-from-before-the-restart", rowNode, mkey(rowNode))
		f.store.CreateDevice("n", "o", "sing-box", adminFuture)
		return f, a
	}
	t.Run("the approved node receives the set without any /v1/info reading", func(t *testing.T) {
		f, a := setup(t, 3, ingressNode("ingress-3", true))
		cur := f.reg.Current(ctx, false)
		if len(cur.Pending) != 1 || cur.Views[1].Active {
			t.Fatalf("an unverified ingress must not be active: %+v", cur.Views)
		}
		if !SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets) || a.putCount() != 1 {
			t.Fatalf("pushes %d", a.putCount())
		}
		if a.hits() != 0 {
			t.Fatal("the sync path read /v1/info")
		}
		if len(cur.Cfg.Ingresses) != 1 {
			t.Fatal("an unverified ingress must not be offered in profiles")
		}
	})
	t.Run("a node with another id is not reached", func(t *testing.T) {
		f, a := setup(t, 3, ingressNodeID("ingress-3", 30, true))
		cur := f.reg.Current(ctx, false)
		SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
		if a.putCount() != 0 {
			t.Fatal("pushed to a node that is not the approved one")
		}
	})
	t.Run("an approval without a pin is not reached", func(t *testing.T) {
		f, a := setup(t, 0, ingressNodeID("ingress-3", 3, true))
		cur := f.reg.Current(ctx, false)
		SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
		if a.putCount() != 0 {
			t.Fatal("pushed on an unpinned approval")
		}
	})
}

func TestImpostorWithTheSameNameCannotTakeOver(t *testing.T) {
	ctx := context.Background()
	t.Run("the pinned node keeps being read and synced while another node carries the name", func(t *testing.T) {
		f, a := approvedIngress3(t)
		f.nodes.set(ingressNodeID("ingress-3", 3, true), ingressNodeID("ingress-3", 30, true))
		f.clk.advance(infoTTL + time.Second)
		hits := a.hits()
		cur := f.reg.Current(ctx, true)
		v := f.view("ingress-3")
		if v.Approval != stApproved || !v.Active || !strings.Contains(v.Detail, "another") {
			t.Fatalf("%+v", v)
		}
		if a.hits() != hits+1 { // one reading: the pinned node's; the impostor is never asked
			t.Fatalf("readings %d -> %d", hits, a.hits())
		}
		if !SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets) || a.putCount() != 2 {
			t.Fatalf("pushes %d", a.putCount())
		}
	})
	t.Run("a pending node is not asked for its parameters while the name is ambiguous", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNodeID("ingress-3", 3, true), ingressNodeID("ingress-3", 30, true))
		if v := f.view("ingress-3"); v.Approval != stInvalid {
			t.Fatalf("%+v", v)
		}
		if a.hits() != 0 {
			t.Fatal("a token was sent to an unapproved duplicate")
		}
	})
	t.Run("a node that only reports the name as its hostname is ignored", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		impostor := ingressNodeID("x", 9, true)
		impostor.Name = "ingress-3" // client-reported
		impostor.GivenName = "laptop-9"
		f.nodes.set(impostor)
		cur := f.reg.Current(ctx, true)
		for _, v := range cur.Views {
			if v.Name == "ingress-3" {
				t.Fatalf("the hostname alone made a node an ingress: %+v", v)
			}
		}
		if a.hits() != 0 {
			t.Fatal("a token was sent to a node that only reports the name")
		}
	})
	t.Run("a configured ingress found through headscale", func(t *testing.T) {
		nodes := &nodeList{}
		reg := configuredByHeadscale(t, nodes)
		resolve := reg.resolver("ingress-2")
		legit := renamedNode("ingress-2", 2, "100.64.0.2")
		clone := renamedNode("ingress-2", 20, "100.64.0.20")
		nodes.set(legit, clone)
		if u, err := resolve(ctx); err == nil {
			t.Fatalf("an ambiguous name resolved to %s", u)
		}
		clone.GivenName = "laptop" // the clone only reports the hostname
		clone.Name = "ingress-2"
		nodes.set(clone, legit)
		if u, err := resolve(ctx); err != nil || !strings.Contains(u, "100.64.0.2:") {
			t.Fatalf("%q %v", u, err)
		}
	})
}

func configuredByHeadscale(t *testing.T, nodes *nodeList) *Registry {
	t.Helper()
	cfg := IngressConfig{LinkHost: "links.example.com", Ingresses: []Ingress{
		{Name: "ingress-2", Host: "ingress-2.example.com", PublicKey: "P2", ShortID: "b2", Port: 443, UUIDs: uuidsBase}}}.WithKey(testKey)
	reg, err := NewRegistry(RegistryConfig{Cfg: cfg, Src: nodes, Getenv: func(k string) string {
		return map[string]string{"AGENT_TOKEN_INGRESS_2": "t"}[k]
	}})
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

func TestAutoDerivedGivenName(t *testing.T) {
	// Every case is decided by headscale v0.29.3 (state.go isAutoDerivedGivenName over
	// dnsname.SanitizeHostname; tailscale.com util/dnsname is identical in v1.98.10 and v1.101.0-pre).
	cases := []struct {
		given, hostname string
		want            bool
	}{
		{"ingress-2", "ingress-2", true},
		{"ingress-2", "Ingress-2", true},
		{"ingress-2", "INGRESS-2", true},
		{"ingress-2", "ingress_2", true},
		{"ingress-2", "ingress 2", true},
		{"ingress-2", "ingress@2", true},
		{"ingress-2", "ingr!ess-2", true},
		{"ingress-2", "ingress-2.local", true},
		{"ingress-2", "ingress-2.lan", true},
		{"ingress-2", "ingress-2.localdomain", true},
		{"ingress2", "ingress+2", true},
		{"ingrss-2", "ingr\u00e9ss-2", true},
		{"ingress-2", "ingress-2" + strings.Repeat("!", 60) + "zz", true}, // cut to 63 bytes first
		{"ingress-2-1", "ingress-2", true},
		{"ingress-2-17", "ingress-2", true},
		{"ingress-2--3", "ingress-2", true}, // headscale parses the suffix with Atoi
		{"ingress-2-+5", "ingress-2", true},
		{"ingress-2", "", true}, // an empty hostname makes headscale name the node "node": refuse
		{"node", "", true},
		{"ingress-2", "my laptop", false},
		{"ingress-2-x", "ingress-2", false},
		{"ingress-22", "ingress-2", false},
		{"ingress-2", "box-2", false},
		{"ingress-2", "ingress-3", false},
		{"", "ingress-2", false},
	}
	for _, c := range cases {
		t.Run(c.given+"/"+c.hostname, func(t *testing.T) {
			if got := autoDerivedGivenName(c.given, c.hostname); got != c.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestConfiguredLookupNeedsAnAdministratorSetName(t *testing.T) {
	ctx := context.Background()
	nodes := &nodeList{}
	resolve := configuredByHeadscale(t, nodes).resolver("ingress-2")
	t.Run("a given name that follows the hostname is not admin-set", func(t *testing.T) {
		// what headscale does at registration, and again when the hostname changes
		n := ingressNodeID("ingress-2", 2, true)
		n.IPAddresses = []string{"100.64.0.2"}
		nodes.set(n)
		if u, err := resolve(ctx); err == nil {
			t.Fatalf("an auto-derived name resolved to %s", u)
		}
	})
	t.Run("a hostname change to a free name by an unrenamed node gets nothing", func(t *testing.T) {
		n := headscale.Node{ID: 66, Name: "Ingress-2", GivenName: "ingress-2", Online: true, Tags: []string{"tag:ingress"}, IPAddresses: []string{"100.64.0.66"}}
		nodes.set(n)
		if u, err := resolve(ctx); err == nil {
			t.Fatalf("resolved to %s", u)
		}
	})
	t.Run("an administrator-renamed node is found", func(t *testing.T) {
		nodes.set(renamedNode("ingress-2", 2, "100.64.0.2"))
		if u, err := resolve(ctx); err != nil || !strings.Contains(u, "100.64.0.2:") {
			t.Fatalf("%q %v", u, err)
		}
	})
	t.Run("a renamed node next to an unrenamed claimant: the claimant is ignored", func(t *testing.T) {
		claimant := headscale.Node{ID: 66, Name: "ingress-2", GivenName: "ingress-2", Online: true, Tags: []string{"tag:ingress"}, IPAddresses: []string{"100.64.0.66"}}
		nodes.set(claimant, renamedNode("ingress-2", 2, "100.64.0.2"))
		if u, err := resolve(ctx); err != nil || !strings.Contains(u, "100.64.0.2:") {
			t.Fatalf("%q %v", u, err)
		}
	})
}

// dialLog records every address a registry client dials and sends all of it to one fake server.
type dialLog struct {
	mu    sync.Mutex
	addrs []string
}

func (d *dialLog) dialed(ip string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range d.addrs {
		if strings.HasPrefix(a, ip+":") {
			return true
		}
	}
	return false
}

func recordingClient(a *infoAgent) (*http.Client, *dialLog) {
	log := &dialLog{}
	c := newHTTPClient(func(ctx context.Context, network, addr string) (net.Conn, error) {
		log.mu.Lock()
		log.addrs = append(log.addrs, addr)
		log.mu.Unlock()
		return (&net.Dialer{}).DialContext(ctx, network, a.srv.Listener.Addr().String())
	})
	return c, log
}

func pinnedFx(t *testing.T) (*regFx, *infoAgent, *dialLog) {
	t.Helper()
	a := newInfoAgent(t, "ingress-3")
	client, log := recordingClient(a)
	f := newRegFx(t, func(rc *RegistryConfig) { rc.HC = client })
	x := ingressNodeID("ingress-3", 3, true)
	x.IPAddresses = []string{"100.64.0.3"}
	f.nodes.set(x)
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)
	if err := f.approve("ingress-3"); err != nil {
		t.Fatal(err)
	}
	return f, a, log
}

func TestApprovedNodeAbsentSendsNoTokenToAnotherNode(t *testing.T) {
	ctx := context.Background()
	f, a, log := pinnedFx(t)
	hits := a.hits()
	y := ingressNodeID("ingress-3", 30, true)
	y.IPAddresses = []string{"100.64.0.30"}
	f.nodes.set(y) // the approved node is gone, another one carries the name
	f.clk.advance(infoTTL + time.Second)
	cur := f.reg.Current(ctx, true)
	v := f.view("ingress-3")
	if v.Active || v.Approval != stInvalid || !strings.Contains(v.Detail, "approved node is not present") {
		t.Fatalf("%+v", v)
	}
	if log.dialed("100.64.0.30") || a.hits() != hits {
		t.Fatal("the token was sent to a node the approval is not pinned to")
	}
	if len(cur.SyncTargets) != 1 {
		t.Fatalf("%d targets", len(cur.SyncTargets))
	}
}

func TestPinnedNodeWhoseNameCollidedIsStillUsedById(t *testing.T) {
	ctx := context.Background()
	f, a, log := pinnedFx(t)
	// the real node re-registered under "ingress-3-1" (headscale's collision suffix) while another
	// node took the name "ingress-3"
	x := ingressNodeID("ingress-3-1", 3, true)
	x.Name = "ingress-3"
	x.IPAddresses = []string{"100.64.0.3"}
	y := ingressNodeID("ingress-3", 30, true)
	y.IPAddresses = []string{"100.64.0.30"}
	f.nodes.set(x, y)
	f.clk.advance(infoTTL + time.Second)
	cur := f.reg.Current(ctx, true)
	v := f.view("ingress-3")
	if v.Approval != stApproved || !v.Active || v.Online == nil || !*v.Online {
		t.Fatalf("%+v", v)
	}
	if !SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets) || a.putCount() != 1 {
		t.Fatalf("the pinned node was not synced: %d", a.putCount())
	}
	if log.dialed("100.64.0.30") {
		t.Fatal("the node that only carries the name was contacted")
	}
}

func TestOnlineFlagFollowsThePinnedNode(t *testing.T) {
	f, _, _ := pinnedFx(t)
	x := ingressNodeID("ingress-3-1", 3, true)
	x.IPAddresses = []string{"100.64.0.3"}
	y := ingressNodeID("ingress-3", 30, false)
	y.IPAddresses = []string{"100.64.0.30"}
	f.nodes.set(x, y)
	f.clk.advance(infoTTL + time.Second)
	if v := f.view("ingress-3"); v.Online == nil || !*v.Online {
		t.Fatalf("%+v", v)
	}
}

func TestApproveBindsTheNodeTheOperatorSaw(t *testing.T) {
	ctx := context.Background()
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNodeID("ingress-3", 3, true))
	v := f.view("ingress-3")
	if v.NodeID != 3 || v.Address != "127.0.0.1" || v.Hostname != "ingress-3" || v.GivenName != "ingress-3" {
		t.Fatalf("the row must say which node it is: %+v", v)
	}
	var ae *ApprovalError
	t.Run("another node id", func(t *testing.T) {
		if err := f.reg.Approve(ctx, "ingress-3", v.Fingerprint, 4); !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatalf("%v", err)
		}
		if err := f.reg.Approve(ctx, "ingress-3", v.Fingerprint, 0); !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatalf("%v", err)
		}
		if rows, _ := f.store.ApprovalRows(); len(rows) != 0 {
			t.Fatal("approved on a node mismatch")
		}
	})
	t.Run("the node was replaced between page load and click", func(t *testing.T) {
		f.nodes.set(ingressNodeID("ingress-3", 30, true)) // an impostor republishing the same values
		if err := f.reg.Approve(ctx, "ingress-3", v.Fingerprint, 3); err == nil {
			t.Fatal("approved a node the operator did not review")
		}
		f.nodes.set(ingressNodeID("ingress-3", 3, true))
	})
	t.Run("the right node id pins", func(t *testing.T) {
		f.clk.advance(infoTTL + time.Second)
		if err := f.reg.Approve(ctx, "ingress-3", v.Fingerprint, 3); err != nil {
			t.Fatal(err)
		}
		if rows, _ := f.store.ApprovalRows(); rows["ingress-3"].NodeID != 3 {
			t.Fatalf("%v", rows)
		}
	})
}

func TestMachineKeyPinsTheNode(t *testing.T) {
	ctx := context.Background()
	t.Run("the same id with another machine key is neither asked nor synced", func(t *testing.T) {
		f, a, log := pinnedFx(t)
		hits := a.hits()
		x := ingressNodeID("ingress-3", 3, true) // the id was recycled by another machine
		x.MachineKey = "mkey:attacker"
		x.IPAddresses = []string{"100.64.0.77"}
		f.nodes.set(x)
		f.clk.advance(infoTTL + time.Second)
		cur := f.reg.Current(ctx, true)
		v := f.view("ingress-3")
		if v.Active || v.Approval != stChanged {
			t.Fatalf("%+v", v)
		}
		if a.hits() != hits || log.dialed("100.64.0.77") {
			t.Fatal("the token went to a node with another machine key")
		}
		if len(cur.SyncTargets) != 1 || a.putCount() != 0 {
			t.Fatalf("targets %d", len(cur.SyncTargets))
		}
		// not even as an unverified target after a failed reading
		SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
		if a.putCount() != 0 {
			t.Fatal("credentials were pushed to a node with another machine key")
		}
	})
	t.Run("the pinned agent is found by id and machine key", func(t *testing.T) {
		f, _, _ := pinnedFx(t)
		n := ingressNodeID("ingress-3", 3, true)
		n.MachineKey = "mkey:other"
		f.nodes.set(n)
		if _, err := f.reg.resolverFor("ingress-3", 3, mkey(3))(ctx); err == nil {
			t.Fatal("resolved a node with another machine key")
		}
		f.nodes.set(ingressNodeID("ingress-3", 3, true))
		if _, err := f.reg.resolverFor("ingress-3", 3, mkey(3))(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := f.reg.resolverFor("ingress-3", 3, "")(ctx); err == nil {
			t.Fatal("an empty machine key pinned something")
		}
	})
	t.Run("an API that omits the key: refused", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		n := ingressNodeID("ingress-3", 3, true)
		n.MachineKey = ""
		f.nodes.set(n)
		v := f.view("ingress-3")
		var ae *ApprovalError
		if err := f.reg.Approve(ctx, "ingress-3", v.Fingerprint, v.NodeID); !errors.As(err, &ae) || ae.Status != 409 {
			t.Fatalf("approved a node without a machine key: %v", err)
		}
		// and a stored approval is not honoured when the key is no longer reported
		f.store.ApproveNode("ingress-3", v.Fingerprint, 3, mkey(3))
		f.clk.advance(infoTTL + time.Second)
		if v := f.view("ingress-3"); v.Active {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("an approval from before machine key pinning needs approving again", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNodeID("ingress-3", 3, true))
		f.store.ApproveNode("ingress-3", f.view("ingress-3").Fingerprint, 3, "")
		v := f.view("ingress-3")
		if v.Active || v.Approval != stChanged || !strings.Contains(v.Detail, "machine key") {
			t.Fatalf("%+v", v)
		}
		if err := f.approve("ingress-3"); err != nil {
			t.Fatal(err)
		}
		if v := f.view("ingress-3"); !v.Active {
			t.Fatalf("%+v", v)
		}
	})
	t.Run("the row shows a short prefix of the key", func(t *testing.T) {
		a := newInfoAgent(t, "ingress-3")
		usePort(t, a)
		f := newRegFx(t, nil)
		f.nodes.set(ingressNodeID("ingress-3", 3, true))
		v := f.view("ingress-3")
		if v.MachineKeyPref == "" || len(v.MachineKeyPref) > 8 || strings.Contains(fmt.Sprint(v), mkey(3)) {
			t.Fatalf("%+v", v)
		}
	})
}

func TestHostnameInTheRowIsPlainAndShort(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	n := ingressNodeID("ingress-3", 3, true)
	n.Name = "evil\x1b[31m\u202ehost\n" + strings.Repeat("x", 200)
	f.nodes.set(n)
	v := f.view("ingress-3")
	for _, r := range v.Hostname + v.GivenName {
		if r < 0x20 || r == 0x7f || r == 0x202e || r == 0x1b {
			t.Fatalf("control character in %q", v.Hostname)
		}
	}
	if len(v.Hostname) > 63 || !strings.HasPrefix(v.Hostname, "evil[31mhost") {
		t.Fatalf("%q", v.Hostname)
	}
}

func TestPinnedNodeIsNotAlsoACandidateUnderItsNewName(t *testing.T) {
	ctx := context.Background()
	f, a, _ := pinnedFx(t)
	f.env["AGENT_TOKEN_INGRESS_9"] = "token-for-ingress-9"
	x := ingressNodeID("ingress-9", 3, true) // renamed since the approval
	f.nodes.set(x)
	f.clk.advance(infoTTL + time.Second)
	cur := f.reg.Current(ctx, true)
	for _, v := range cur.Views {
		if v.Name == "ingress-9" {
			t.Fatalf("the pinned node became a second candidate: %+v", v)
		}
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range a.infoAuth {
		if strings.Contains(h, "ingress-9") {
			t.Fatal("the token of another ingress was sent to the pinned node")
		}
	}
}

func TestDiscoveredNodeOfAConfiguredBridgeIsOneRow(t *testing.T) {
	old := infoTimeout
	infoTimeout = 200 * time.Millisecond
	t.Cleanup(func() { infoTimeout = old })
	f := newRegFx(t, func(rc *RegistryConfig) {
		rc.Cfg.Ingresses[0].AgentURL = "http://100.64.0.15:9000"
	})
	f.nodes.set(renamedNode("rus2", 7, "100.64.0.15"), renamedNode("other", 8, "100.64.0.99"))
	f.store.CreateDevice("n", "o", "sing-box", adminFuture)

	cur := f.reg.Current(context.Background(), true)
	var names []string
	for _, v := range cur.Views {
		names = append(names, v.Name)
	}
	if got := strings.Join(names, ","); got != "ingress-1,other" {
		t.Fatalf("the node of ingress-1 must be one row with it, another node stays: %s", got)
	}
	v := cur.Views[0]
	if len(v.Aliases) != 1 || v.Aliases[0] != "rus2" || v.Online == nil || !*v.Online {
		t.Fatalf("%+v", v)
	}
	if len(cur.Pending) != 1 || cur.Pending[0] != "other" {
		t.Fatalf("pending %v", cur.Pending)
	}
}
