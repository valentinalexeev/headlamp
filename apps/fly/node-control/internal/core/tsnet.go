package core

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"tailscale.com/ipn"
	"tailscale.com/tsnet"
)

// TsnetConfig configures the embedded tailscale node.
type TsnetConfig struct {
	ControlURL string
	Hostname   string
	AuthKey    string
	Dir        string
	Verbose    bool // TSNET_VERBOSE=1: also forward the backend's own logs
	// Store, when set, holds the node state instead of a file under Dir (Dir stays set: tsnet still
	// keeps other files there). Nil means the default file store.
	Store ipn.StateStore
}

// TsnetConfigFromEnv reads HEADSCALE_LOGIN_SERVER, TAILNET_HOSTNAME
// (default node-control) and TAILSCALE_AUTH_KEY; state lives in
// /var/lib/tailscale. TSNET_VERBOSE=1 turns on the backend's own (very
// chatty) logs; without it only user-facing messages such as the login URL
// are logged.
func TsnetConfigFromEnv(env Env) TsnetConfig {
	return TsnetConfig{
		ControlURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
		Hostname:   env.Default("TAILNET_HOSTNAME", "node-control"),
		AuthKey:    env.Getenv("TAILSCALE_AUTH_KEY"),
		Dir:        "/var/lib/tailscale",
		Verbose:    env.Getenv("TSNET_VERBOSE") == "1",
	}
}

// Tsnet is the production Tailnet backed by tailscale.com/tsnet.
type Tsnet struct {
	srv     *tsnet.Server
	authKey string
}

// NewTsnet builds (but does not start) the embedded node; it joins the
// tailnet on first Listen or Dial.
func NewTsnet(cfg TsnetConfig) *Tsnet {
	srv := &tsnet.Server{
		Hostname:   cfg.Hostname,
		AuthKey:    cfg.AuthKey,
		ControlURL: cfg.ControlURL,
		Dir:        cfg.Dir,
		Store:      cfg.Store,
		UserLogf:   func(format string, args ...any) { slog.Info(fmt.Sprintf("tailnet: "+format, args...)) },
	}
	if cfg.Verbose {
		srv.Logf = func(format string, args ...any) { slog.Info(fmt.Sprintf("tsnet: "+format, args...)) }
	}
	return &Tsnet{srv: srv, authKey: cfg.AuthKey}
}

// Up joins the tailnet and waits until the node is running, so a bad auth key
// or an unreachable control server fails at start instead of at first use.
func (t *Tsnet) Up(ctx context.Context) error {
	st, err := t.srv.Up(ctx)
	if err != nil {
		return upError(err, t.authKey)
	}
	slog.Info("tailnet up", "addresses", len(st.TailscaleIPs))
	return nil
}

// upError wraps err for logging with the auth key removed.
func upError(err error, authKey string) error {
	msg := err.Error()
	if authKey != "" {
		msg = strings.ReplaceAll(msg, authKey, "***")
	}
	return errors.New(msg)
}

func (t *Tsnet) ListenAdmin() (net.Listener, error)  { return t.srv.Listen("tcp", ":80") }
func (t *Tsnet) ListenPublic() (net.Listener, error) { return t.srv.Listen("tcp", ":8081") }

func (t *Tsnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	return t.srv.Dial(ctx, network, addr)
}

func (t *Tsnet) WhoIs(ctx context.Context, remoteAddr string) (string, []string, error) {
	lc, err := t.srv.LocalClient()
	if err != nil {
		return "", nil, err
	}
	who, err := lc.WhoIs(ctx, remoteAddr)
	if err != nil {
		return "", nil, err
	}
	var login string
	if who.UserProfile != nil {
		login = who.UserProfile.LoginName
	}
	var tags []string
	if who.Node != nil {
		tags = who.Node.Tags
	}
	return login, tags, nil
}

// Close shuts the embedded node down.
func (t *Tsnet) Close() error { return t.srv.Close() }
