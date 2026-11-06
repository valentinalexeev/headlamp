// Command funnel-sidecar publishes services of a Kubernetes pod through funnel-ingress. It joins the tailnet
// as an ephemeral, tagged node (no privileges, no TUN device, no tailscaled), forwards TCP from its tailnet
// address to the pod, and registers the services with the ingress.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"tailscale.com/tsnet"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/config"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/sidecar"
)

const (
	tailnetUpTimeout   = 90 * time.Second
	tailnetWatchPeriod = 30 * time.Second
)

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

	// /readyz answers 200 only while every service is registered; the body also gives the age of the last
	// successful registration round.
	var ready readiness
	mux := http.NewServeMux()
	mux.Handle("/readyz", ready.handler(time.Now))
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

	lc, err := srv.LocalClient()
	if err != nil {
		return fmt.Errorf("opening the tailnet node's local client: %w", err)
	}
	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go watchTailnet(watchCtx, func(c context.Context) (string, []string, error) {
		st, err := lc.StatusWithoutPeers(c)
		if err != nil {
			return "", nil, err
		}
		return st.BackendState, st.Health, nil
	}, tailnetWatchPeriod, func(healthy bool, msg string, args ...any) {
		if healthy {
			slog.Info(msg, args...)
		} else {
			slog.Warn(msg, args...)
		}
	})

	client := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{DialContext: srv.Dial, DisableKeepAlives: true},
	}
	slog.Info("funnel-sidecar running", "ns", cfg.NS, "services", len(svcs))
	return sidecar.Run(ctx, cfg, svcs, srv.Listen, sidecar.NewHTTPRegistrar(cfg.APIURL, client), func(ok bool) { ready.set(time.Now(), ok) })
}

// watchTailnet reads the node's backend state and health warnings every period and logs them when they change.
// tsnet's own log is too verbose to keep, so without this a node that the control server removed (state
// NeedsLogin, or a "cannot reach the coordination server" warning) leaves no trace in the pod log.
func watchTailnet(ctx context.Context, status func(context.Context) (string, []string, error), period time.Duration, logf func(healthy bool, msg string, args ...any)) {
	var lastState, lastErr string
	var lastHealth []string
	first := true
	for {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		state, health, err := status(c)
		cancel()
		switch {
		case err != nil && ctx.Err() == nil:
			if err.Error() != lastErr {
				logf(false, "tailnet: reading the node status failed", "error", err)
				lastErr = err.Error()
			}
		case err == nil:
			lastErr = ""
			health = slices.Clone(health)
			slices.Sort(health)
			if first || state != lastState || !slices.Equal(health, lastHealth) {
				logf(state == "Running" && len(health) == 0, "tailnet: node status", "state", state, "health", strings.Join(health, "; "))
				lastState, lastHealth, first = state, health, false
			}
		}
		t := time.NewTimer(period)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// readiness holds the result of the last registration round and the time of the last successful one.
type readiness struct {
	mu     sync.Mutex
	ready  bool
	lastOK time.Time
}

func (r *readiness) set(at time.Time, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ready = ok
	if ok {
		r.lastOK = at
	}
}

// handler answers 200 while ready and 503 otherwise, with a JSON body:
// {"ready":bool,"last_registered":RFC 3339 time,"age_seconds":int}; the last two are absent before the first
// successful round. The age lets a probe or an operator see how long registration has been failing.
func (r *readiness) handler(now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		r.mu.Lock()
		ready, lastOK := r.ready, r.lastOK
		r.mu.Unlock()
		body := struct {
			Ready          bool    `json:"ready"`
			LastRegistered *string `json:"last_registered,omitempty"`
			AgeSeconds     *int64  `json:"age_seconds,omitempty"`
		}{Ready: ready}
		if !lastOK.IsZero() {
			ts := lastOK.UTC().Format(time.RFC3339)
			age := int64(now().Sub(lastOK) / time.Second)
			body.LastRegistered, body.AgeSeconds = &ts, &age
		}
		w.Header().Set("Content-Type", "application/json")
		if ready {
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		json.NewEncoder(w).Encode(body)
	})
}
