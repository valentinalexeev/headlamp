package serveclient

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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeNC signs CSRs with a local CA, as node-control's serve module does: it certifies the name of the caller's
// service (here, name), never what the CSR asks for. lie, when set, certifies another name while still returning
// name, to test the client's own check.
type fakeNC struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
	caPEM  []byte
	ttl    time.Duration
	refuse int
	name   string
	lie    string
	path   string
}

func newFakeNC(t *testing.T) *fakeNC {
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &k.PublicKey, k)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	return &fakeNC{caCert: c, caKey: k, caPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), ttl: 3 * time.Hour,
		name: "grafana.svc.example.com"}
}

func (f *fakeNC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.path = r.URL.Path
	if f.refuse != 0 {
		http.Error(w, "this node did not join with the key of a service", f.refuse)
		return
	}
	var req map[string]any
	json.NewDecoder(r.Body).Decode(&req)
	if len(req) != 1 {
		http.Error(w, "the sidecar must send only a csr", 400)
		return
	}
	csrText, _ := req["csr"].(string)
	b, _ := pem.Decode([]byte(csrText))
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil || csr.CheckSignature() != nil {
		http.Error(w, "bad csr", 422)
		return
	}
	names := []string{f.name}
	if f.lie != "" {
		names = []string{f.lie}
	}
	now := time.Now()
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(now.UnixNano()), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(f.ttl), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, f.caCert, csr.PublicKey, f.caKey)
	json.NewEncoder(w).Encode(Response{Name: f.name, CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		ChainPEM: string(f.caPEM), NotAfter: tmpl.NotAfter})
}

func client(srv *httptest.Server) *Client {
	return &Client{URL: srv.URL, HTTP: srv.Client()}
}

// The certificate node-control returns serves TLS that a client trusting the serve CA accepts for the name.
func TestRegisterThenServeTLS(t *testing.T) {
	nc := newFakeNC(t)
	srv := httptest.NewServer(nc)
	defer srv.Close()
	c := client(srv)
	if err := c.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	if nc.path != "/serve/v1/cert" || c.Name() != "grafana.svc.example.com" {
		t.Fatalf("path %q name %q", nc.path, c.Name())
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", c.TLSConfig())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				// The handshake runs on first I/O; a client that rejects the certificate makes this write fail.
				conn.SetDeadline(time.Now().Add(5 * time.Second))
				conn.Write([]byte("hi"))
				conn.Close()
			}()
		}
	}()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(nc.caPEM)
	d := &net.Dialer{Timeout: 5 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "grafana.svc.example.com"})
	if err != nil {
		t.Fatalf("a client trusting the serve CA must accept the name: %v", err)
	}
	b := make([]byte, 2)
	conn.Read(b)
	conn.Close()
	if string(b) != "hi" {
		t.Fatalf("read %q", b)
	}
	// Another name fails verification.
	if c2, err := tls.DialWithDialer(d, "tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "bank.example.com"}); err == nil {
		c2.Close()
		t.Fatal("verified a name the certificate does not cover")
	}
}

func TestRenewKeepsTheKeyAndSwapsTheCertificate(t *testing.T) {
	nc := newFakeNC(t)
	srv := httptest.NewServer(nc)
	defer srv.Close()
	c := client(srv)
	c.Register(context.Background())
	first, key := c.cert.Load(), c.key
	c.Register(context.Background())
	if c.key != key {
		t.Fatal("a renewal must reuse the process's key")
	}
	if c.cert.Load() == first {
		t.Fatal("the new certificate was not installed")
	}
	cert, _ := c.TLSConfig().GetCertificate(nil)
	if cert != c.cert.Load() {
		t.Fatal("TLSConfig does not serve the current certificate")
	}
}

func TestRenewAtTwoThirds(t *testing.T) {
	nc := newFakeNC(t)
	srv := httptest.NewServer(nc)
	defer srv.Close()
	c := client(srv)
	if !c.RenewAt().IsZero() {
		t.Fatal("no certificate yet: RenewAt must be zero")
	}
	c.Register(context.Background())
	leaf := c.cert.Load().Leaf
	want := leaf.NotBefore.Add(leaf.NotAfter.Sub(leaf.NotBefore) * 2 / 3)
	if d := c.RenewAt().Sub(want); d > time.Second || d < -time.Second {
		t.Fatalf("RenewAt %v, want about %v", c.RenewAt(), want)
	}
}

func TestRefusalAndWrongNames(t *testing.T) {
	nc := newFakeNC(t)
	srv := httptest.NewServer(nc)
	defer srv.Close()
	nc.refuse = 409
	c := client(srv)
	var api *APIError
	if err := c.Register(context.Background()); !errors.As(err, &api) || api.Status != 409 || !strings.Contains(api.Msg, "key of a service") {
		t.Fatalf("refusal: %v", err)
	}
	nc.refuse = 0
	nc.lie = "other.svc.example.com"
	if err := c.Register(context.Background()); err == nil || !strings.Contains(err.Error(), "does not cover") {
		t.Fatalf("a certificate for other names must be refused: %v", err)
	}
	if c.cert.Load() != nil {
		t.Fatal("a refused certificate was installed")
	}
	if _, err := c.TLSConfig().GetCertificate(nil); err == nil {
		t.Fatal("TLS without a certificate must fail the handshake")
	}
}

// Keep reports readiness and stops with its context.
func TestKeepReportsReady(t *testing.T) {
	nc := newFakeNC(t)
	srv := httptest.NewServer(nc)
	defer srv.Close()
	c := client(srv)
	ctx, cancel := context.WithCancel(context.Background())
	got := make(chan bool, 4)
	done := make(chan struct{})
	go func() { c.Keep(ctx, func(b bool) { got <- b }, func(string, ...any) {}); close(done) }()
	if !<-got {
		t.Fatal("not ready after a good registration")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Keep did not stop")
	}
}

// A wildcard service: the certificate covers one label under the suffix, and the client accepts it.
func TestWildcardService(t *testing.T) {
	nc := newFakeNC(t)
	nc.name = "*.apps.svc.example.com"
	srv := httptest.NewServer(nc)
	defer srv.Close()
	c := client(srv)
	if err := c.Register(context.Background()); err != nil {
		t.Fatalf("wildcard: %v", err)
	}
	if err := c.cert.Load().Leaf.VerifyHostname("grafana.apps.svc.example.com"); err != nil {
		t.Fatal(err)
	}
	// a returned wildcard name with a certificate for something else is refused
	nc.lie = "x.svc.example.com"
	c2 := client(srv)
	if err := c2.Register(context.Background()); err == nil {
		t.Fatal("a certificate that does not cover the wildcard must be refused")
	}
}
