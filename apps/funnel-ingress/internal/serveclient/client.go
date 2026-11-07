// Package serveclient is the sidecar side of headlamp serve (#117): it asks node-control for a certificate for its
// service, serves TLS with it, and renews it before it expires. The node says nothing about which service it is:
// node-control knows that from the auth key the node joined the tailnet with, and returns the name. The private key
// is generated here and never leaves the process: node-control only sees a certificate request.
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
	"strings"
	"sync/atomic"
	"time"
)

// Client keeps a current certificate for the node's service.
type Client struct {
	URL  string       // node-control, reached over the tailnet
	HTTP *http.Client // dials over the tailnet

	name atomic.Pointer[string] // the service name node-control returned, exact or "*.<suffix>"
	key  *ecdsa.PrivateKey
	cert atomic.Pointer[tls.Certificate]
	exp  atomic.Int64 // unix seconds
	from atomic.Int64 // unix seconds, NotBefore of the current certificate
}

// Response is node-control's answer to POST /serve/v1/cert.
type Response struct {
	Name     string    `json:"name"`
	CertPEM  string    `json:"cert_pem"`
	ChainPEM string    `json:"chain_pem"`
	NotAfter time.Time `json:"not_after"`
}

// APIError is a refusal from node-control. The message is node-control's reason (for example "this node did not
// join with the key of a service"), so the operator can fix the key in the secret.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("node-control answered %d: %s", e.Status, e.Msg)
}

// Name is the service name of the current certificate, empty before the first one.
func (c *Client) Name() string {
	if p := c.name.Load(); p != nil {
		return *p
	}
	return ""
}

// Register asks node-control for a certificate and installs it. The key is created once per process; a renewal
// reuses it.
func (c *Client) Register(ctx context.Context) error {
	if c.key == nil {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return err
		}
		c.key = k
	}
	// The subject is a placeholder: node-control names the certificate itself.
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "headlamp-serve"}}, c.key)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]any{
		"csr": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})),
	})
	req, _ := http.NewRequestWithContext(ctx, "POST", c.URL+"/serve/v1/cert", bytes.NewReader(body))
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
		return fmt.Errorf("certificate answer: %w", err)
	}
	return c.install(r)
}

// install checks that the certificate is for this key and covers the name node-control returned, then makes it
// current. For a wildcard name it checks a host one label under the suffix, which is what the wildcard covers.
func (c *Client) install(r Response) error {
	if r.Name == "" {
		return errors.New("node-control returned no name")
	}
	pair, err := tls.X509KeyPair([]byte(r.CertPEM+r.ChainPEM), pemKey(c.key))
	if err != nil {
		return fmt.Errorf("certificate does not match the key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	probe := r.Name
	if suf, ok := strings.CutPrefix(r.Name, "*."); ok {
		probe = "check." + suf
	}
	if err := leaf.VerifyHostname(probe); err != nil {
		return fmt.Errorf("certificate does not cover %s", r.Name)
	}
	pair.Leaf = leaf
	name := r.Name
	c.name.Store(&name)
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
			logf("serve: no certificate: %v", err)
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
			logf("serve: certificate for %s until %s; renewing at %s", c.Name(), c.Expires().UTC().Format(time.RFC3339), c.RenewAt().UTC().Format(time.RFC3339))
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
