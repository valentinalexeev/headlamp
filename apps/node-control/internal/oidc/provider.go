// Package oidc is a small OpenID Connect relying party for node-control's self-enrolment (#102): discovery, the
// authorization code flow with PKCE, and ID token checks against the issuer's JWKS. It needs no client library: the
// token checks use golang-jwt, and the rest is plain HTTP.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Config is the relying party's configuration.
type Config struct {
	Issuer       string // the issuer URL, exactly as in the ID token's iss
	ClientID     string
	ClientSecret string // may be empty for a public client; PKCE is used either way
	RedirectURL  string // the callback URL registered at the issuer
	Scopes       []string
	HTTP         *http.Client
	Now          func() time.Time
}

// Provider is a discovered issuer.
type Provider struct {
	cfg  Config
	meta metadata

	mu       sync.Mutex
	keys     map[string]any // kid -> *rsa.PublicKey or *ecdsa.PublicKey
	keysAt   time.Time
	lastMiss time.Time
}

type metadata struct {
	Issuer        string `json:"issuer"`
	AuthEndpoint  string `json:"authorization_endpoint"`
	TokenEndpoint string `json:"token_endpoint"`
	JWKSURI       string `json:"jwks_uri"`
	UserinfoURL   string `json:"userinfo_endpoint"`
}

const maxBody = 1 << 20

// Discover reads the issuer's /.well-known/openid-configuration. The issuer in the document must equal cfg.Issuer.
func Discover(ctx context.Context, cfg Config) (*Provider, error) {
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if len(cfg.Scopes) == 0 {
		cfg.Scopes = []string{"openid", "profile", "email"}
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("OIDC issuer must be an http(s) URL")
	}
	var m metadata
	if err := getJSON(ctx, cfg.HTTP, strings.TrimSuffix(cfg.Issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	if m.Issuer != cfg.Issuer {
		return nil, fmt.Errorf("OIDC discovery: the issuer is %q, not %q", m.Issuer, cfg.Issuer)
	}
	if m.AuthEndpoint == "" || m.TokenEndpoint == "" || m.JWKSURI == "" {
		return nil, errors.New("OIDC discovery: the document lacks an authorization, token or JWKS endpoint")
	}
	return &Provider{cfg: cfg, meta: m}, nil
}

func getJSON(ctx context.Context, hc *http.Client, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(v)
}

// key returns the verification key for kid. An unknown kid refreshes the JWKS at most once a minute, so a key
// rotation at the issuer is picked up without letting forged kids hammer it.
// Issuer is the issuer URL of the discovery document, the same as the iss claim of the ID tokens.
func (p *Provider) Issuer() string { return p.meta.Issuer }

func (p *Provider) key(ctx context.Context, kid string) (any, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.cfg.Now()
	if k, ok := p.keys[kid]; ok && now.Sub(p.keysAt) < 24*time.Hour {
		return k, nil
	}
	if p.keys != nil && now.Sub(p.lastMiss) < time.Minute {
		if k, ok := p.keys[kid]; ok {
			return k, nil
		}
		return nil, errors.New("unknown signing key")
	}
	if p.keys != nil {
		p.lastMiss = now // only a refresh caused by an unknown kid counts towards the limit
	}
	keys, err := fetchJWKS(ctx, p.cfg.HTTP, p.meta.JWKSURI)
	if err != nil {
		return nil, err
	}
	p.keys, p.keysAt = keys, now
	if k, ok := keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

type jwk struct {
	Kty, Kid, Use, Alg, N, E, Crv, X, Y string
}

func fetchJWKS(ctx context.Context, hc *http.Client, u string) (map[string]any, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := getJSON(ctx, hc, u, &set); err != nil {
		return nil, fmt.Errorf("JWKS: %w", err)
	}
	out := map[string]any{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		switch k.Kty {
		case "RSA":
			n, err1 := b64int(k.N)
			e, err2 := b64int(k.E)
			if err1 != nil || err2 != nil || !e.IsInt64() || n.BitLen() < 2048 {
				continue
			}
			out[k.Kid] = &rsa.PublicKey{N: n, E: int(e.Int64())}
		case "EC":
			var c elliptic.Curve
			switch k.Crv {
			case "P-256":
				c = elliptic.P256()
			case "P-384":
				c = elliptic.P384()
			default:
				continue
			}
			x, err1 := b64int(k.X)
			y, err2 := b64int(k.Y)
			if err1 != nil || err2 != nil || !c.IsOnCurve(x, y) {
				continue
			}
			out[k.Kid] = &ecdsa.PublicKey{Curve: c, X: x, Y: y}
		}
	}
	if len(out) == 0 {
		return nil, errors.New("JWKS: no usable signing key")
	}
	return out, nil
}

func b64int(s string) (*big.Int, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) == 0 {
		return nil, errors.New("bad key value")
	}
	return new(big.Int).SetBytes(b), nil
}
