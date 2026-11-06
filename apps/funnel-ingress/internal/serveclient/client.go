// Package serveclient is the sidecar side of headlamp serve (#117): it asks node-control for private tailnet names
// and a certificate for them, serves TLS with that certificate, and renews it before it expires. The private key is
// generated here and never leaves the process: node-control only sees a certificate request.
package serveclient

import (
	"bytes"
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
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// Client registers names and keeps a current certificate.
type Client struct {
	URL       string       // node-control, reached over the tailnet
	Namespace string       // optional when the node has one namespace tag
	Names     []string     // full names under the MagicDNS base domain
	HTTP      *http.Client // dials over the tailnet

	key  *ecdsa.PrivateKey
	cert atomic.Pointer[tls.Certificate]
	exp  atomic.Int64 // unix seconds
	from atomic.Int64 // unix seconds, NotBefore of the current certificate
}

// Response is node-control's answer to a registration.
type Response struct {
	Names   []string  `json:"names"`
	Cert    string    `json:"cert"`
	Chain   string    `json:"chain"`
	Expires time.Time `json:"expires"`
}

// APIError is a refusal from node-control. The message is node-control's reason (for example "name held by
// another node"), so the operator can fix the namespace or the name.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("node-control answered %d: %s", e.Status, e.Msg)
}

// Register asks node-control for the names and a certificate, and installs it. The key is created once per
// process; a renewal reuses it.
func (c *Client) Register(ctx context.Context) error {
	if c.key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		c.key = k
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: c.Names[0]}}, c.key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"namespace": c.Namespace,
		"names":     c.Names,
		"csr":       string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})),
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.URL+"/serve/v1/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return &APIError{Status: resp.StatusCode, Msg: string(bytes.TrimSpace(b))}
	}
	var r Response
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("register answer: %w", err)
	}
	return c.install(r)
}

// install checks that the certificate is for this key and these names, then makes it current.
func (c *Client) install(r Response) error {
	pair, err := tls.X509KeyPair([]byte(r.Cert+r.Chain), pemKey(c.key))
	if err != nil {
		return fmt.Errorf("certificate does not match the key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	for _, n := range c.Names {
		if err := leaf.VerifyHostname(n); err != nil {
			return fmt.Errorf("certificate does not cover %s", n)
		}
	}
	pair.Leaf = leaf
	c.cert.Store(&pair)
	c.exp.Store(leaf.NotAfter.Unix())
	c.from.Store(leaf.NotBefore.Unix())
	return nil
}

func pemKey(k *ecdsa.PrivateKey) []byte {
	der, _ := x509.MarshalECPrivateKey(k)
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// TLSConfig serves the current certificate; a renewal takes effect on the next handshake, with no restart.
func (c *Client) TLSConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			if p := c.cert.Load(); p != nil {
				return p, nil
			}
			return nil, errors.New("serve: no certificate yet")
		},
	}
}

// RenewAt is when to renew: two thirds into the certificate's life. Zero when there is no certificate.
func (c *Client) RenewAt() time.Time {
	from, exp := c.from.Load(), c.exp.Load()
	if exp == 0 {
		return time.Time{}
	}
	return time.Unix(from+(exp-from)*2/3, 0)
}

// Expires is the current certificate's end, zero when there is none.
func (c *Client) Expires() time.Time {
	if e := c.exp.Load(); e != 0 {
		return time.Unix(e, 0)
	}
	return time.Time{}
}

// Keep registers, then renews at RenewAt until ctx ends. A failure is retried with backoff up to a minute; the
// current certificate keeps serving meanwhile, so a short outage of node-control does not break TLS. ready reports
// whether a valid certificate is in place.
func (c *Client) Keep(ctx context.Context, ready func(bool), logf func(string, ...any)) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.Register(ctx)
		now := time.Now()
		valid := c.Expires().After(now)
		if ready != nil {
			ready(valid)
		}
		var wait time.Duration
		if err != nil {
			logf("serve: registration failed: %v", err)
			wait = backoff
			if backoff *= 2; backoff > time.Minute {
				backoff = time.Minute
			}
		} else {
			backoff = time.Second
			wait = time.Until(c.RenewAt())
			if wait < time.Minute {
				wait = time.Minute
			}
			logf("serve: certificate for %v until %s; renewing at %s", c.Names, c.Expires().UTC().Format(time.RFC3339), c.RenewAt().UTC().Format(time.RFC3339))
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}
