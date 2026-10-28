// Command funnel-sidecar publishes services of a Kubernetes pod through funnel-ingress. It joins the tailnet
// as an ephemeral, tagged node (no privileges, no TUN device, no tailscaled), forwards TCP from its tailnet
// address to the pod, and registers the services with the ingress.
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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/sidecar"
)

const tailnetUpTimeout = 90 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	err := run(ctx, os.Getenv)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "funnel-sidecar:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	cfg, err := config.LoadSidecar(getenv)
	if err != nil {
		return fmt.Errorf("configuration: %w", err)
	}
	svcs, err := sidecar.ParseServices(cfg.Services)
	if err != nil {
		return fmt.Errorf("configuration: FUNNEL_SERVICES: %w", err)
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("creating the state directory: %w", err)
	}

	srv := &tsnet.Server{
		Hostname: cfg.Hostname, AuthKey: cfg.AuthKey, ControlURL: cfg.ControlURL, Dir: cfg.StateDir,
		Ephemeral: true, // headscale removes the node (and so the route) when the pod disappears
		UserLogf:  func(format string, args ...any) { slog.Info(fmt.Sprintf("tailnet: "+format, args...)) },
	}
	upCtx, cancel := context.WithTimeout(ctx, tailnetUpTimeout)
	_, err = srv.Up(upCtx)
	cancel()
	if err != nil {
		// No deferred Close before this point: closing a tsnet server whose start failed panics inside tsnet.
		msg := strings.ReplaceAll(err.Error(), cfg.AuthKey, "***")
		return fmt.Errorf("joining the tailnet failed (check TS_AUTHKEY and FUNNEL_CONTROL_URL): %s", msg)
	}
	defer srv.Close()

	// /readyz answers 200 only while every service is registered, and nothing else.
	var isReady atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if isReady.Load() {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	readySrv := &http.Server{Addr: cfg.ReadyAddr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	readyLn, err := net.Listen("tcp", cfg.ReadyAddr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", cfg.ReadyAddr, err)
	}
	go func() {
		if err := readySrv.Serve(readyLn); err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("readiness server stopped", "error", err)
		}
	}()
	defer readySrv.Close()

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: srv.Dial, DisableKeepAlives: true},
	}
	slog.Info("funnel-sidecar running", "ns", cfg.NS, "services", len(svcs))
	return sidecar.Run(ctx, cfg, svcs, srv.Listen, sidecar.NewHTTPRegistrar(cfg.APIURL, client), isReady.Store)
}
