package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"slices"
	"strings"
	"testing"
	"time"
)

func csrFor(t *testing.T, cn string, sans ...string) []byte {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: cn}, DNSNames: sans}, k)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

func leaf(t *testing.T, p []byte) *x509.Certificate {
	t.Helper()
	b, _ := pem.Decode(p)
	c, err := x509.ParseCertificate(b.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestSelfSignedCASignsVerifiableLeaf(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	ca, certPEM, keyPEM, err := NewSelfSignedCA("headlamp serve test", now)
	if err != nil {
		t.Fatal(err)
	}
	ca.now = func() time.Time { return now }
	if !strings.Contains(string(keyPEM), "PRIVATE KEY") || ca.Issued != "self-signed" {
		t.Fatal("key not returned for encrypted storage")
	}
	out, exp, err := ca.SignCSR(csrFor(t, "x"), []string{"grafana.cluster.vpn.example.com"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.Equal(now.Add(DefaultLeafTTL)) {
		t.Fatalf("expiry %v", exp)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	if _, err := leaf(t, out).Verify(x509.VerifyOptions{DNSName: "grafana.cluster.vpn.example.com", Roots: roots, CurrentTime: now,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		t.Fatalf("leaf does not verify against the CA: %v", err)
	}
}

// A node cannot ask for more: the CSR's own names and subject are ignored.
func TestSignCSRUsesOnlyApprovedNames(t *testing.T) {
	ca, _, _, _ := NewSelfSignedCA("t", time.Now())
	out, _, err := ca.SignCSR(csrFor(t, "bank.example.com", "bank.example.com", "*.vpn.example.com"), []string{"a.cluster.vpn.example.com"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := leaf(t, out)
	if !slices.Equal(c.DNSNames, []string{"a.cluster.vpn.example.com"}) || c.Subject.CommonName != "a.cluster.vpn.example.com" {
		t.Fatalf("names %v cn %q", c.DNSNames, c.Subject.CommonName)
	}
	if c.IsCA || c.KeyUsage&x509.KeyUsageCertSign != 0 {
		t.Fatal("a leaf must not be a CA")
	}
	if !slices.Equal(c.ExtKeyUsage, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}) {
		t.Fatalf("ext key usage %v", c.ExtKeyUsage)
	}
}

func TestSignCSRCapsTTL(t *testing.T) {
	now := time.Now()
	ca, _, _, _ := NewSelfSignedCA("t", now)
	ca.now = func() time.Time { return now }
	_, exp, err := ca.SignCSR(csrFor(t, "x"), []string{"a.b.c"}, 10*365*24*time.Hour)
	if err != nil || !exp.Equal(now.Add(MaxLeafTTL)) {
		t.Fatalf("ttl not capped: %v %v", exp, err)
	}
}

func TestSignCSRRefuses(t *testing.T) {
	ca, _, _, _ := NewSelfSignedCA("t", time.Now())
	good := csrFor(t, "x")
	tampered := []byte(strings.Replace(string(good), "A", "B", 1))
	for name, in := range map[string][]byte{
		"not pem":  []byte("hello"),
		"cert pem": ca.CertPEM(),
		"tampered": tampered,
	} {
		if _, _, err := ca.SignCSR(in, []string{"a.b.c"}, 0); err == nil {
			t.Errorf("%s: signed", name)
		}
	}
	if _, _, err := ca.SignCSR(good, nil, 0); err == nil {
		t.Error("signed with no names")
	}
}

// An operator CA: an intermediate signed by the operator's root. Leaves verify against the root with the chain.
func TestOperatorIntermediate(t *testing.T) {
	now := time.Now()
	// The operator's own root may sign one intermediate (path length 1). The self-signed serve CA has path length 0
	// on purpose: it signs leaves only.
	rk, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	rootTmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "operator root"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, MaxPathLen: 1}
	rootDER, _ := x509.CreateCertificate(rand.Reader, rootTmpl, rootTmpl, &rk.PublicKey, rk)
	rootCert, _ := x509.ParseCertificate(rootDER)
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	root := &CA{cert: rootCert, key: rk}
	ik, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: serial(), Subject: pkix.Name{CommonName: "serve intermediate"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0), IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, MaxPathLenZero: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, root.cert, &ik.PublicKey, root.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalPKCS8PrivateKey(ik)
	interPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	ca, err := LoadCA(append(interPEM, rootPEM...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd}), "operator")
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := ca.SignCSR(csrFor(t, "x"), []string{"svc.lab.vpn.example.com"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	roots, inters := x509.NewCertPool(), x509.NewCertPool()
	roots.AppendCertsFromPEM(rootPEM)
	inters.AppendCertsFromPEM(ca.ChainPEM())
	if _, err := leaf(t, out).Verify(x509.VerifyOptions{DNSName: "svc.lab.vpn.example.com", Roots: roots, Intermediates: inters}); err != nil {
		t.Fatalf("leaf does not chain to the operator root: %v", err)
	}
	// The leaf expiry is capped by the intermediate's.
	_, exp, _ := ca.SignCSR(csrFor(t, "x"), []string{"a.b.c"}, MaxLeafTTL)
	if exp.After(ca.cert.NotAfter) {
		t.Fatal("leaf outlives its CA")
	}
}

func TestLoadCARefuses(t *testing.T) {
	ca, certPEM, keyPEM, _ := NewSelfSignedCA("t", time.Now())
	if !ca.cert.MaxPathLenZero {
		t.Fatal("the self-signed serve CA must not be able to sign other CAs")
	}
	_, _, otherKey, _ := NewSelfSignedCA("u", time.Now())
	if _, err := LoadCA(certPEM, otherKey, "operator"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Errorf("mismatched key accepted: %v", err)
	}
	out, _, _ := ca.SignCSR(csrFor(t, "x"), []string{"a.b.c"}, 0)
	if _, err := LoadCA(out, keyPEM, "operator"); err == nil || !strings.Contains(err.Error(), "not a CA") {
		t.Errorf("a leaf accepted as CA: %v", err)
	}
	if _, err := LoadCA(nil, keyPEM, "operator"); err == nil {
		t.Error("empty cert accepted")
	}
}
