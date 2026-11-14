// Package config reads the process settings of funnel-ingress from the environment. Namespaces, their
// allowed ports, the public domain and manual routes are not process settings: they come from the
// configuration source (internal/controlsync).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/clientip"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/statestore"
)

// PortRange is an inclusive range of TCP ports.
type PortRange struct{ Lo, Hi int }

// ValidateRanges checks that every range satisfies 1 <= Lo <= Hi <= 65535.
func ValidateRanges(rs []PortRange) error {
	for _, r := range rs {
		if r.Lo < 1 || r.Hi > 65535 || r.Lo > r.Hi {
			return fmt.Errorf("invalid port range %d-%d", r.Lo, r.Hi)
		}
	}
	return nil
}

// Config holds the process settings.
type Config struct {
	FunnelTag, NSTagPrefix string
	ConfigURL, ConfigFile  string // exactly one; ConfigURL is the node-control base URL
	SyncInterval           time.Duration

	ControlURL, AuthKey, Hostname, StateDir string // StateDir is used only for file state (no REDIS_URL)
	RedisURL, RedisKey, StateKey            string // RedisKey defaults to "<FLY_APP_NAME or Hostname>:tsnet"; StateKey is base64, 32 bytes
	AllowPlaintext                          bool
	ImportFrom                              string

	ProxyAddr string
	APIPort   int

	MissingChecks     int
	ReconcileInterval time.Duration
	MaxRoutesPerNode  int
	MaxBodyBytes      int64

	ConnectTimeout, IdleTimeout, RequestTimeout time.Duration

	ClientIP *clientip.Resolver // where the client address comes from (FUNNEL_CLIENT_IP_HEADER, FUNNEL_TRUSTED_PROXIES)

	// HSTS is the Strict-Transport-Security value sent on responses for routed hosts that
	// the client reached over HTTPS (FUNNEL_HSTS). Empty (the default) sends nothing and
	// leaves a backend's own header alone.
	HSTS string
}

