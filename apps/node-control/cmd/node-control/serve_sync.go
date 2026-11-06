package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	servemod "github.com/valentinalexeev/headlamp/apps/node-control/internal/serve"
)

// runServeSync is "node-control serve-sync": the records-sync agent of the serve module (#117). It runs next to
// headscale, joins the tailnet as its own node (tag it SERVE_SYNC_TAG, default tag:headscale), pulls the record list
// from node-control and writes headscale's dns.extra_records_path file.
//
// Environment: SERVE_SYNC_URL (node-control on the tailnet, for example http://node-control), SERVE_RECORDS_PATH
// (the file headscale reads), SERVE_BASE_DOMAIN, SERVE_SYNC_SECONDS (default 15), and for the tailnet node
// HEADSCALE_LOGIN_SERVER, SERVE_SYNC_AUTH_KEY, SERVE_SYNC_HOSTNAME (default headscale-sync) and SERVE_SYNC_STATE_DIR
// (default /data/serve-sync, on the headscale volume, so the node keeps its identity).
func runServeSync(ctx context.Context, env core.Env) error {
	s, cfg, err := serveSyncConfig(env)
	if err != nil {
		return err
	}
	tn := core.NewTsnet(cfg)
	defer tn.Close()
	if err := joinTailnet(ctx, tn.Up, tailnetUpTimeout); err != nil {
		return err
	}
	s.Client = &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{DialContext: tn.Dial}}
	return s.Run(ctx)
}

// serveSyncConfig reads and checks the environment, before anything joins the tailnet.
func serveSyncConfig(env core.Env) (*servemod.Syncer, core.TsnetConfig, error) {
	every := 15 * time.Second
	if v := env.Getenv("SERVE_SYNC_SECONDS"); v != "" {
		d, err := time.ParseDuration(v + "s")
		if err != nil || d < 5*time.Second || d > time.Hour {
			return nil, core.TsnetConfig{}, errors.New("SERVE_SYNC_SECONDS must be a whole number from 5 to 3600")
		}
		every = d
	}
	s := &servemod.Syncer{
		URL:        env.Getenv("SERVE_SYNC_URL"),
		Path:       env.Getenv("SERVE_RECORDS_PATH"),
		BaseDomain: env.Getenv("SERVE_BASE_DOMAIN"),
		Every:      every,
		Log:        slog.Default(),
	}
	if s.URL == "" || s.Path == "" || s.BaseDomain == "" {
		return nil, core.TsnetConfig{}, errors.New("serve-sync needs SERVE_SYNC_URL, SERVE_RECORDS_PATH and SERVE_BASE_DOMAIN")
	}
	cfg := core.TsnetConfig{
		ControlURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
		Hostname:   env.Default("SERVE_SYNC_HOSTNAME", "headscale-sync"),
		AuthKey:    env.Getenv("SERVE_SYNC_AUTH_KEY"),
		Dir:        env.Default("SERVE_SYNC_STATE_DIR", "/data/serve-sync"),
		Verbose:    env.Getenv("TSNET_VERBOSE") == "1",
	}
	if cfg.ControlURL == "" {
		return nil, core.TsnetConfig{}, errors.New("serve-sync needs HEADSCALE_LOGIN_SERVER")
	}
	return s, cfg, nil
}
