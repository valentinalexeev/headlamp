package serve

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certcrypto"
	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/challenge"
	"github.com/go-acme/lego/v4/challenge/dns01"
	"github.com/go-acme/lego/v4/lego"
	"github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/providers/dns/gcloud"
	"github.com/go-acme/lego/v4/providers/dns/rfc2136"
	"github.com/go-acme/lego/v4/providers/dns/route53"
	"github.com/go-acme/lego/v4/registration"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// DefaultACMEDirectory is the Let's Encrypt production directory. The staging one is
// lego.LEDirectoryStaging.
const DefaultACMEDirectory = lego.LEDirectoryProduction

// The DNS providers SERVE_ACME_DNS accepts: the same four as the funnel edge. Each reads its own
// credentials from the environment (lego's names), or uses workload identity where the cloud has
// it.
var acmeProviders = map[string]func() (challenge.Provider, error){
	"googlecloud": func() (challenge.Provider, error) { return gcloud.NewDNSProvider() },
	"route53":     func() (challenge.Provider, error) { return route53.NewDNSProvider() },
	"cloudflare":  func() (challenge.Provider, error) { return cloudflare.NewDNSProvider() },
	"rfc2136":     func() (challenge.Provider, error) { return rfc2136.NewDNSProvider() },
}

func providerNames() string {
	return "googlecloud, route53, cloudflare, rfc2136"
}

// acmeConfig is the ACME part of the serve settings.
type acmeConfig struct {
	directory string
	email     string
	provider  string
	resolvers []string // optional: recursive resolvers for the propagation check
}

func loadACMEConfig(env core.Env) (acmeConfig, error) {
	c := acmeConfig{
		directory: env.Default("SERVE_ACME_DIRECTORY", DefaultACMEDirectory),
		email:     strings.TrimSpace(env.Getenv("SERVE_ACME_EMAIL")),
		provider:  strings.ToLower(strings.TrimSpace(env.Getenv("SERVE_ACME_DNS"))),
	}
	if v := strings.TrimSpace(env.Getenv("SERVE_ACME_RESOLVERS")); v != "" {
		for _, r := range strings.Split(v, ",") {
			if r = strings.TrimSpace(r); r != "" {
				c.resolvers = append(c.resolvers, r)
			}
		}
	}
	if !strings.HasPrefix(c.directory, "https://") {
		return c, errors.New("SERVE_ACME_DIRECTORY must be an https:// URL")
	}
	if c.email == "" || !strings.Contains(c.email, "@") {
		return c, errors.New("SERVE_CERT_SOURCE=acme needs SERVE_ACME_EMAIL " +
			"(the ACME account contact)")
	}
	if _, ok := acmeProviders[c.provider]; !ok {
		return c, fmt.Errorf("SERVE_CERT_SOURCE=acme needs SERVE_ACME_DNS, one of: %s",
			providerNames())
	}
	return c, nil
}

// acmeUser is the ACME account. Its key is kept sealed in the store, so a restart reuses the
// account.
type acmeUser struct {
	email string
	key   crypto.PrivateKey
	reg   *registration.Resource
}

func (u *acmeUser) GetEmail() string                        { return u.email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.reg }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

// acmeOrderTimeout is how long lego waits for the CA to issue after the challenge.
const acmeOrderTimeout = 2 * time.Minute

const (
	acmeAccountKey = "acme-account-key"
	acmeAccountReg = "acme-account:" // + directory URL: an account belongs to one CA
)

// legoIssuer gets certificates with lego and DNS-01.
type legoIssuer struct {
	cfg  acmeConfig
	st   Store
	ciph *statestore.Cipher

	mu     sync.Mutex
	client *lego.Client
}

func newLegoIssuer(cfg acmeConfig, st Store, ciph *statestore.Cipher) *legoIssuer {
	return &legoIssuer{cfg: cfg, st: st, ciph: ciph}
}

func (l *legoIssuer) account() (*acmeUser, error) {
	u := &acmeUser{email: l.cfg.email}
	sealed, err := l.st.GetSecret(acmeAccountKey)
	switch {
	case err == nil:
		pemKey, err := l.ciph.Open(acmeScope, acmeAccountKey, sealed)
		if err != nil {
			return nil, fmt.Errorf("serve acme: account key does not decrypt: %w", err)
		}
		b, _ := pem.Decode(pemKey)
		if b == nil {
			return nil, errors.New("serve acme: account key: no PEM")
		}
		k, err := x509.ParseECPrivateKey(b.Bytes)
		if err != nil {
			return nil, fmt.Errorf("serve acme: account key: %w", err)
		}
		u.key = k
		if raw, err := l.st.GetSecret(acmeAccountReg + l.cfg.directory); err == nil {
			var reg registration.Resource
			if json.Unmarshal(raw, &reg) == nil && reg.URI != "" {
				u.reg = &reg
			}
		}
		return u, nil
	case errors.Is(err, ErrNotFound):
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
		sealed, err := l.ciph.Seal(acmeScope, acmeAccountKey, keyPEM)
		if err != nil {
			return nil, err
		}
		if err := l.st.PutSecret(acmeAccountKey, sealed); err != nil {
			return nil, err
		}
		u.key = k
		return u, nil
	default:
		return nil, err
	}
}

// setup makes the lego client once: the account (registered on first use) and the DNS provider.
func (l *legoIssuer) setup() (*lego.Client, error) {
	if l.client != nil {
		return l.client, nil
	}
	u, err := l.account()
	if err != nil {
		return nil, err
	}
	cfg := lego.NewConfig(u)
	cfg.CADirURL = l.cfg.directory
	cfg.UserAgent = "headlamp-node-control"
	cfg.Certificate = lego.CertificateConfig{KeyType: certcrypto.EC256, Timeout: acmeOrderTimeout}
	client, err := lego.NewClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("serve acme: directory %s: %w", l.cfg.directory, err)
	}
	prov, err := acmeProviders[l.cfg.provider]()
	if err != nil {
		return nil, fmt.Errorf("serve acme: DNS provider %s: %w", l.cfg.provider, err)
	}
	var opts []dns01.ChallengeOption
	if len(l.cfg.resolvers) > 0 {
		opts = append(opts, dns01.AddRecursiveNameservers(l.cfg.resolvers))
	}
	if err := client.Challenge.SetDNS01Provider(prov, opts...); err != nil {
		return nil, err
	}
	if u.reg == nil {
		opts := registration.RegisterOptions{TermsOfServiceAgreed: true}
		reg, err := client.Registration.Register(opts)
		if err != nil {
			return nil, fmt.Errorf("serve acme: account registration: %w", err)
		}
		u.reg = reg
		if b, err := json.Marshal(reg); err == nil {
			_ = l.st.PutSecret(acmeAccountReg+l.cfg.directory, b)
		}
	}
	l.client = client
	return client, nil
}

// Obtain gets one certificate for all names. lego has no context: ctx only stops a call that has
// not started.
func (l *legoIssuer) Obtain(ctx context.Context, names []string) ([]byte, []byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	client, err := l.setup()
	if err != nil {
		return nil, nil, err
	}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{Domains: names, Bundle: true})
	if err != nil {
		return nil, nil, fmt.Errorf("serve acme: order for %s: %w", strings.Join(names, ","), err)
	}
	return res.Certificate, res.PrivateKey, nil
}
