package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc/oidctest"
)

const redirect = "https://links.example.test/oidc/callback"

func setup(t *testing.T, secret string) (*oidctest.Issuer, *oidc.Provider) {
	is := oidctest.New(t, "nc", secret, redirect)
	p, err := oidc.Discover(context.Background(), oidc.Config{Issuer: is.URL, ClientID: "nc", ClientSecret: secret, RedirectURL: redirect})
	if err != nil {
		t.Fatal(err)
	}
	return is, p
}

// login follows the issuer's redirect and returns the state and code it sends back.
func login(t *testing.T, p *oidc.Provider, f oidc.Flow) (state, code string) {
	t.Helper()
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Get(p.AuthURL(f))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	u, err := url.Parse(resp.Header.Get("Location"))
	if err != nil || !strings.HasPrefix(u.String(), redirect) {
		t.Fatalf("redirect %q (%d)", resp.Header.Get("Location"), resp.StatusCode)
	}
	return u.Query().Get("state"), u.Query().Get("code")
}

func TestLoginRoundTrip(t *testing.T) {
	for _, secret := range []string{"", "s3cret"} {
		_, p := setup(t, secret)
		f := oidc.NewFlow()
		st, code := login(t, p, f)
		id, err := p.Exchange(context.Background(), f, st, code, "groups")
		if err != nil {
			t.Fatalf("secret %q: %v", secret, err)
		}
		if id.Subject != "sub-1" || id.Email != "anna@example.test" || len(id.Groups) != 1 || id.Groups[0] != "staff" || id.RefreshToken == "" {
			t.Errorf("%+v", id)
		}
	}
}

func TestAuthURLHasPKCEAndNonce(t *testing.T) {
	_, p := setup(t, "")
	f := oidc.NewFlow()
	u, _ := url.Parse(p.AuthURL(f))
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") != f.Nonce || q.Get("state") != f.State {
		t.Errorf("%v", q)
	}
	if strings.Contains(u.String(), f.Verifier) {
		t.Error("the verifier leaked into the URL")
	}
}

func TestRefusals(t *testing.T) {
	ctx := context.Background()
	t.Run("wrong state", func(t *testing.T) {
		_, p := setup(t, "")
		f := oidc.NewFlow()
		_, code := login(t, p, f)
		if _, err := p.Exchange(ctx, f, "other", code, ""); !errors.Is(err, oidc.ErrState) {
			t.Error(err)
		}
	})
	t.Run("wrong verifier", func(t *testing.T) {
		_, p := setup(t, "")
		f := oidc.NewFlow()
		st, code := login(t, p, f)
		f.Verifier = "x" + f.Verifier
		if _, err := p.Exchange(ctx, f, st, code, ""); !errors.Is(err, oidc.ErrDenied) {
			t.Error(err)
		}
	})
	t.Run("code replay", func(t *testing.T) {
		_, p := setup(t, "")
		f := oidc.NewFlow()
		st, code := login(t, p, f)
		if _, err := p.Exchange(ctx, f, st, code, ""); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Exchange(ctx, f, st, code, ""); !errors.Is(err, oidc.ErrDenied) {
			t.Error(err)
		}
	})
	for name, tamper := range map[string]func(jwt.MapClaims){
		"wrong nonce": func(c jwt.MapClaims) { c["nonce"] = "n" },
		"wrong aud":   func(c jwt.MapClaims) { c["aud"] = "other" },
		"wrong iss":   func(c jwt.MapClaims) { c["iss"] = "https://evil.example" },
		"expired":     func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() },
		"no exp":      func(c jwt.MapClaims) { delete(c, "exp") },
		"no sub":      func(c jwt.MapClaims) { delete(c, "sub") },
		"multi aud":   func(c jwt.MapClaims) { c["aud"] = []string{"nc", "other"} },
		"future iat":  func(c jwt.MapClaims) { c["iat"] = time.Now().Add(time.Hour).Unix() },
	} {
		t.Run(name, func(t *testing.T) {
			is, p := setup(t, "")
			is.Tamper = tamper
			f := oidc.NewFlow()
			st, code := login(t, p, f)
			if _, err := p.Exchange(ctx, f, st, code, ""); !errors.Is(err, oidc.ErrToken) {
				t.Error(err)
			}
		})
	}
	t.Run("forged signature", func(t *testing.T) {
		is, p := setup(t, "")
		other, _ := rsa.GenerateKey(rand.Reader, 2048)
		is.SignWith = other
		f := oidc.NewFlow()
		st, code := login(t, p, f)
		if _, err := p.Exchange(ctx, f, st, code, ""); !errors.Is(err, oidc.ErrToken) {
			t.Error(err)
		}
	})
	t.Run("alg none", func(t *testing.T) {
		_, p := setup(t, "")
		tok := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.MapClaims{"sub": "x"})
		raw, _ := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if _, err := p.Verify(ctx, raw, "", ""); !errors.Is(err, oidc.ErrToken) {
			t.Error(err)
		}
	})
}

