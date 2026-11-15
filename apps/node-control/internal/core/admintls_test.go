package core

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// tlsLocal is Local with a :443 listener for the console on HTTPS (#366).
type tlsLocal struct {
	*Local
	mu  sync.Mutex
	tls net.Listener
	got chan struct{}
}

func (t *tlsLocal) ListenAdminTLS() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.tls = ln
	t.mu.Unlock()
	close(t.got)
	return ln, nil
}

func selfSigned(t *testing.T, name string) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(c)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

// With WithAdminTLS, the same admin handler answers on the TLS listener, behind the same Host guard.
func TestAdminTLSListener(t *testing.T) {
	const name = "console.svc.example.test"
	cert, pool := selfSigned(t, name)
	tn := &tlsLocal{Local: NewLocal("127.0.0.1"), got: make(chan struct{})}
	mod := &stubModule{name: "s", register: func(r *Routes) {
		r.Admin.HandleFunc("GET /hello", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "hi") })
	}}
	env := MapEnv(map[string]string{"ENROLL_ALLOWED_HOSTS": name})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return &cert, nil }}
	go func() { done <- Run(ctx, []Module{mod}, env, tn, WithAdminTLS(cfg)) }()
	defer func() { cancel(); <-done }()
	select {
	case <-tn.got:
	case <-time.After(5 * time.Second):
		t.Fatal("no TLS listener")
	}
	tn.mu.Lock()
	addr := tn.tls.Addr().String()
	tn.mu.Unlock()
	c := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: name},
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
		}}}
	resp, err := c.Get("https://" + name + "/hello")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || strings.TrimSpace(string(b)) != "hi" {
		t.Fatalf("over TLS: %d %q", resp.StatusCode, b)
	}
	req, _ := http.NewRequest(http.MethodGet, "https://"+name+"/hello", nil)
	req.Host = "evil.example"
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("the Host guard applies on TLS too: %d", resp.StatusCode)
	}
}

// A tailnet without a :443 listener cannot run the console on HTTPS.
func TestAdminTLSNeedsListener(t *testing.T) {
	err := Run(context.Background(), nil, MapEnv(nil), NewLocal("127.0.0.1"), WithAdminTLS(&tls.Config{}))
	if err == nil || !strings.Contains(err.Error(), ":443") {
		t.Fatalf("%v", err)
	}
}
