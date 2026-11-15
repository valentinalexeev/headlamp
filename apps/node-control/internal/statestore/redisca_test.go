package statestore

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

// testCA makes a CA and a server certificate for 127.0.0.1 signed by it.
func testCA(t *testing.T) (caPEM []byte, server tls.Certificate) {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test redis CA"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "redis"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// REDIS_CA_FILE (#361): a rediss:// server with a private CA is refused with the system roots and
// accepted with its CA file.
func TestRedisCAFile(t *testing.T) {
	t.Cleanup(func() { rootCAs = nil })
	caPEM, cert := testCA(t)
	mr, err := miniredis.RunTLS(&tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mr.Close)
	url := "rediss://" + mr.Addr()

	if err := SetCAFile(""); err != nil || rootCAs != nil {
		t.Fatalf("empty path keeps the system roots: %v", err)
	}
	if _, _, err := OpenClient(url); err == nil {
		t.Fatal("a private CA must be refused with the system roots")
	}

	dir := t.TempDir()
	caFile := filepath.Join(dir, "server-ca.pem")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SetCAFile(caFile); err != nil {
		t.Fatal(err)
	}
	rdb, _, err := OpenClient(url)
	if err != nil {
		t.Fatalf("with REDIS_CA_FILE: %v", err)
	}
	_ = rdb.Close()

	junk := filepath.Join(dir, "junk.pem")
	_ = os.WriteFile(junk, []byte("not a certificate"), 0o600)
	if err := SetCAFile(junk); err == nil || !strings.Contains(err.Error(), "no PEM certificate") {
		t.Fatalf("junk: %v", err)
	}
	if err := SetCAFile(filepath.Join(dir, "missing.pem")); err == nil || !strings.Contains(err.Error(), "REDIS_CA_FILE") {
		t.Fatalf("missing: %v", err)
	}
}
