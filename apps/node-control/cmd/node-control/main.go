// Command node-control serves the node control page and API on the tailnet.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/enroll"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/nodes"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/overview"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policy"
	servemod "github.com/valentinalexeev/headlamp/apps/node-control/internal/serve"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/tailnet"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/tasks"
)

const defaultNodesConfig = "/srv/app/nodes.json"

// trackedTimeout bounds the headscale lookup done on behalf of the tailnet status route.
const trackedTimeout = 10 * time.Second

// buildModules wires the modules in registration order: tailnet, nodes, enroll, policy, funnel, tasks, overview (funnel submits its ACL changes through the policy module). One cached
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
		"yandex": nodes.NewYandex(env, hc),
		"oci":    nodes.NewOCI(env, hc),
		// One OpenStack implementation (#101). A provider name names an organisation: "selectel" is the default cloud
		// (SELECTEL_*), and "openstack" is the same provider under a generic name. Further clouds are declared in
		// OPENSTACK_CLOUDS and picked with a node block's "cloud".
		// Deprecated: the provider name "selectel" (tag:cloud-selectel, a "selectel" node block) is an alias of
		// "openstack" and must be removed by 1.0.0.
		"selectel":  nodes.NewOpenStack(env, hc),
		"openstack": nodes.NewOpenStack(env, hc),
		"aws":       nodes.NewAWS(env, hc),
		"fly":       nodes.NewFly(env, hc),
	}
	// Nodes takes the audit log from enrolment, and enrolment asks Nodes what is stopped: hence nm is declared first.
	var nm interface {
		core.Module
		Targets(context.Context) []nodes.Target
		StoppedNames(context.Context) map[string]bool
	}
	// The audit log lives in the enrolment store, which opens at Register, after nodes.New: hence a function.
	enr := enroll.New(env, enroll.WithNodeSource(src), enroll.WithStoppedNodes(func(ctx context.Context) map[string]bool {
		return nm.StoppedNames(ctx)
	}))
	audit := func() core.AuditLog {
		if p, ok := enr.(interface{ AuditLog() core.AuditLog }); ok {
			return p.AuditLog()
		}
		return nil
	}
	var ok bool
	nm, ok = nodes.New(cfg, src, providers, nodes.WithAudit(audit)).(interface {
		core.Module
		Targets(context.Context) []nodes.Target
		StoppedNames(context.Context) map[string]bool
	})
	if !ok {
		return nil, errors.New("nodes module does not expose Targets and StoppedNames")
	}

	tracked := func(ctx context.Context) map[string]bool {
		ctx, cancel := context.WithTimeout(ctx, trackedTimeout)
		defer cancel()
		return nodes.TailnetNames(cfg, nm.Targets(ctx))
	}
	hsClient := &headscale.Client{
		BaseURL: env.Getenv("HEADSCALE_LOGIN_SERVER"),
		APIKey:  env.Getenv("HEADSCALE_API_KEY"),
		HTTP:    hc,
	}
	pol := policy.New(env, hsClient)
	mods := []core.Module{
		tailnet.New(src, tracked, tailnet.WithKeyExpiry(hsClient.APIKeyExpiry), tailnet.WithKeyViewers(keyViewers(env))),
		nm,
		enr,
		pol,
		funnel.New(env, hsClient, func() funnel.PolicyService {
			// Only an active console has a service; a typed nil must not become a non-nil interface.
			if p, ok := pol.(interface{ Service() *policy.Service }); ok {
				if svc := p.Service(); svc != nil {
					return svc
				}
			}
			return nil
		}),
		tasks.New(env, src),
		servemod.New(env, src),
	}
	// Overview is last: it counts the navigation of everything before it.
	others := slices.Clone(mods)
	return append(mods, overview.New(func() []core.Module { return others })), nil
}

// keyViewers is who sees the headscale API key warning: anyone on an admin list of a module. With no list at all
// there is nobody to limit it to, so it returns nil and anyone who can reach the console sees it.
func keyViewers(env core.Env) func(string) bool {
	// With an admin policy, whoever may view the overview sees the warning. Without one, the old rule applies.
	if p := authz.Default(); p != nil && !p.Empty() {
		return func(login string) bool {
			return p.Can(authz.Subject{Login: login}, authz.P(authz.FeatureOverview, authz.LevelView))
		}
	}
	lists := strings.Join([]string{env.Getenv("POLICY_ADMIN_USERS"), env.Getenv("FUNNEL_ADMIN_USERS"), env.Getenv("TASKS_ADMIN_USERS")}, ",")
	if strings.Trim(lists, ", \t") == "" {
		return nil
	}
	return authz.ParseAllowed(lists).Has
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
		run := func(args []string, env core.Env, out io.Writer) error {
			return runEnrollImport(args, env, out, os.Stdin)
		}
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
	// The admin policy is installed before the modules register, so their start checks and gates see it.
	pol, err := buildPolicy(env)
	if err != nil {
		slog.Error("node-control cannot start", "err", err)
		os.Exit(1)
	}
	authz.SetDefault(pol)
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
	err = core.Run(ctx, mods, env, tn, core.WithAuthorizer(newAuthorizer(pol, tn, slog.Default())))
	tn.Close() // writes the final state, so the store is closed after it
	closeStore()
	if err != nil {
		slog.Error("node-control stopped", "err", err)
		os.Exit(1)
	}
}
