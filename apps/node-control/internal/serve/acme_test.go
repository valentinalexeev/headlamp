package serve

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeIssuer is a public ACME CA: it signs whatever names it is asked for with a test root.
type fakeIssuer struct {
	mu    sync.Mutex
	root  *x509.Certificate
	key   *ecdsa.PrivateKey
	ttl   time.Duration
	calls [][]string
	fail  error
}

func newFakeIssuer(t *testing.T) *fakeIssuer {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake ACME root"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(48 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := x509.ParseCertificate(der)
	return &fakeIssuer{root: root, key: k, ttl: 9 * time.Hour}
}

func (f *fakeIssuer) Obtain(_ context.Context, names []string) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, names)
	if f.fail != nil {
		return nil, nil, f.fail
	}
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(f.ttl), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, f.root, &k.PublicKey, f.key)
	if err != nil {
		return nil, nil, err
	}
	kd, _ := x509.MarshalECPrivateKey(k)
	chain := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.root.Raw})...)
	return chain, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}), nil
}

func (f *fakeIssuer) n() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

var acmeEnv = map[string]string{"SERVE_CERT_SOURCE": "acme", "SERVE_ACME_EMAIL": "ops@example.com", "SERVE_ACME_DNS": "rfc2136"}

func TestACMENames(t *testing.T) {
	got := acmeNames(zone, []Service{{Name: "grafana." + zone}, {Name: "*.apps." + zone}, {Name: "a.b." + zone}, {Name: "x.apps." + zone}})
	want := []string{"*." + zone, "*.apps." + zone, "*.b." + zone}
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	for _, n := range []string{"grafana." + zone, "*.apps." + zone, "a.b." + zone} {
		if !covers(got, n) {
			t.Errorf("%s not covered", n)
		}
	}
	if covers([]string{"*." + zone}, "*.apps."+zone) || covers([]string{"*." + zone}, "a.b."+zone) || covers([]string{"*." + zone}, zone) {
		t.Fatal("a wildcard covers exactly one label")
	}
}

