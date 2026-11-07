package serve

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"time"
)

// DefaultLeafTTL is how long a signed certificate lasts unless the caller asks for less. Short, so a node that leaves
// the namespace loses its certificate soon even without revocation.
const DefaultLeafTTL = 30 * 24 * time.Hour

// MaxLeafTTL caps what a caller may ask for.
const MaxLeafTTL = 90 * 24 * time.Hour

// CA signs serve certificates. It is either the operator's intermediate (with its chain up to their root) or a
// self-signed root node-control created.
type CA struct {
	cert   *x509.Certificate
	key    crypto.Signer
	chain  []*x509.Certificate // above cert, not including it; empty for a self-signed root
	now    func() time.Time
	Issued string // "operator" or "self-signed", for the UI
}

// NewSelfSignedCA creates a root CA valid for ten years. Its private key is returned PEM-encoded so the caller can
// keep it encrypted (the state store); node-control never writes it in clear.
func NewSelfSignedCA(commonName string, now time.Time) (*CA, []byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{"headlamp serve"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	ca, err := LoadCA(certPEM, keyPEM, "self-signed")
	return ca, certPEM, keyPEM, err
}

// LoadCA reads a CA certificate (the first PEM block; any further blocks are its chain, nearest first) and its
// private key. The certificate must be a CA that may sign leaves, and the key must match it.
func LoadCA(certPEM, keyPEM []byte, issued string) (*CA, error) {
	certs, err := parseCerts(certPEM)
	if err != nil {
		return nil, err
	}
	if len(certs) == 0 {
		return nil, errors.New("serve CA: no certificate")
	}
	c := certs[0]
	if !c.IsCA || !c.BasicConstraintsValid || c.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, errors.New("serve CA: the certificate is not a CA that may sign certificates")
	}
	b, _ := pem.Decode(keyPEM)
	if b == nil {
		return nil, errors.New("serve CA: no private key")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		if ek, err2 := x509.ParseECPrivateKey(b.Bytes); err2 == nil {
			k = ek
		} else {
			return nil, fmt.Errorf("serve CA: private key: %w", err)
		}
	}
	signer, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("serve CA: the private key cannot sign")
	}
	if !publicKeysEqual(signer.Public(), c.PublicKey) {
		return nil, errors.New("serve CA: the private key does not match the certificate")
	}
	return &CA{cert: c, key: signer, chain: certs[1:], now: time.Now, Issued: issued}, nil
}

// CertPEM is the CA certificate, for clients to trust (self-signed) or for the chain served with each leaf.
func (ca *CA) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
}

// ChainPEM is the CA certificate followed by its chain: what a server sends after its leaf.
func (ca *CA) ChainPEM() []byte {
	out := ca.CertPEM()
	for _, c := range ca.chain {
		out = append(out, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return out
}

// SignCSR issues a server certificate for the public key in csrPEM. The certificate names exactly names, which
// the caller has already approved with Policy.Check: the subject and any names inside the CSR are ignored, so a
// node cannot ask for more than it was given. ttl is capped at MaxLeafTTL and the CA's own expiry; 0 means
// DefaultLeafTTL. The result is the leaf PEM.
func (ca *CA) SignCSR(csrPEM []byte, names []string, ttl time.Duration) ([]byte, time.Time, error) {
	if len(names) == 0 {
		return nil, time.Time{}, errors.New("serve: no names to certify")
	}
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, time.Time{}, errors.New("serve: not a PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("serve: certificate request: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, time.Time{}, fmt.Errorf("serve: certificate request signature: %w", err)
	}
	switch k := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if k.Curve.Params().BitSize < 256 {
			return nil, time.Time{}, errors.New("serve: key too small")
		}
	default:
		if rk, ok := csr.PublicKey.(interface{ Size() int }); !ok || rk.Size()*8 < 2048 {
			return nil, time.Time{}, errors.New("serve: unsupported or too small key")
		}
	}
	if ttl <= 0 {
		ttl = DefaultLeafTTL
	}
	if ttl > MaxLeafTTL {
		ttl = MaxLeafTTL
	}
	now := ca.now()
	notAfter := now.Add(ttl)
	if notAfter.After(ca.cert.NotAfter) {
		notAfter = ca.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: names[0]},
		DNSNames:              append([]string(nil), names...),
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, csr.PublicKey, ca.key)
	if err != nil {
		return nil, time.Time{}, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), notAfter, nil
}

func parseCerts(p []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var b *pem.Block
		b, p = pem.Decode(p)
		if b == nil {
			return out, nil
		}
		if b.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("serve CA: certificate: %w", err)
		}
		out = append(out, c)
	}
}

func publicKeysEqual(a, b crypto.PublicKey) bool {
	type eq interface{ Equal(crypto.PublicKey) bool }
	if e, ok := a.(eq); ok {
		return e.Equal(b)
	}
	return false
}

func serial() *big.Int {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[0] &= 0x7f
	return new(big.Int).SetBytes(b)
}

// Fingerprint is the SHA-256 of a certificate, for users comparing the CA they install.
func (ca *CA) Fingerprint() string {
	sum := sha256.Sum256(ca.cert.Raw)
	return fmt.Sprintf("%X", sum[:])
}
