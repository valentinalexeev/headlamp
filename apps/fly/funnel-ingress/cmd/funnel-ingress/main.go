// Command funnel-ingress exposes services of tailnet nodes on the public internet under stable names: a
// reverse proxy routes <service>.<namespace>.<public domain> to the registered node, nodes publish their
// own services through a tailnet-only API, and configuration is pulled from node-control.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"funnel-ingress/internal/api"
	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/config"
	"funnel-ingress/internal/controlsync"
	"funnel-ingress/internal/proxy"
	"funnel-ingress/internal/reconcile"
	"funnel-ingress/internal/routes"
	"funnel-ingress/internal/statekv"
	"funnel-ingress/internal/tsnode"
)

const (
	tailnetUpTimeout = 90 * time.Second
	shutdownTimeout  = 20 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Getenv)
	stop()
	if err != nil {
		// One clear line; Fly restarts the machine. Errors never contain the auth key, Redis URL or state key.
		fmt.Fprintln(os.Stderr, "funnel-ingress:", err)
		os.Exit(1)
	}
}

func logf(format string, args ...any) { slog.Info(fmt.Sprintf(format, args...)) }

// run starts the service and blocks until ctx is done. Every failure before the listeners are up is
// returned: nothing registers a new tailnet identity unless the state store opened.
func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := config.Load(getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	proxyPort, err := portOf(cfg.ProxyAddr)
	if err != nil {
		return fmt.Errorf("configuration: FUNNEL_PROXY_ADDR: %w", err)
	}

	// State first: with Redis down the process exits here, before the node could register a new identity.
	stores, err := statekv.Open(ctx, cfg)
	if err != nil {
		return fmt.Errorf("opening the state store: %w", err)
	}
	defer stores.Close()

	table, err := routes.NewTable(statekv.NewPersister(stores.KV, statekv.RoutesField), cfg.MaxRoutesPerNode)
	if err != nil {
		return err // a corrupt blob is never replaced by an empty table
	}
	ns := auth.NewNamespaces()
	applier := &controlsync.Applier{Table: table, NS: ns, Domain: new(atomic.Pointer[string]), ProxyPort: proxyPort, APIPort: cfg.APIPort}
	cache := controlsync.NewCache(statekv.NewBlob(stores.KV, statekv.ConfigField))
	// Serve from the cached configuration before the first fetch and before the listener opens.
	controlsync.ApplyCached(cache, applier, logf)

	// tsnet keeps some files under the state directory even when its state is in Redis; it must exist and be writable.
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("the state directory FUNNEL_STATE_DIR is not usable: %w", err)
	}
	node := tsnode.NewTSNet(cfg, stores.TSNet)
	defer node.Close()
	upCtx, cancel := context.WithTimeout(ctx, tailnetUpTimeout)
	err = node.Up(upCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("joining the tailnet failed (check TS_AUTHKEY and FUNNEL_CONTROL_URL): %w", err)
	}

	apiLn, err := node.ListenAPI(cfg.APIPort)
	if err != nil {
		return fmt.Errorf("listening for the registration API on the tailnet: %w", err)
	}
	publicLn, err := net.Listen("tcp", cfg.ProxyAddr)
	if err != nil {
		apiLn.Close()
		return fmt.Errorf("listening on %s: %w", cfg.ProxyAddr, err)
	}

	policy := auth.Policy{FunnelTag: cfg.FunnelTag, NSTagPrefix: cfg.NSTagPrefix, NS: ns}
	rec := &reconcile.Reconciler{Node: node, Table: table, Policy: policy, MissingChecks: cfg.MissingChecks, Interval: cfg.ReconcileInterval, Log: logf}

	apiSrv := &http.Server{
		Handler:           api.NewHandler(api.Deps{Node: node, Table: table, Policy: policy, APIPort: cfg.APIPort, ProxyPort: proxyPort}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
	}
	publicSrv := &http.Server{
		Handler: proxy.NewHandler(table, node.DialContext, func() string {
			if d := applier.Domain.Load(); d != nil {
				return *d
			}
			return ""
		}, cfg),
		// No read/write timeout: bodies and WebSocket sessions are bounded by the proxy's own limits.
		ReadHeaderTimeout: 10 * time.Second, IdleTimeout: cfg.IdleTimeout, MaxHeaderBytes: 64 << 10,
	}

	src, rep := syncEndpoints(cfg, node)
	state := func() controlsync.State { return observe(ctx, node, table, rec) }

	var wg sync.WaitGroup
	serve := func(name string, s *http.Server, ln net.Listener) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("server stopped", "server", name, "error", err)
			}
		}()
	}
	serve("api", apiSrv, apiLn)
	serve("public", publicSrv, publicLn)
	wg.Add(2)
	go func() { defer wg.Done(); rec.Run(ctx) }()
	go func() {
		defer wg.Done()
		controlsync.Run(ctx, src, rep, applier, cache, state, cfg.SyncInterval, logf)
	}()
	slog.Info("funnel-ingress running", "proxy", cfg.ProxyAddr, "api_port", cfg.APIPort)

	<-ctx.Done()
	sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer scancel()
	_ = publicSrv.Shutdown(sctx)
	_ = apiSrv.Shutdown(sctx)
	wg.Wait()
	return nil
}

// syncEndpoints picks the configuration source: node-control over the tailnet, or a local file (no report).
func syncEndpoints(cfg config.Config, node tsnode.Node) (controlsync.Source, controlsync.Reporter) {
	if cfg.ConfigFile != "" {
		return controlsync.NewFileSource(cfg.ConfigFile), nil
	}
	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: node.DialContext, ResponseHeaderTimeout: 10 * time.Second, DisableKeepAlives: true},
		// The config endpoint never redirects; following one could send the request somewhere else.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return controlsync.NewNodeControlSource(cfg.ConfigURL, client), controlsync.NewNodeControlReporter(cfg.ConfigURL, client)
}

// observe collects what the state report carries: peer-list status and the routes with their owners.
func observe(ctx context.Context, node tsnode.Node, table *routes.Table, rec *reconcile.Reconciler) controlsync.State {
	sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	snap, err := node.Snapshot(sctx)
	st := controlsync.State{SnapshotLoaded: err == nil && snap.Loaded, PeerCount: len(snap.Peers), LastReconcile: rec.LastTick()}
	byID := map[string]tsnode.Peer{}
	byTarget := map[string]tsnode.Peer{}
	for _, p := range snap.Peers {
		byID[p.NodeID] = p
		byTarget[p.Name] = p
		if p.Addr.IsValid() {
			byTarget[p.Addr.String()] = p
		}
	}
	for _, r := range table.All() {
		rs := controlsync.RouteState{Route: r}
		if p, ok := byID[r.NodeID]; ok && r.NodeID != "" {
			rs.OwnerName, rs.TargetOnline = p.Name, p.Online
		} else if p, ok := byTarget[r.Target]; ok && r.NodeID == "" {
			rs.TargetOnline = p.Online
		}
		st.Routes = append(st.Routes, rs)
	}
	return st
}

func portOf(addr string) (int, error) {
	_, p, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, errors.New("expected host:port")
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("port out of range")
	}
	return n, nil
}
