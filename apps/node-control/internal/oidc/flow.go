package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Flow is the per-login state the relying party keeps between the redirect and the callback. All three values
// are random; only the verifier is secret, and it never leaves the server.
type Flow struct {
	State    string
	Nonce    string
	Verifier string
}

// NewFlow returns fresh random flow values.
func NewFlow() Flow {
	return Flow{State: random(24), Nonce: random(24), Verifier: random(48)}
}

func random(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// AuthURL is the URL to send the browser to: code flow with PKCE (S256), state and nonce.
func (p *Provider) AuthURL(f Flow) string {
	sum := sha256.Sum256([]byte(f.Verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {p.cfg.ClientID},
		"redirect_uri":          {p.cfg.RedirectURL},
		"scope":                 {strings.Join(p.cfg.Scopes, " ")},
		"state":                 {f.State},
		"nonce":                 {f.Nonce},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	sep := "?"
	if strings.Contains(p.meta.AuthEndpoint, "?") {
		sep = "&"
	}
	return p.meta.AuthEndpoint + sep + q.Encode()
}

// Identity is what a verified login yields.
type Identity struct {
	Subject string
	Email   string
	// EmailUnverified is true when the ID token says the email address is not verified
	// (email_verified false). A token without the claim leaves it false: many issuers only hand out
	// verified addresses and omit the claim.
	EmailUnverified bool
	// EmailVerified is true only when the ID token says email_verified true.
	EmailVerified bool
	Name          string
	Groups        []string
	// GroupsKnown is true when an ID token was verified with a groups claim name: then a missing claim means no
	// groups. A refresh without an ID token leaves it false, and the groups are unknown.
	GroupsKnown  bool
	RefreshToken string // empty when the issuer gave none
	Expiry       time.Time
}

// Errors of the callback. ErrState and ErrDenied are user-facing; the others are failures of the issuer or an attack.
var (
	ErrState  = errors.New("the login state does not match; start again")
	ErrDenied = errors.New("the identity provider refused the login")
	ErrToken  = errors.New("the ID token is not valid")
)

// Exchange completes a login: it checks state, redeems code with the PKCE verifier, and verifies the ID token
// (signature, iss, aud, azp, exp, iat, nonce). groupsClaim names the claim with the groups ("" for none).
func (p *Provider) Exchange(ctx context.Context, f Flow, gotState, code, groupsClaim string) (Identity, error) {
	if f.State == "" || subtle.ConstantTimeCompare([]byte(f.State), []byte(gotState)) != 1 {
		return Identity{}, ErrState
	}
	if code == "" {
		return Identity{}, ErrDenied
	}
	tok, err := p.token(ctx, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {p.cfg.RedirectURL},
		"code_verifier": {f.Verifier},
	})
	if err != nil {
		return Identity{}, err
	}
	id, err := p.Verify(ctx, tok.IDToken, f.Nonce, groupsClaim)
	if err != nil {
		return Identity{}, err
	}
	id.RefreshToken = tok.RefreshToken
	return id, nil
}

type tokenResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func (p *Provider) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	form.Set("client_id", p.cfg.ClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.meta.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return tokenResponse{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if p.cfg.ClientSecret != "" {
		req.SetBasicAuth(url.QueryEscape(p.cfg.ClientID), url.QueryEscape(p.cfg.ClientSecret))
	}
	resp, err := p.cfg.HTTP.Do(req)
	if err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Error == "invalid_grant" {
			return tokenResponse{}, ErrDenied
		}
		return tokenResponse{}, fmt.Errorf("token endpoint: status %d %s", resp.StatusCode, e.Error)
	}
	var t tokenResponse
	if err := json.Unmarshal(body, &t); err != nil {
		return tokenResponse{}, fmt.Errorf("token endpoint: %w", err)
	}
	return t, nil
}

// Verify checks an ID token. nonce must match when not empty (a refresh may return a token without one).
func (p *Provider) Verify(ctx context.Context, raw, nonce, groupsClaim string) (Identity, error) {
	claims, err := p.parse(ctx, raw, p.cfg.ClientID)
	if err != nil {
		return Identity{}, err
	}
	// With several audiences, azp must name this client.
	if aud, _ := claims.GetAudience(); len(aud) > 1 {
		if azp, _ := claims["azp"].(string); azp != p.cfg.ClientID {
			return Identity{}, fmt.Errorf("%w: azp", ErrToken)
		}
	}
	got, _ := claims["nonce"].(string)
	if nonce != "" && subtle.ConstantTimeCompare([]byte(got), []byte(nonce)) != 1 {
		return Identity{}, fmt.Errorf("%w: nonce", ErrToken)
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Identity{}, fmt.Errorf("%w: no sub", ErrToken)
	}
	id := Identity{Subject: sub}
	id.Email, _ = claims["email"].(string)
	switch v := claims["email_verified"].(type) {
	case bool:
		id.EmailVerified, id.EmailUnverified = v, !v
	case string: // some issuers send the claim as a string
		id.EmailVerified = strings.EqualFold(v, "true")
		id.EmailUnverified = strings.EqualFold(v, "false")
	}
	id.Name, _ = claims["name"].(string)
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		id.Expiry = exp.Time
	}
	if groupsClaim != "" {
		id.Groups = stringsOf(claims[groupsClaim])
		id.GroupsKnown = true
	}
	return id, nil
}

// parse checks a signed JWT of this issuer for audience: signature by a key of the JWKS, iss, aud, exp and iat.
func (p *Provider) parse(ctx context.Context, raw, audience string) (jwt.MapClaims, error) {
	if raw == "" {
		return nil, fmt.Errorf("%w: missing", ErrToken)
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string) // a token without a kid fails the key lookup
		return p.key(ctx, kid)
	},
		jwt.WithValidMethods([]string{"RS256", "RS384", "RS512", "ES256", "ES384", "PS256"}),
		jwt.WithIssuer(p.cfg.Issuer),
		jwt.WithAudience(audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(time.Minute),
		jwt.WithTimeFunc(p.cfg.Now),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrToken, err)
	}
	return claims, nil
}

