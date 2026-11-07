// Package oidctest is a fake OpenID Connect issuer for tests: discovery, JWKS, an authorization endpoint that
// approves at once, and a token endpoint that checks PKCE and serves refresh tokens. Tests can flip its behaviour.
package oidctest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// User is the identity the issuer logs in.
type User struct {
	Subject string
	Email   string
	Groups  []string
}

// Issuer is the fake.
type Issuer struct {
	*httptest.Server
	ClientID     string
	ClientSecret string

	mu      sync.Mutex
	key     *rsa.PrivateKey
	kid     string
	user    User
	codes   map[string]codeGrant
	refresh map[string]string // refresh token -> subject
	// Knobs.
	Disabled   bool                       // refuse refresh (the user is disabled)
	Tamper     func(claims jwt.MapClaims) // edit the claims of the next ID tokens
	SignWith   *rsa.PrivateKey            // sign with another key (keeps the advertised kid)
	Now        func() time.Time
	TokenCalls int
	jwksCalls  int
}

type codeGrant struct {
	challenge, nonce, redirect string
}

// New starts an issuer for clientID. It is closed with the test.
func New(t testing.TB, clientID, clientSecret string) *Issuer {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &Issuer{ClientID: clientID, ClientSecret: clientSecret, key: k, kid: "k1",
		user:  User{Subject: "sub-1", Email: "anna@example.test", Groups: []string{"staff"}},
		codes: map[string]codeGrant{}, refresh: map[string]string{}, Now: time.Now}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", is.discovery)
	mux.HandleFunc("GET /jwks", is.jwks)
	mux.HandleFunc("GET /authorize", is.authorize)
	mux.HandleFunc("POST /token", is.token)
	is.Server = httptest.NewServer(mux)
	t.Cleanup(is.Close)
	return is
}

// SetUser sets who logs in next.
func (is *Issuer) SetUser(u User) { is.mu.Lock(); is.user = u; is.mu.Unlock() }

// RotateKey replaces the signing key and kid.
func (is *Issuer) RotateKey(t testing.TB, kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is.mu.Lock()
	is.key, is.kid = k, kid
	is.mu.Unlock()
}

func (is *Issuer) discovery(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"issuer": is.URL, "authorization_endpoint": is.URL + "/authorize",
		"token_endpoint": is.URL + "/token", "jwks_uri": is.URL + "/jwks"})
}

// JWKSCalls is the number of JWKS fetches so far.
func (is *Issuer) JWKSCalls() int { is.mu.Lock(); defer is.mu.Unlock(); return is.jwksCalls }

func (is *Issuer) jwks(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.jwksCalls++
	pub := is.key.PublicKey
	writeJSON(w, map[string]any{"keys": []any{map[string]any{"kty": "RSA", "kid": is.kid, "use": "sig", "alg": "RS256",
		"n": b64(pub.N.Bytes()), "e": b64(big.NewInt(int64(pub.E)).Bytes())}}})
}

// authorize approves at once: it redirects to redirect_uri with a code and the state.
func (is *Issuer) authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("client_id") != is.ClientID || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	code := randStr()
	is.mu.Lock()
	is.codes[code] = codeGrant{challenge: q.Get("code_challenge"), nonce: q.Get("nonce"), redirect: q.Get("redirect_uri")}
	is.mu.Unlock()
	u, _ := url.Parse(q.Get("redirect_uri"))
	v := u.Query()
	v.Set("code", code)
	v.Set("state", q.Get("state"))
	u.RawQuery = v.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (is *Issuer) token(w http.ResponseWriter, r *http.Request) {
	is.mu.Lock()
	defer is.mu.Unlock()
	is.TokenCalls++
	if is.ClientSecret != "" {
		id, sec, ok := r.BasicAuth()
		if !ok || id != is.ClientID || sec != is.ClientSecret {
			oauthErr(w, "invalid_client")
			return
		}
	}
	_ = r.ParseForm()
	switch r.PostForm.Get("grant_type") {
	case "authorization_code":
		g, ok := is.codes[r.PostForm.Get("code")]
		delete(is.codes, r.PostForm.Get("code"))
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if !ok || g.redirect != r.PostForm.Get("redirect_uri") || b64(sum[:]) != g.challenge {
			oauthErr(w, "invalid_grant")
			return
		}
		rt := randStr()
		is.refresh[rt] = is.user.Subject
		writeJSON(w, map[string]any{"id_token": is.sign(g.nonce), "access_token": randStr(), "refresh_token": rt, "token_type": "Bearer"})
	case "refresh_token":
		sub, ok := is.refresh[r.PostForm.Get("refresh_token")]
		if !ok || is.Disabled || sub != is.user.Subject {
			oauthErr(w, "invalid_grant")
			return
		}
		writeJSON(w, map[string]any{"id_token": is.sign(""), "access_token": randStr(), "token_type": "Bearer"})
	default:
		oauthErr(w, "unsupported_grant_type")
	}
}

func (is *Issuer) sign(nonce string) string {
	now := is.Now()
	c := jwt.MapClaims{"iss": is.URL, "aud": is.ClientID, "sub": is.user.Subject, "email": is.user.Email,
		"groups": is.user.Groups, "iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix()}
	if nonce != "" {
		c["nonce"] = nonce
	}
	if is.Tamper != nil {
		is.Tamper(c)
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	t.Header["kid"] = is.kid
	var k crypto.Signer = is.key
	if is.SignWith != nil {
		k = is.SignWith
	}
	s, err := t.SignedString(k)
	if err != nil {
		panic(err)
	}
	return s
}

func oauthErr(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randStr() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64(b)
}
