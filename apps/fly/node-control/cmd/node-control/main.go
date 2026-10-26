// Command node-control serves the node control page and API on the tailnet.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/enroll"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/nodes"
	"example.com/node-control/internal/policy"
	"example.com/node-control/internal/tailnet"
	"example.com/node-control/internal/tasks"
)

const defaultNodesConfig = "/srv/app/nodes.json"

// trackedTimeout bounds the headscale lookup done on behalf of the tailnet status route.
const trackedTimeout = 10 * time.Second

// buildModules wires the modules in registration order: tailnet, nodes, enroll, policy, tasks. One cached
// headscale source feeds the tailnet, nodes, enroll and tasks modules, so a page load costs one fetch.
func buildModules(env core.Env, hc *http.Client) ([]core.Module, error) {
	cfg, err := nodes.LoadConfig(env.Default("NODES_CONFIG", defaultNodesConfig))
	if err != nil {
		return nil, err
	}
	src := headscale.Cached(&headscale.Client{
		BaseURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
		APIKey:  env.Getenv("HEADSCALE_API_KEY"),
		HTTP:    hc,
	}, headscale.DefaultTTL)

	providers := map[string]nodes.Provider{
		"yandex":   nodes.NewYandex(env, hc),
		"oci":      nodes.NewOCI(env, hc),
		"selectel": nodes.NewSelectel(env, hc),
		"fly":      nodes.NewFly(env, hc),
	}
	nm, ok := nodes.New(cfg, src, providers).(interface {
		core.Module
		Targets(context.Context) []nodes.Target
	})
	if !ok {
		return nil, errors.New("nodes module does not expose Targets")
	}

	tracked := func(ctx context.Context) map[string]bool {
		ctx, cancel := context.WithTimeout(ctx, trackedTimeout)
		defer cancel()
		return nodes.TailnetNames(cfg, nm.Targets(ctx))
	}
	return []core.Module{
		tailnet.New(src, tracked),
		nm,
		enroll.New(env, enroll.WithNodeSource(src)),
		policy.New(env, &headscale.Client{
			BaseURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
			APIKey:  env.Getenv("HEADSCALE_API_KEY"),
			HTTP:    hc,
		}),
		tasks.New(env, src),
	}, nil
}

// tailnetUpTimeout bounds waiting for the embedded node to join the tailnet.
const tailnetUpTimeout = 60 * time.Second

// joinTailnet runs up bounded by timeout; the error is what the process exits on.
func joinTailnet(ctx context.Context, up func(context.Context) error, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := up(ctx); err != nil {
		return fmt.Errorf("joining the tailnet failed (check TAILSCALE_AUTH_KEY and HEADSCALE_LOGIN_SERVER): %w", err)
	}
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "state-import" {
		if err := runStateImport(context.Background(), os.Args[2:], core.OSEnv(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "state-import failed:", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && (os.Args[1] == "enroll-import" || os.Args[1] == "enroll-export") {
		run := runEnrollImport
		if os.Args[1] == "enroll-export" {
			run = runEnrollExport
		}
		if err := run(os.Args[2:], core.OSEnv(), os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, os.Args[1]+" failed:", err)
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	env := core.OSEnv()
	mods, err := buildModules(env, &http.Client{Timeout: 30 * time.Second})
	if err != nil {
		slog.Error("node-control cannot start", "err", err)
		os.Exit(1)
	}

	// The state store is built (and a first-start import done) before the embedded node starts.
	store, closeStore, err := buildStateStore(ctx, env)
	if err != nil {
		slog.Error("node-control cannot start", "err", err)
		os.Exit(1)
	}
	defer closeStore()

	tcfg := core.TsnetConfigFromEnv(env)
	tcfg.Store = store
	tn := core.NewTsnet(tcfg)
	if err := joinTailnet(ctx, tn.Up, tailnetUpTimeout); err != nil {
		slog.Error("node-control cannot start", "err", err)
		tn.Close()
		closeStore()
		os.Exit(1)
	}
	err = core.Run(ctx, mods, env, tn)
	tn.Close() // writes the final state, so the store is closed after it
	closeStore()
	if err != nil {
		slog.Error("node-control stopped", "err", err)
		os.Exit(1)
	}
}
