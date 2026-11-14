package serve

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/compat"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// Issuer gets a certificate for names from a public CA (#334). The real one is ACME with DNS-01
// (lego); the tests use a fake. It returns the leaf followed by its chain, and the private key,
// both PEM.
type Issuer interface {
	Obtain(ctx context.Context, names []string) (chainPEM, keyPEM []byte, err error)
}

// The store keys of the ACME certificate. The key is sealed with NODE_CONTROL_STATE_KEY like the CA
// key.
const (
	acmeCertKey  = "acme-cert"
	acmeKeyKey   = "acme-key"
	acmeNamesKey = "acme-names"
	acmeScope    = "serve-acme"
)

const (
	acmeRetry    = 5 * time.Minute // after a failed order
	maxACMECheck = 24 * time.Hour  // the longest SERVE_ACME_CHECK
	// renew at renewNum/renewDen of the certificate's life
	renewNum = 2
	renewDen = 3
)

// acmeCert is the current certificate from the ACME CA, shared by every serve proxy.
type acmeCert struct {
	chainPEM []byte
	keyPEM   []byte
	names    []string
	from     time.Time
	until    time.Time
}

// keeper holds the ACME certificate: it loads it from the store, gets a new one when the names
// change or the certificate is two thirds through its life, and keeps it sealed in the store.
type keeper struct {
	iss   Issuer
	st    Store
	ciph  *statestore.Cipher
	log   *slog.Logger
	now   func() time.Time
	retry time.Duration

	mu      sync.Mutex // one issuance at a time
	cur     atomicCert
	lastErr atomicStr
	kick    chan struct{}
}

type atomicCert struct {
	sync.RWMutex
	c *acmeCert
}

func (a *atomicCert) load() *acmeCert { a.RLock(); defer a.RUnlock(); return a.c }
func (a *atomicCert) store(c *acmeCert) {
	a.Lock()
	a.c = c
	a.Unlock()
}

type atomicStr struct {
	sync.Mutex
	s string
}

func (a *atomicStr) load() string { a.Lock(); defer a.Unlock(); return a.s }
func (a *atomicStr) store(s string) {
	a.Lock()
	a.s = s
	a.Unlock()
}

