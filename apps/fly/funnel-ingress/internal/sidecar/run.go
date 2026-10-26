package sidecar

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"funnel-ingress/internal/config"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 60 * time.Second

	registerTimeout = 15 * time.Second
)

// shutdownBudget bounds the best-effort withdrawal of routes at shutdown (a variable so tests can shorten it).
var shutdownBudget = 5 * time.Second

// Run publishes svcs until ctx is done: it listens on every TailnetPort via listen, forwards each to its
// target, registers the services (retrying with backoff, re-asserting every cfg.ReregisterInterval), reports
// readiness through ready (true only while the last round registered every service), and on shutdown deletes
// its routes best effort within a bounded time. Registration failures, including 403 and 409, never end the
// process: the operator may fix a tag or free a name while the sidecar keeps retrying.
func Run(ctx context.Context, cfg config.SidecarConfig, svcs []Service, listen func(network, addr string) (net.Listener, error), reg Registrar, ready func(bool)) error {
	return run(ctx, cfg, svcs, listen, reg, ready, sleep, (&net.Dialer{Timeout: 10 * time.Second}).DialContext)
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func run(ctx context.Context, cfg config.SidecarConfig, svcs []Service, listen func(network, addr string) (net.Listener, error), reg Registrar, ready func(bool),
	wait func(context.Context, time.Duration) bool, dial func(context.Context, string, string) (net.Conn, error)) error {
	var lns []net.Listener
	for _, s := range svcs {
		ln, err := listen("tcp", fmt.Sprintf(":%d", s.TailnetPort))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("listening for %s on tailnet port %d: %w", s.Name, s.TailnetPort, err)
		}
		lns = append(lns, ln)
	}
	fctx, stopForward := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i, s := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			Forward(fctx, lns[i], s.Target, dial)
		}()
	}

	backoff := initialBackoff
	for ctx.Err() == nil {
		allOK := true
		for _, s := range svcs {
			rctx, cancel := context.WithTimeout(ctx, registerTimeout)
			err := reg.Put(rctx, cfg.NS, s.Name, s.TailnetPort)
			cancel()
			if err != nil && ctx.Err() == nil {
				allOK = false
				slog.Warn("registering failed", "ns", cfg.NS, "service", s.Name, "error", err)
			}
		}
		if ctx.Err() != nil {
			break
		}
		ready(allOK)
		d := cfg.ReregisterInterval
		if allOK {
			backoff = initialBackoff
		} else {
			d = backoff
			if backoff *= 2; backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
		if !wait(ctx, d) {
			break
		}
	}

	// Withdraw first so the ingress stops sending traffic, then stop forwarding.
	ready(false)
	dctx, cancel := context.WithTimeout(context.Background(), shutdownBudget)
	for _, s := range svcs {
		if err := reg.Delete(dctx, cfg.NS, s.Name); err != nil {
			slog.Warn("withdrawing failed", "ns", cfg.NS, "service", s.Name, "error", err)
		}
	}
	cancel()
	stopForward()
	wg.Wait()
	return nil
}
