package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/oidc"
)

// The permission that opens the endpoint. Each tool also needs the permission of its own route.
var permConnect = authz.P(authz.FeatureMCP, authz.LevelAdmin)

const (
	// discoveryRetryInterval is the least time between two discovery attempts after one failed, so
	// an issuer that is down is not asked on every request.
	discoveryRetryInterval = 30 * time.Second
	// discoveryTimeout bounds one discovery of the issuer.
	discoveryTimeout = 10 * time.Second
	// bearerTokenLengthMax bounds the bearer token read from a request.
	bearerTokenLengthMax = 16 << 10
)

// authError is a refused request: status 401 (no or bad credentials) or 403 (a known caller without
// the right).
type authError struct {
	status int
	reason string // for the log only
}

func (e authError) Error() string { return e.reason }

// verifier checks bearer tokens against the issuer. Discovery is lazy: an issuer that is down at
// start does not stop node-control, and the first token after it comes back works.
type verifier struct {
	cfg    oidc.Config
	claims oidc.AccessClaims
	now    func() time.Time

	mu       sync.Mutex
	provider *oidc.Provider
	failedAt time.Time
}

func newVerifier(o *oidcSettings) *verifier {
	return &verifier{
		cfg: oidc.Config{Issuer: o.issuer, HTTP: &http.Client{Timeout: discoveryTimeout}},
		claims: oidc.AccessClaims{
			Audience: o.audience, Login: o.loginClaim, Groups: o.groupsClaim,
		},
		now: time.Now,
	}
}

var errIssuerDown = errors.New("the OIDC issuer cannot be reached")

func (v *verifier) get(ctx context.Context) (*oidc.Provider, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.provider != nil {
		return v.provider, nil
	}
	if !v.failedAt.IsZero() && v.now().Sub(v.failedAt) < discoveryRetryInterval {
		return nil, errIssuerDown
	}
	ctx, cancel := context.WithTimeout(ctx, discoveryTimeout)
	defer cancel()
	p, err := oidc.Discover(ctx, v.cfg)
	if err != nil {
		v.failedAt = v.now()
		return nil, errors.Join(errIssuerDown, err)
	}
	v.provider = p
	return p, nil
}

func (v *verifier) verify(ctx context.Context, token string) (core.Caller, error) {
	p, err := v.get(ctx)
	if err != nil {
		return core.Caller{}, err
	}
	id, err := p.VerifyAccess(ctx, token, v.claims)
	if err != nil {
		return core.Caller{}, err
	}
	return core.Caller{Login: authz.NormalizeLogin(id.Email), Groups: id.Groups}, nil
}

// bearer returns the token of an "Authorization: Bearer" header. present is true when the request
// has an Authorization header of any kind.
func bearer(r *http.Request) (token string, present bool) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return "", false
	}
	scheme, token, ok := strings.Cut(h, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || len(token) > bearerTokenLengthMax {
		return "", true
	}
	return strings.TrimSpace(token), true
}

// authenticate names the caller of an MCP request and checks that it may connect. A bearer token,
// when sent, decides: a bad one is refused and does not fall back to the tailnet identity. Without
// one, the admin listener asks the tailnet who the peer is; the public listener has nobody to ask.
func (m *Module) authenticate(r *http.Request, public bool) (core.Caller, error) {
	c, err := m.identify(r, public)
	if err != nil {
		return core.Caller{}, err
	}
	p := authz.Default()
	if p == nil || !p.Can(authz.Subject{Login: c.Login, Groups: c.Groups}, permConnect) {
		return core.Caller{}, authError{http.StatusForbidden, "missing_mcp_admin"}
	}
	return c, nil
}

func (m *Module) identify(r *http.Request, public bool) (core.Caller, error) {
	if token, present := bearer(r); present {
		if m.verifier == nil {
			return core.Caller{}, authError{http.StatusUnauthorized, "bearer_without_oidc"}
		}
		if token == "" {
			return core.Caller{}, authError{http.StatusUnauthorized, "not_bearer"}
		}
		c, err := m.verifier.verify(r.Context(), token)
		switch {
		case errors.Is(err, errIssuerDown):
			m.log.Warn("mcp: the OIDC issuer cannot be reached", "error", err)
			return core.Caller{}, authError{http.StatusServiceUnavailable, "issuer_down"}
		case err != nil:
			return core.Caller{}, authError{http.StatusUnauthorized, "invalid_token"}
		}
		return c, nil
	}
	if public || m.tailnet == nil {
		return core.Caller{}, authError{http.StatusUnauthorized, "no_token"}
	}
	login, tags, err := m.tailnet.WhoIs(r.Context(), r.RemoteAddr)
	l := authz.NormalizeLogin(login)
	switch {
	case err != nil:
		return core.Caller{}, authError{http.StatusForbidden, "whois_error"}
	case len(tags) > 0 || l == "" || l == "tagged-devices":
		// A tagged node (an ingress, a CI runner) is no person: it must bring a token.
		return core.Caller{}, authError{http.StatusUnauthorized, "tagged"}
	}
	return core.Caller{Login: l}, nil
}

// sameOrigin refuses a browser request from another site (MCP transport rules, against DNS
// rebinding). A request without an Origin header is no browser request and passes.
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, r.Host)
}

// challenge writes a 401 with the RFC 9728 pointer to the metadata, so an MCP client can find the
// issuer.
func (m *Module) challenge(w http.ResponseWriter, reason string) {
	if m.cfg.oidc != nil {
		v := `Bearer resource_metadata="` + m.cfg.oidc.metadataURL() + `"`
		if reason == "invalid_token" {
			v += `, error="invalid_token"`
		}
		w.Header().Set("WWW-Authenticate", v)
	}
	w.WriteHeader(http.StatusUnauthorized)
}