func TestKeyRotationIsPickedUp(t *testing.T) {
	is, p := setup(t, "")
	f := oidc.NewFlow()
	st, code := login(t, p, f)
	if _, err := p.Exchange(context.Background(), f, st, code, ""); err != nil {
		t.Fatal(err)
	}
	is.RotateKey(t, "k2")
	// the first miss refreshes the JWKS at once; a minute has not passed since the first fetch, but the first fetch
	// was a hit, not a miss
	f = oidc.NewFlow()
	st, code = login(t, p, f)
	if _, err := p.Exchange(context.Background(), f, st, code, ""); err != nil {
		t.Errorf("after rotation: %v", err)
	}
}

func TestRefreshAndDisable(t *testing.T) {
	is, p := setup(t, "")
	f := oidc.NewFlow()
	st, code := login(t, p, f)
	id, err := p.Exchange(context.Background(), f, st, code, "groups")
	if err != nil {
		t.Fatal(err)
	}
	again, err := p.Refresh(context.Background(), id.RefreshToken, id.Subject, "groups")
	if err != nil || again.Subject != "sub-1" || again.RefreshToken != id.RefreshToken || again.Groups[0] != "staff" {
		t.Fatalf("%+v %v", again, err)
	}
	is.SetUser(oidctest.User{Subject: "sub-1", Groups: nil})
	if again, _ = p.Refresh(context.Background(), id.RefreshToken, id.Subject, "groups"); len(again.Groups) != 0 {
		t.Errorf("groups not updated: %v", again.Groups)
	}
	is.Disabled = true
	if _, err := p.Refresh(context.Background(), id.RefreshToken, id.Subject, "groups"); !errors.Is(err, oidc.ErrDenied) {
		t.Errorf("disabled: %v", err)
	}
}

func TestDiscoveryIssuerMismatch(t *testing.T) {
	is := oidctest.New(t, "nc", "", redirect)
	if _, err := oidc.Discover(context.Background(), oidc.Config{Issuer: is.URL + "/", ClientID: "nc", RedirectURL: redirect}); err == nil {
		t.Error("a different issuer string must be refused")
	}
}

func TestForgedKidsDoNotHammerTheIssuer(t *testing.T) {
	is, p := setup(t, "")
	f := oidc.NewFlow()
	st, code := login(t, p, f)
	if _, err := p.Exchange(context.Background(), f, st, code, ""); err != nil {
		t.Fatal(err)
	}
	fetches := func() int { return is.JWKSCalls() }
	before := fetches()
	for i := 0; i < 5; i++ {
		tok := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.MapClaims{"sub": "x"})
		tok.Header["kid"] = "forged"
		k, _ := rsa.GenerateKey(rand.Reader, 2048)
		raw, _ := tok.SignedString(k)
		if _, err := p.Verify(context.Background(), raw, "", ""); !errors.Is(err, oidc.ErrToken) {
			t.Fatal(err)
		}
	}
	if n := fetches() - before; n > 1 {
		t.Errorf("%d JWKS fetches for 5 forged kids, want at most 1", n)
	}
}

func TestFakeIssuerRefusesUnregisteredRedirect(t *testing.T) {
	is := oidctest.New(t, "nc", "", redirect)
	hc := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, target := range []string{"https://evil.example/cb", redirect + "x", ""} {
		q := url.Values{"client_id": {"nc"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "redirect_uri": {target}}
		resp, err := hc.Get(is.URL + "/authorize?" + q.Encode())
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || resp.Header.Get("Location") != "" {
			t.Errorf("%q: %d %q", target, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}
