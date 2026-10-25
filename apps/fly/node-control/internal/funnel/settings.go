// Package funnel manages funnel namespaces: named groups of ports published through a shared
// ingress node, with their ACL entries, manual routes and blocks kept in a small SQLite store.
//
// The module is active only when FUNNEL_DB is set; with it unset nothing is registered.
package funnel

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
)

const (
	defaultFunnelTag  = "tag:funnel"
	defaultNSPrefix   = "tag:ns-"
	defaultIngressTag = "tag:funnel-ingress"
	maxTagLen         = 64
)

// Settings is the module configuration read from FUNNEL_* variables.
type Settings struct {
	DBPath          string
	PublicDomain    string // lower-case DNS name that namespace hosts live under
	IngressHostname string // optional: the ingress node's own name
	FunnelTag       string // tag of every funnel-capable node
	NSPrefix        string // tag prefix of a namespace: <prefix><name>
	IngressTag      string // tag of the ingress node
	TagOwners       []string
	KeyUser         string // a headscale user NAME, resolved to an id when minting keys
	Admins          authz.Allowed
}

// SettingsFromEnv reads and validates the configuration. It returns nil, nil when FUNNEL_DB is
// unset. Errors name the variable and never contain its value.
func SettingsFromEnv(env core.Env) (*Settings, error) {
	db := strings.TrimSpace(env.Getenv("FUNNEL_DB"))
	if db == "" {
		return nil, nil
	}
	s := &Settings{DBPath: db}
	var err error
	if s.PublicDomain, err = hostEnv(env, "FUNNEL_PUBLIC_DOMAIN", true); err != nil {
		return nil, err
	}
	if s.IngressHostname, err = hostEnv(env, "FUNNEL_INGRESS_HOSTNAME", false); err != nil {
		return nil, err
	}
	if err := s.readTags(env); err != nil {
		return nil, err
	}
	if s.TagOwners, err = ownersEnv(env); err != nil {
		return nil, err
	}
	if s.KeyUser = strings.TrimSpace(env.Getenv("FUNNEL_KEY_USER")); s.KeyUser == "" {
		return nil, errors.New("funnel enabled (FUNNEL_DB set) but FUNNEL_KEY_USER is not set")
	}
	if strings.ContainsFunc(s.KeyUser, isSpaceOrControl) {
		return nil, errors.New("FUNNEL_KEY_USER must not contain spaces")
	}
	admins := env.List("FUNNEL_ADMIN_USERS")
	if len(admins) == 0 {
		return nil, errors.New("funnel enabled (FUNNEL_DB set) but FUNNEL_ADMIN_USERS names no login")
	}
	s.Admins = authz.ParseAllowed(strings.Join(admins, ","))
	return s, nil
}

func (s *Settings) readTags(env core.Env) error {
	tags := []struct {
		dst        *string
		key, value string
	}{
		{&s.FunnelTag, "FUNNEL_TAG", defaultFunnelTag},
		{&s.NSPrefix, "FUNNEL_NS_TAG_PREFIX", defaultNSPrefix},
		{&s.IngressTag, "FUNNEL_INGRESS_TAG", defaultIngressTag},
	}
	for _, t := range tags {
		v := strings.TrimSpace(env.Default(t.key, t.value))
		if err := checkTag(t.key, v); err != nil {
			return err
		}
		*t.dst = v
	}
	if s.FunnelTag == s.NSPrefix || s.FunnelTag == s.IngressTag || s.NSPrefix == s.IngressTag {
		return errors.New("FUNNEL_TAG, FUNNEL_NS_TAG_PREFIX and FUNNEL_INGRESS_TAG must be different from each other")
	}
	if strings.HasPrefix(s.FunnelTag, s.NSPrefix) {
		return errors.New("FUNNEL_TAG must not start with FUNNEL_NS_TAG_PREFIX")
	}
	if strings.HasPrefix(s.IngressTag, s.NSPrefix) {
		return errors.New("FUNNEL_INGRESS_TAG must not start with FUNNEL_NS_TAG_PREFIX")
	}
	return nil
}

func checkTag(key, v string) error {
	if !strings.HasPrefix(v, "tag:") || len(v) == len("tag:") {
		return fmt.Errorf("%s must start with \"tag:\" and name the tag", key)
	}
	if len(v) > maxTagLen || strings.ContainsFunc(v, isSpaceOrControl) {
		return fmt.Errorf("%s must be at most %d characters with no spaces", key, maxTagLen)
	}
	return nil
}

func ownersEnv(env core.Env) ([]string, error) {
	owners := env.List("FUNNEL_TAG_OWNERS")
	if len(owners) == 0 {
		return nil, errors.New("funnel enabled (FUNNEL_DB set) but FUNNEL_TAG_OWNERS names no owner")
	}
	for _, o := range owners {
		if strings.ContainsFunc(o, isSpaceOrControl) {
			return nil, errors.New("FUNNEL_TAG_OWNERS entries must not contain spaces")
		}
	}
	return owners, nil
}

var hostLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// hostEnv reads a lower-case DNS name with at least two labels (no scheme, port or trailing dot).
func hostEnv(env core.Env, key string, required bool) (string, error) {
	v := strings.TrimSpace(env.Getenv(key))
	if v == "" {
		if required {
			return "", fmt.Errorf("funnel enabled (FUNNEL_DB set) but %s is not set", key)
		}
		return "", nil
	}
	labels := strings.Split(v, ".")
	ok := len(v) <= 253 && len(labels) >= 2
	for _, l := range labels {
		ok = ok && hostLabel.MatchString(l)
	}
	if !ok {
		return "", fmt.Errorf("%s must be a lower-case DNS name with at least two labels (no scheme, port or trailing dot)", key)
	}
	return v, nil
}