// acmeNames is what the certificate must cover: the wildcard of the zone, and the wildcard of the
// parent of every service that is deeper than one label under the zone (a "*.apps" service needs
// "*.apps.<zone>"; "a.b" needs "*.b.<zone>"). A wildcard covers one label, so this is the smallest
// set that covers every service name.
func acmeNames(zone string, svcs []Service) []string {
	set := map[string]bool{}
	for _, s := range svcs {
		rest := strings.TrimSuffix(s.Name, "."+zone)
		if !strings.Contains(rest, ".") {
			continue
		}
		_, parent, _ := strings.Cut(rest, ".")
		set["*."+parent+"."+zone] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	slices.Sort(out)
	// the zone wildcard first: it is the common name of the certificate
	return append([]string{"*." + zone}, out...)
}

func parseACME(chainPEM, keyPEM []byte, names []string) (*acmeCert, error) {
	pair, err := tls.X509KeyPair(chainPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("serve acme: certificate and key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &acmeCert{chainPEM: chainPEM, keyPEM: keyPEM, names: names,
		from: leaf.NotBefore, until: leaf.NotAfter}, nil
}

// load reads the certificate kept in the store, if any.
func (k *keeper) load() error {
	chain, err := k.st.GetSecret(acmeCertKey)
	if errors.Is(err, ErrNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	sealed, err := k.st.GetSecret(acmeKeyKey)
	if err != nil {
		return fmt.Errorf("serve acme: key: %w", err)
	}
	key, err := k.ciph.Open(acmeScope, acmeKeyKey, sealed)
	if err != nil {
		return fmt.Errorf("serve acme: key does not decrypt (NODE_CONTROL_STATE_KEY changed?): %w",
			err)
	}
	var names []string
	if b, err := k.st.GetSecret(acmeNamesKey); err == nil {
		_ = json.Unmarshal(b, &names)
	}
	c, err := parseACME(chain, key, names)
	if err != nil {
		return err
	}
	k.cur.store(c)
	return nil
}

// due says whether the certificate must be replaced: none yet, other names, or past two thirds of
// its life.
func (k *keeper) due(want []string) bool {
	c := k.cur.load()
	if c == nil || !slices.Equal(c.names, want) {
		return true
	}
	return !k.now().Before(c.from.Add(c.until.Sub(c.from) * renewNum / renewDen))
}

// ensure gets a new certificate when due. A failure keeps the old certificate.
func (k *keeper) ensure(ctx context.Context, want []string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.due(want) {
		return nil
	}
	k.log.Info("serve acme: requesting a certificate", "names", strings.Join(want, ","))
	chain, key, err := k.iss.Obtain(ctx, want)
	if err == nil {
		var c *acmeCert
		if c, err = parseACME(chain, key, want); err == nil {
			err = k.save(c)
			if err == nil {
				k.cur.store(c)
				k.lastErr.store("")
				k.log.Info("serve acme: certificate in place", "names", strings.Join(want, ","),
					"until", c.until.UTC().Format(time.RFC3339))
				return nil
			}
		}
	}
	k.lastErr.store(err.Error())
	k.log.Warn("serve acme: no new certificate; the old one, if any, stays", "error", err)
	return err
}

func (k *keeper) save(c *acmeCert) error {
	sealed, err := k.ciph.Seal(acmeScope, acmeKeyKey, c.keyPEM)
	if err != nil {
		return err
	}
	names, _ := json.Marshal(c.names)
	if err := k.st.PutSecret(acmeKeyKey, sealed); err != nil {
		return err
	}
	if err := k.st.PutSecret(acmeNamesKey, names); err != nil {
		return err
	}
	return k.st.PutSecret(acmeCertKey, c.chainPEM)
}

// run keeps the certificate current: at start, when kicked (a service was created), and every check
// period. After a failure it tries again after retry.
func (k *keeper) run(ctx context.Context, names func() ([]string, error), check time.Duration) {
	for ctx.Err() == nil {
		wait := check
		if want, err := names(); err != nil {
			k.log.Warn("serve acme: reading the services", "error", err)
			wait = k.retry
		} else if err := k.ensure(ctx, want); err != nil {
			wait = k.retry
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-k.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// poke asks run to check now, without blocking.
func (k *keeper) poke() {
	select {
	case k.kick <- struct{}{}:
	default:
	}
}

// covers reports whether a certificate for names serves name (exact, or "*.x" for a wildcard
// service).
func covers(names []string, name string) bool {
	probe := name
	if suf, ok := strings.CutPrefix(name, "*."); ok {
		probe = "check." + suf
	}
	for _, n := range names {
		if n == probe || n == name {
			return true
		}
		if suf, ok := strings.CutPrefix(n, "*."); ok {
			if l, rest, ok := strings.Cut(probe, "."); ok && l != "" && rest == suf {
				return true
			}
		}
	}
	return false
}

// setupACME makes the keeper. The certificate key is kept sealed, so it needs
// NODE_CONTROL_STATE_KEY like a self-signed CA.
func (m *Module) setupACME(st Store) error {
	ciph, err := statestore.NewCipher(m.env.Getenv("NODE_CONTROL_STATE_KEY"))
	if err == nil && ciph == nil {
		err = errors.New("NODE_CONTROL_STATE_KEY is not set")
	}
	if err != nil {
		return fmt.Errorf("serve: SERVE_CERT_SOURCE=acme needs NODE_CONTROL_STATE_KEY "+
			"to keep the certificate key encrypted: %w", err)
	}
	iss := m.iss
	if iss == nil {
		iss = newLegoIssuer(m.cfg.acme, st, ciph)
	}
	m.acme = &keeper{iss: iss, st: st, ciph: ciph, log: m.log, now: m.now, retry: acmeRetry,
		kick: make(chan struct{}, 1)}
	if err := m.acme.load(); err != nil {
		return err
	}
	m.log.Info("serve: certificates from ACME", "directory", m.cfg.acme.directory,
		"dns", m.cfg.acme.provider)
	return nil
}

func (m *Module) acmeWant() ([]string, error) {
	svcs, err := m.st.Services()
	if err != nil {
		return nil, err
	}
	return acmeNames(m.cfg.zone, svcs), nil
}

// acmeCert hands the shared certificate and its key to a proxy of svc. The CSR is not used.
func (m *Module) acmeCert(w http.ResponseWriter, req certReq, svc Service) {
	if req.API < compat.Must().Interfaces[compat.ServeAPI].Current {
		http.Error(w, "SERVE_CERT_SOURCE=acme needs a newer funnel-sidecar (serve-api 2): "+
			"upgrade the proxy", http.StatusUpgradeRequired)
		return
	}
	c := m.acme.cur.load()
	if c == nil || !covers(c.names, svc.Name) || !m.now().Before(c.until) {
		m.acme.poke()
		w.Header().Set("Retry-After", "30")
		http.Error(w, "the ACME certificate for this name is not ready yet",
			http.StatusServiceUnavailable)
		return
	}
	leaf, chain := splitLeaf(c.chainPEM)
	writeJSON(w, certResp{Name: svc.Name, CertPEM: string(leaf), ChainPEM: string(chain),
		NotAfter: c.until, KeyPEM: string(c.keyPEM)})
}

// splitLeaf returns the first PEM block and the rest.
func splitLeaf(p []byte) ([]byte, []byte) {
	b, rest := pem.Decode(p)
	if b == nil {
		return p, nil
	}
	return pem.EncodeToMemory(b), bytes.TrimLeft(rest, "\n")
}