// Load builds a Config from getenv (os.Getenv in production). Error texts name settings, never values:
// the auth key, the Redis URL and the state key must not leak through them.
func Load(getenv func(string) string) (Config, error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	str := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	integer := func(name string, def, min, max int) int {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return def
		}
		n, err := strconv.Atoi(v)
		if err != nil || n < min || n > max {
			fail("%s must be an integer between %d and %d", name, min, max)
			return def
		}
		return n
	}
	duration := func(name string, def, min time.Duration) time.Duration {
		v := strings.TrimSpace(getenv(name))
		if v == "" {
			return def
		}
		d, err := time.ParseDuration(v)
		if err != nil || d < min {
			fail("%s must be a duration of at least %s", name, min)
			return def
		}
		return d
	}

	c := Config{
		FunnelTag:         str("FUNNEL_TAG", "tag:funnel"),
		NSTagPrefix:       str("FUNNEL_NS_TAG_PREFIX", "tag:ns-"),
		ConfigURL:         str("FUNNEL_CONFIG_URL", ""),
		ConfigFile:        str("FUNNEL_CONFIG_FILE", ""),
		SyncInterval:      duration("FUNNEL_SYNC_INTERVAL", 30*time.Second, 5*time.Second),
		ControlURL:        str("FUNNEL_CONTROL_URL", ""),
		AuthKey:           strings.TrimSpace(getenv("TS_AUTHKEY")),
		Hostname:          str("FUNNEL_HOSTNAME", "funnel-ingress"),
		StateDir:          str("FUNNEL_STATE_DIR", "/var/lib/tailscale"),
		RedisURL:          strings.TrimSpace(getenv("REDIS_URL")),
		StateKey:          strings.TrimSpace(getenv("FUNNEL_STATE_KEY")),
		AllowPlaintext:    getenv("STATE_ALLOW_PLAINTEXT") == "1",
		ImportFrom:        str("STATE_IMPORT_FROM", ""),
		ProxyAddr:         str("FUNNEL_PROXY_ADDR", ":8080"),
		APIPort:           integer("FUNNEL_API_PORT", 9100, 1, 65535),
		MissingChecks:     integer("FUNNEL_MISSING_CHECKS", 3, 1, 1000),
		ReconcileInterval: duration("FUNNEL_RECONCILE_INTERVAL", 60*time.Second, time.Second),
		MaxRoutesPerNode:  integer("FUNNEL_MAX_ROUTES_PER_NODE", 20, 1, 10000),
		MaxBodyBytes:      int64(integer("FUNNEL_MAX_BODY_BYTES", 32<<20, 1, 1<<30)),
		ConnectTimeout:    duration("FUNNEL_CONNECT_TIMEOUT", 10*time.Second, time.Millisecond),
		IdleTimeout:       duration("FUNNEL_IDLE_TIMEOUT", 120*time.Second, time.Millisecond),
		RequestTimeout:    duration("FUNNEL_REQUEST_TIMEOUT", 300*time.Second, time.Millisecond),
	}

	if !strings.HasPrefix(c.FunnelTag, "tag:") || len(c.FunnelTag) == len("tag:") {
		fail(`FUNNEL_TAG must start with "tag:"`)
	}
	if !strings.HasPrefix(c.NSTagPrefix, "tag:") {
		fail(`FUNNEL_NS_TAG_PREFIX must start with "tag:"`)
	}
	switch {
	case c.ConfigURL == "" && c.ConfigFile == "":
		fail("set exactly one of FUNNEL_CONFIG_URL and FUNNEL_CONFIG_FILE")
	case c.ConfigURL != "" && c.ConfigFile != "":
		fail("set exactly one of FUNNEL_CONFIG_URL and FUNNEL_CONFIG_FILE, not both")
	}
	if c.ConfigURL != "" && !httpURL(c.ConfigURL) {
		fail("FUNNEL_CONFIG_URL must be an http(s) URL")
	}
	if c.ControlURL != "" && !httpURL(c.ControlURL) {
		fail("FUNNEL_CONTROL_URL must be an http(s) URL")
	}
	if c.AuthKey == "" {
		fail("TS_AUTHKEY is required")
	}

	if c.RedisURL != "" {
		if !strings.HasPrefix(c.RedisURL, "redis://") {
			fail("REDIS_URL must be a plain redis:// URL (the Redis host is only reachable on Fly's private network)")
		}
		if c.StateKey == "" && !c.AllowPlaintext {
			fail("REDIS_URL is set but FUNNEL_STATE_KEY is not: tailscale state holds private keys and Redis is shared, " +
				"so it must be encrypted (openssl rand -base64 32), or set STATE_ALLOW_PLAINTEXT=1")
		}
	}
	if err := statestore.ValidateStateKey(c.StateKey); err != nil {
		errs = append(errs, err)
	}
	key, err := statestore.KeyFor(getenv)
	if err != nil {
		errs = append(errs, err)
	}
	c.RedisKey = key

	if c.ClientIP, err = clientip.New(getenv("FUNNEL_CLIENT_IP_HEADER"), getenv("FUNNEL_TRUSTED_PROXIES")); err != nil {
		fail("FUNNEL_CLIENT_IP_HEADER or FUNNEL_TRUSTED_PROXIES: %v", err)
	}

	if v := strings.TrimSpace(getenv("FUNNEL_HSTS")); v != "" {
		m := hstsValue.FindStringSubmatch(v)
		switch {
		case m == nil:
			fail(`FUNNEL_HSTS must be "max-age=<seconds>", ` +
				`optionally followed by "; includeSubDomains"`)
		case len(m[1]) > 9 || mustAtoi(m[1]) > hstsMaxAge:
			fail("FUNNEL_HSTS max-age must be at most %d (two years)", hstsMaxAge)
		default:
			c.HSTS = v
		}
	}

	if len(errs) > 0 {
		return Config{}, errors.Join(errs...)
	}
	return c, nil
}

// hstsValue accepts only max-age and includeSubDomains. preload is refused on purpose: it
// is a commitment that cannot be taken back quickly, and it must not be switched on by a
// typo in an environment variable. max-age is capped at two years (hstsMaxAge): a browser
// keeps the policy that long, so a larger value is almost certainly a typo.
var hstsValue = regexp.MustCompile(`^max-age=([0-9]{1,10})(; ?includeSubDomains)?$`)

const hstsMaxAge = 63072000