// AccessClaims names the claims that VerifyAccess reads.
type AccessClaims struct {
	Audience string // the resource: the aud the token must name
	Login    string // the claim that names the caller, for example "email" or "preferred_username"
	Groups   string // the claim with the groups ("" for none)
}

// VerifyAccess checks an access token that is a JWT (RFC 9068) for a resource server, such as the MCP endpoint:
// signature, iss, exp, iat and aud (it must name c.Audience). The login is the c.Login claim, lower-cased. When that
// claim is "email", a token with email_verified false is refused. An issuer that hands out opaque access tokens
// cannot be used.
func (p *Provider) VerifyAccess(ctx context.Context, raw string, c AccessClaims) (Identity, error) {
	if c.Audience == "" || c.Login == "" {
		return Identity{}, errors.New("oidc: VerifyAccess needs an audience and a login claim")
	}
	claims, err := p.parse(ctx, raw, c.Audience)
	if err != nil {
		return Identity{}, err
	}
	sub, _ := claims["sub"].(string)
	if sub == "" {
		return Identity{}, fmt.Errorf("%w: no sub", ErrToken)
	}
	id := Identity{Subject: sub}
	login, _ := claims[c.Login].(string)
	id.Email = strings.ToLower(strings.TrimSpace(login))
	if id.Email == "" {
		return Identity{}, fmt.Errorf("%w: no %s claim", ErrToken, c.Login)
	}
	if c.Login == "email" {
		switch v := claims["email_verified"].(type) {
		case bool:
			id.EmailUnverified = !v
		case string:
			id.EmailUnverified = strings.EqualFold(v, "false")
		}
		if id.EmailUnverified {
			return Identity{}, fmt.Errorf("%w: the email address is not verified", ErrToken)
		}
	}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		id.Expiry = exp.Time
	}
	if c.Groups != "" {
		id.Groups = stringsOf(claims[c.Groups])
		id.GroupsKnown = true
	}
	return id, nil
}

func stringsOf(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// Refresh redeems a refresh token and verifies the new ID token. It is the periodic re-check of a user: an issuer
// that disabled the user, or revoked the grant, refuses it (ErrDenied). An issuer that returns no ID token on refresh
// yields an Identity with only the subject, the same as before.
func (p *Provider) Refresh(ctx context.Context, refreshToken, subject, groupsClaim string) (Identity, error) {
	tok, err := p.token(ctx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refreshToken}})
	if err != nil {
		return Identity{}, err
	}
	id := Identity{Subject: subject}
	if tok.IDToken != "" {
		if id, err = p.Verify(ctx, tok.IDToken, "", groupsClaim); err != nil {
			return Identity{}, err
		}
		if id.Subject != subject {
			return Identity{}, fmt.Errorf("%w: the subject changed", ErrToken)
		}
	}
	id.RefreshToken = tok.RefreshToken
	if id.RefreshToken == "" {
		id.RefreshToken = refreshToken
	}
	return id, nil
}
