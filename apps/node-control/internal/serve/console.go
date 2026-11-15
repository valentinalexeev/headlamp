package serve

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
)

// The admin console on HTTPS (#366): node-control serves its own console at a name under the serve
// zone, with the serve certificate, so an OIDC sign-in can use an https redirect URL and a Secure
// cookie. The name resolves to node-control's own tailnet addresses, through the same DNS answers
// as the services.

// consoleLeafTTL is how long a console certificate from the serve CA lasts. It is renewed at half.
const (
	consoleLeafTTL     = 7 * 24 * time.Hour
	consoleRenewBefore = consoleLeafTTL / 2
)

// console is the state of the console name: its addresses and, with the serve CA, its leaf.
type console struct {
	mu    sync.Mutex
	name  string
	addrs []netip.Addr
	leaf  *tls.Certificate
	until time.Time
}

// SetConsole makes name (one label under SERVE_ZONE, so the ACME *.<zone> certificate covers it)
// resolve to addrs, node-control's own tailnet addresses, and lets ConsoleCert serve a certificate
// for it. An empty name turns it off.
func (m *Module) SetConsole(name string, addrs []netip.Addr) error {
	name = servedns.Normalize(name)
	if name != "" {
		label, rest, _ := strings.Cut(name, ".")
		if m.cfg.zone == "" || rest != m.cfg.zone || label == "" || label == "*" {
			return errors.New("the console name must be one label under SERVE_ZONE, " +
				"for example console.<SERVE_ZONE>")
		}
	}
	m.con.mu.Lock()
	m.con.name, m.con.addrs, m.con.leaf = name, append([]netip.Addr(nil), addrs...), nil
	m.con.mu.Unlock()
	return nil
}

// isConsoleName reports whether a service name would take the console name.
func (m *Module) isConsoleName(name string) bool {
	m.con.mu.Lock()
	c := m.con.name
	m.con.mu.Unlock()
	return c != "" && name == c
}

// consoleRecord is the DNS record of the console name, or false when it is off.
func (m *Module) consoleRecord() (servedns.Record, bool) {
	m.con.mu.Lock()
	defer m.con.mu.Unlock()
	if m.con.name == "" || len(m.con.addrs) == 0 {
		return servedns.Record{}, false
	}
	return servedns.Record{Name: m.con.name, Addrs: append([]netip.Addr(nil), m.con.addrs...)}, true
}

// ConsoleCert is a tls.Config.GetCertificate for the console name. With SERVE_CERT_SOURCE=acme it
// serves the shared certificate (its *.<zone> covers a name one label under the zone). With the
// serve CA it signs a leaf for the console name, kept in memory and renewed at half its life.
func (m *Module) ConsoleCert(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.con.mu.Lock()
	name := m.con.name
	m.con.mu.Unlock()
	if name == "" {
		return nil, errors.New("console on https is off")
	}
	if hello != nil && hello.ServerName != "" && servedns.Normalize(hello.ServerName) != name {
		return nil, errors.New("unknown server name")
	}
	if m.acme != nil {
		c := m.acme.cur.load()
		if c == nil {
			return nil, errors.New("the ACME certificate is not ready yet")
		}
		pair, err := tls.X509KeyPair(c.chainPEM, c.keyPEM)
		if err != nil {
			return nil, err
		}
		return &pair, nil
	}
	if m.ca == nil {
		return nil, errors.New("serve has no certificate source")
	}
	now := m.now()
	m.con.mu.Lock()
	defer m.con.mu.Unlock()
	if m.con.leaf != nil && now.Before(m.con.until.Add(-consoleRenewBefore)) {
		return m.con.leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr := &x509.CertificateRequest{DNSNames: []string{name}}
	der, err := x509.CreateCertificateRequest(rand.Reader, csr, key)
	if err != nil {
		return nil, err
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	leafPEM, until, err := m.ca.SignCSR(csrPEM,
		[]string{name}, consoleLeafTTL)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	pair, err := tls.X509KeyPair(append(leafPEM, m.ca.ChainPEM()...),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		return nil, err
	}
	m.con.leaf, m.con.until = &pair, until
	return &pair, nil
}