// mustAtoi parses digits that a regexp already checked; at most 9 digits fit in an int.
func mustAtoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// httpURL reports whether s parses as an absolute http or https URL with a host. It never returns the
// parser's error, which would echo the input.
func httpURL(s string) bool {
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// SidecarConfig holds the settings of funnel-sidecar, the Kubernetes registration sidecar.
type SidecarConfig struct {
	NS                                                 string        // namespace the sidecar publishes into
	Services                                           string        // "name=port[@host:port],..." (parsed by the sidecar package)
	APIURL                                             string        // funnel-ingress registration API, e.g. http://funnel-ingress:9100
	ControlURL, AuthKey, Hostname, StateDir, ReadyAddr string        // ControlURL optional; StateDir is scratch space for the ephemeral node
	ReregisterInterval                                 time.Duration // how often registrations are re-asserted
	UnreachableExit                                    time.Duration // exit after the API gave no answer this long; 0 never exits
	// Serve mode (#117): a tailnet-only name with TLS instead of a public funnel route. ServeURL is node-control on
	// the tailnet. The name is not configured here: node-control knows it from the auth key the node joined with.
	ServeURL string
	// ServeCertFile and ServeKeyFile (#334): in serve mode, take the certificate from these
	// files (a cert-manager Secret) instead of node-control. ServeURL is then not needed.
	ServeCertFile, ServeKeyFile string
}

// Serve reports whether the sidecar runs in serve mode.
func (c SidecarConfig) Serve() bool { return c.ServeURL != "" || c.ServeCertFile != "" }

// LoadSidecar builds a SidecarConfig from getenv. Like Load, errors name settings, never values.
func LoadSidecar(getenv func(string) string) (SidecarConfig, error) {
	var errs []error
	fail := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	str := func(name, def string) string {
		if v := strings.TrimSpace(getenv(name)); v != "" {
			return v
		}
		return def
	}
	c := SidecarConfig{
		NS:         str("FUNNEL_NS", ""),
		Services:   str("FUNNEL_SERVICES", ""),
		APIURL:     strings.TrimRight(str("FUNNEL_API_URL", ""), "/"),
		ControlURL: str("FUNNEL_CONTROL_URL", ""),
		AuthKey:    strings.TrimSpace(getenv("TS_AUTHKEY")),
		Hostname:   str("FUNNEL_HOSTNAME", str("HOSTNAME", "funnel-sidecar")),
		StateDir:   str("FUNNEL_STATE_DIR", "/tmp/funnel-sidecar"),
		ReadyAddr:  str("FUNNEL_READY_ADDR", ":8081"),
	}
	c.ReregisterInterval = 5 * time.Minute
	if v := strings.TrimSpace(getenv("FUNNEL_REREGISTER_INTERVAL")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 10*time.Second {
			fail("FUNNEL_REREGISTER_INTERVAL must be a duration of at least 10s")
		} else {
			c.ReregisterInterval = d
		}
	}
	c.UnreachableExit = 5 * time.Minute
	if v := strings.TrimSpace(getenv("FUNNEL_UNREACHABLE_EXIT")); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || (d != 0 && d < time.Minute) {
			fail("FUNNEL_UNREACHABLE_EXIT must be 0 or a duration of at least 1m")
		} else {
			c.UnreachableExit = d
		}
	}
	c.ServeURL = strings.TrimRight(str("FUNNEL_SERVE_URL", ""), "/")
	c.ServeCertFile = str("FUNNEL_SERVE_CERT_FILE", "")
	c.ServeKeyFile = str("FUNNEL_SERVE_KEY_FILE", "")
	if (c.ServeCertFile == "") != (c.ServeKeyFile == "") {
		fail("set both FUNNEL_SERVE_CERT_FILE and FUNNEL_SERVE_KEY_FILE, or neither")
	}
	if c.Services == "" {
		fail("FUNNEL_SERVICES is required")
	}
	if c.AuthKey == "" {
		fail("TS_AUTHKEY is required")
	}
	switch {
	case c.Serve():
		// The service and its namespace come from the auth key, so FUNNEL_NS is not needed.
		if c.ServeURL != "" && !httpURL(c.ServeURL) {
			fail("FUNNEL_SERVE_URL must be an http(s) URL")
		}
		if c.APIURL != "" {
			fail("set FUNNEL_SERVE_URL or FUNNEL_SERVE_CERT_FILE (tailnet-only name) " +
				"or FUNNEL_API_URL (public funnel), not both")
		}
		if getenv("FUNNEL_SERVE_NAMES") != "" {
			fail("FUNNEL_SERVE_NAMES is gone: the name comes from the auth key of the service (see Serve mode)")
		}
	case c.NS == "":
		fail("FUNNEL_NS is required")
	case c.APIURL == "":
		fail("FUNNEL_API_URL is required")
	case !httpURL(c.APIURL):
		fail("FUNNEL_API_URL must be an http(s) URL")
	}
	if c.ControlURL != "" && !httpURL(c.ControlURL) {
		fail("FUNNEL_CONTROL_URL must be an http(s) URL")
	}
	if len(errs) > 0 {
		return SidecarConfig{}, errors.Join(errs...)
	}
	return c, nil
}
