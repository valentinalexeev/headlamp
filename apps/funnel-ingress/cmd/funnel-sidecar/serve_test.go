package main

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
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/serveclient"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/sidecar"
)

// A signer standing in for node-control: certifies the requested names with a test CA.
func fakeNodeControl(t *testing.T) (*httptest.Server, []byte) {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ct := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour),
		NotAfter: time.Now().Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, _ := x509.CreateCertificate(rand.Reader, ct, ct, &k.PublicKey, k)
	ca, _ := x509.ParseCertificate(der)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			CSR string `json:"csr"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		b, _ := pem.Decode([]byte(req.CSR))
		csr, _ := x509.ParseCertificateRequest(b.Bytes)
		// node-control names the certificate from the caller's service key: here a wildcard service
		names := []string{"*.apps.svc.example.com"}
		lt := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: names, NotBefore: time.Now().Add(-time.Minute),
			NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
		ld, _ := x509.CreateCertificate(rand.Reader, lt, ca, csr.PublicKey, k)
		json.NewEncoder(w).Encode(serveclient.Response{Name: names[0], CertPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ld})),
			ChainPEM: string(caPEM), NotAfter: lt.NotAfter})
	}))
	return srv, caPEM
}

// Serve mode end to end: a client that trusts the serve CA reaches the pod's plain backend through the sidecar's
// TLS listener, under a name of the wildcard service (the ingress case).
func TestServeModeTLSToBackend(t *testing.T) {
	nc, caPEM := fakeNodeControl(t)
	defer nc.Close()
	backend, _ := net.Listen("tcp", "127.0.0.1:0")
	defer backend.Close()
	go func() {
		for {
			c, err := backend.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("hello from the pod"))
			c.Close()
		}
	}()
	// The "tailnet" listener: a local port the test dials.
	tailnet, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := func(string, string) (net.Listener, error) { return tailnet, nil }
	svcs := []sidecar.Service{{Name: "grafana", TailnetPort: 443, Target: backend.Addr().String()}}
	c := &serveclient.Client{URL: nc.URL, HTTP: nc.Client()}
	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan bool, 4)
	done := make(chan error, 1)
	go func() {
		done <- serveWith(ctx, svcs, listen, c, func(b bool) { readyCh <- b }, (&net.Dialer{}).DialContext, func(string, ...any) {})
	}()
	if !<-readyCh {
		t.Fatal("not ready")
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", tailnet.Addr().String(),
		&tls.Config{RootCAs: roots, ServerName: "grafana.apps.svc.example.com"})
	if err != nil {
		t.Fatalf("TLS to the sidecar: %v", err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	conn.Close()
	if string(b) != "hello from the pod" {
		t.Fatalf("got %q", b)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("serve mode did not stop")
	}
}
