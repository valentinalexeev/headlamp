package mcp

import (
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// settings is the MCP configuration read from the environment.
type settings struct {
	enabled bool
	// public serves the endpoint on the public listener too (MCP_PUBLIC=1). There only a bearer
	// token counts.
	public bool
	oidc   *oidcSettings // nil without MCP_OIDC_ISSUER: then only tailnet logins can connect
}

// oidcSettings says which access tokens the endpoint accepts.
type oidcSettings struct {
	issuer      string
	resource    string // the URL of the endpoint, as MCP clients reach it (RFC 9728 "resource")
	audience    string // the aud a token must name; defaults to resource
	loginClaim  string
	groupsClaim string
}

// claimNameLengthMax bounds a claim name from the environment.
const claimNameLengthMax = 64

func flag(env core.Env, key string) (bool, error) {
	switch v := strings.TrimSpace(env.Getenv(key)); v {
	case "", "0", "false":
		return false, nil
	case "1", "true":
		return true, nil
	default:
		return false, fmt.Errorf("%s=%q: want 1 (on) or 0 (off)", key, v)
	}
}

func readSettings(env core.Env) (settings, error) {
	var s settings
	var err error
	if s.enabled, err = flag(env, "MCP_ENABLED"); err != nil || !s.enabled {
		return settings{}, err
	}
	if s.public, err = flag(env, "MCP_PUBLIC"); err != nil {
		return settings{}, err
	}
	if s.oidc, err = readOIDC(env); err != nil {
		return settings{}, err
	}
	if s.public && s.oidc == nil {
		return settings{}, errors.New("MCP_PUBLIC=1 needs MCP_OIDC_ISSUER: " +
			"the public listener has no tailnet identity, " +
			"so only a bearer token can name the caller")
	}
	return s, nil
}

func readOIDC(env core.Env) (*oidcSettings, error) {
	issuer := strings.TrimSpace(env.Getenv("MCP_OIDC_ISSUER"))
	if issuer == "" {
		return nil, nil
	}
	o := &oidcSettings{
		issuer:      issuer,
		resource:    strings.TrimSpace(env.Getenv("MCP_RESOURCE_URL")),
		audience:    strings.TrimSpace(env.Getenv("MCP_OIDC_AUDIENCE")),
		loginClaim:  strings.TrimSpace(env.Default("MCP_OIDC_LOGIN_CLAIM", "email")),
		groupsClaim: strings.TrimSpace(env.Getenv("MCP_OIDC_GROUPS_CLAIM")),
	}
	if err := httpURL("MCP_OIDC_ISSUER", o.issuer); err != nil {
		return nil, err
	}
	if o.resource == "" {
		return nil, errors.New("MCP_OIDC_ISSUER is set but MCP_RESOURCE_URL is not: " +
			"set it to the URL MCP clients use, for example https://node-control.example.com/mcp")
	}
	if err := httpURL("MCP_RESOURCE_URL", o.resource); err != nil {
		return nil, err
	}
	if o.audience == "" {
		o.audience = o.resource
	}
	for key, v := range map[string]string{
		"MCP_OIDC_LOGIN_CLAIM": o.loginClaim, "MCP_OIDC_GROUPS_CLAIM": o.groupsClaim,
	} {
		if len(v) > claimNameLengthMax || strings.ContainsAny(v, " \t\r\n") {
			return nil, fmt.Errorf("%s=%q: want one claim name", key, v)
		}
	}
	return o, nil
}

func httpURL(key, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s=%q: want an http(s) URL", key, raw)
	}
	if u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%s=%q: no user info or fragment", key, raw)
	}
	return nil
}

// metadataURL is the RFC 9728 path of the protected resource metadata for the resource URL: the
// well-known prefix then the path of the resource ("/.well-known/oauth-protected-resource/mcp" for
// ".../mcp").
func (o *oidcSettings) metadataURL() string {
	u, err := url.Parse(o.resource)
	if err != nil {
		return ""
	}
	p := strings.TrimSuffix(u.Path, "/")
	return u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource" + p
}