func TestACMEConfigRefusals(t *testing.T) {
	for name, mod := range map[string]map[string]string{
		"bad source":  {"SERVE_CERT_SOURCE": "letsencrypt"},
		"no email":    {"SERVE_ACME_EMAIL": ""},
		"bad dns":     {"SERVE_ACME_DNS": "manual"},
		"http dir":    {"SERVE_ACME_DIRECTORY": "http://ca.example.com/dir"},
		"with own CA": {"SERVE_CA_CERT": "x", "SERVE_CA_KEY": "y"},
		"no statekey": {"NODE_CONTROL_STATE_KEY": ""},
	} {
		env := map[string]string{}
		for k, v := range acmeEnv {
			env[k] = v
		}
		for k, v := range mod {
			env[k] = v
		}
		if err := tryRig(t, env); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The default stays the CA, with no ACME settings needed (#334: no change for existing sites).
func TestDefaultSourceIsCA(t *testing.T) {
	r := newRig(t, nil)
	if r.mod.acme != nil || r.mod.ca == nil || r.mod.cfg.source != "ca" {
		t.Fatal("the default source must be the CA")
	}
}

func acmeRig(t *testing.T, iss *fakeIssuer, extra map[string]string) *rig {
	env := map[string]string{}
	for k, v := range acmeEnv {
		env[k] = v
	}
	for k, v := range extra {
		env[k] = v
	}
	return newRigOpts(t, env, WithIssuer(iss))
}

func certCall(r *rig, ip string, api int) (int, certResp, string) {
	c, b := r.do("POST", "/serve/v1/cert", ip, map[string]any{"csr": csrPEM(r.t), "api": api})
	var out certResp
	_ = json.Unmarshal(b, &out)
	return c, out, string(b)
}

func TestACMEHandsTheSharedCertificateToProxies(t *testing.T) {
	iss := newFakeIssuer(t)
	r := acmeRig(t, iss, nil)
	svc := r.create("grafana", "mon")
	r.hs.attach(ipProxyA, svc.Service.Keys[0].ID, true)
	wild := r.create("*.apps", "k8s")
	r.hs.attach(ipProxyB, wild.Service.Keys[0].ID, true)

	// before the first issuance the proxy is told to come back
	if c, _, _ := certCall(r, ipProxyA, 2); c != http.StatusServiceUnavailable {
		t.Fatalf("no certificate yet: %d", c)
	}
	want, _ := r.mod.acmeWant()
	if err := r.mod.acme.ensure(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(iss.calls[0], []string{"*." + zone, "*.apps." + zone}) {
		t.Fatalf("ordered %v", iss.calls[0])
	}
	// an old proxy cannot use a shared key: it is told to upgrade, and gets no key
	if c, out, _ := certCall(r, ipProxyA, 0); c != http.StatusUpgradeRequired || out.KeyPEM != "" {
		t.Fatalf("old proxy: %d", c)
	}
	roots := x509.NewCertPool()
	roots.AddCert(iss.root)
	for ip, host := range map[string]string{ipProxyA: "grafana." + zone, ipProxyB: "x.apps." + zone} {
		c, out, body := certCall(r, ip, 2)
		if c != http.StatusOK || out.KeyPEM == "" {
			t.Fatalf("%s: %d %s", ip, c, body)
		}
		pair, err := tls.X509KeyPair([]byte(out.CertPEM+out.ChainPEM), []byte(out.KeyPEM))
		if err != nil {
			t.Fatal(err)
		}
		leaf, _ := x509.ParseCertificate(pair.Certificate[0])
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Roots: roots}); err != nil {
			t.Fatalf("%s: chain to the ACME root: %v", host, err)
		}
	}
	// a stray tag:serve node gets nothing
	if c, _, _ := certCall(r, ipStray, 2); c != http.StatusForbidden {
		t.Fatalf("stray: %d", c)
	}
	// nothing to renew yet; the same names make no new order
	_ = r.mod.acme.ensure(context.Background(), want)
	if iss.n() != 1 {
		t.Fatalf("orders %d", iss.n())
	}
	// the state shows the source and the names, and /serve/ca.crt has nothing to give
	_, b := r.do("GET", "/serve/state", ipOwner, nil)
	var st stateResp
	_ = json.Unmarshal(b, &st)
	if st.CertSource != "acme" || len(st.ACMENames) != 2 || st.CAFingerprint != "" {
		t.Fatalf("state %+v", st)
	}
	if c, _ := r.do("GET", "/serve/ca.crt", ipOwner, nil); c != http.StatusNotFound {
		t.Fatalf("ca.crt: %d", c)
	}
}

func TestACMERenewalAndFailureKeepsOld(t *testing.T) {
	iss := newFakeIssuer(t)
	r := acmeRig(t, iss, nil)
	k := r.mod.acme
	now := time.Now()
	k.now = func() time.Time { return now }
	want := []string{"*." + zone}
	if err := k.ensure(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	first := k.cur.load()
	// a third into its life: not due
	now = first.from.Add(first.until.Sub(first.from) / 3)
	_ = k.ensure(context.Background(), want)
	if iss.n() != 1 {
		t.Fatal("renewed too early")
	}
	// past two thirds, but the CA fails: the old certificate stays and the error shows
	now = first.from.Add(first.until.Sub(first.from)*2/3 + time.Minute)
	iss.fail = errors.New("acme: rate limited")
	if err := k.ensure(context.Background(), want); err == nil || k.cur.load() != first || !strings.Contains(k.lastErr.load(), "rate limited") {
		t.Fatal("a failed renewal must keep the old certificate")
	}
	iss.fail = nil
	if err := k.ensure(context.Background(), want); err != nil || k.cur.load() == first || k.lastErr.load() != "" {
		t.Fatal("renewal")
	}
}

// The certificate and its sealed key survive a restart: no new order, and the key is not in the store in clear.
func TestACMEStateSurvivesRestart(t *testing.T) {
	iss := newFakeIssuer(t)
	env := map[string]string{"SERVE_DB": t.TempDir() + "/serve.db", "NODE_CONTROL_STATE_KEY": stateKey()}
	r := acmeRig(t, iss, env)
	want := []string{"*." + zone}
	if err := r.mod.acme.ensure(context.Background(), want); err != nil {
		t.Fatal(err)
	}
	sealed, err := r.mod.st.GetSecret(acmeKeyKey)
	if err != nil || strings.Contains(string(sealed), "PRIVATE KEY") {
		t.Fatalf("the key must be sealed: %v", err)
	}
	cert := r.mod.acme.cur.load().chainPEM
	_ = r.mod.Stop(context.Background())

	r2 := acmeRig(t, iss, env)
	got := r2.mod.acme.cur.load()
	if got == nil || string(got.chainPEM) != string(cert) || !slices.Equal(got.names, want) {
		t.Fatal("the certificate was not loaded")
	}
	_ = r2.mod.acme.ensure(context.Background(), want)
	if iss.n() != 1 {
		t.Fatal("a restart must not order again")
	}
	// another state key cannot open it
	env["NODE_CONTROL_STATE_KEY"] = stateKey()
	if err := tryRigOpts(t, mergeEnv(acmeEnv, env), WithIssuer(iss)); err == nil {
		t.Fatal("a wrong state key must refuse to start")
	}
}

// A new service deeper in the zone adds a wildcard: the keeper orders again with the new names.
func TestACMENewDeepServiceOrdersAgain(t *testing.T) {
	iss := newFakeIssuer(t)
	r := acmeRig(t, iss, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go r.mod.acme.run(ctx, r.mod.acmeWant, time.Hour)
	waitFor(t, func() bool { return iss.n() == 1 })
	r.create("*.apps", "k8s")
	waitFor(t, func() bool { return iss.n() == 2 })
	if !slices.Equal(iss.calls[1], []string{"*." + zone, "*.apps." + zone}) {
		t.Fatalf("second order %v", iss.calls[1])
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func TestSplitLeaf(t *testing.T) {
	iss := newFakeIssuer(t)
	chain, _, _ := iss.Obtain(context.Background(), []string{"a." + zone})
	leaf, rest := splitLeaf(chain)
	if strings.Count(string(leaf), "BEGIN CERTIFICATE") != 1 || strings.Count(string(rest), "BEGIN CERTIFICATE") != 1 {
		t.Fatalf("leaf %q rest %q", leaf, rest)
	}
}
