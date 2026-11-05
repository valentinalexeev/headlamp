package sidecar

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/funnel-ingress/internal/config"
)

const (
	initialBackoff = time.Second
	maxBackoff     = 60 * time.Second

	registerTimeout = 15 * time.Second
)

// ErrUnreachable ends Run when the registration API gave no answer for cfg.UnreachableExit. The usual cause is
// that the control server removed this ephemeral node: tsnet does not log in again by itself, so only a new
// process (a pod restart) recovers.
var ErrUnreachable = errors.New("the registration API gave no answer for too long; exiting so the pod restarts with a new tailnet login")

// shutdownBudget bounds the best-effort withdrawal of routes at shutdown (a variable so tests can shorten it).
var shutdownBudget = 5 * time.Second

// Run publishes svcs until ctx is done: it listens on every TailnetPort via listen, forwards each to its
// target, registers the services (retrying with backoff, re-asserting every cfg.ReregisterInterval), reports
// readiness through ready (true only while the last round registered every service), and on shutdown deletes
// its routes best effort within a bounded time. Registration failures, including 403 and 409, never end the
// process: the operator may fix a tag or free a name while the sidecar keeps retrying. Only when the API gives
// no answer at all (a transport error, typically a timeout) for cfg.UnreachableExit does Run withdraw and return
// ErrUnreachable, so that the process exits and Kubernetes restarts it.
func Run(ctx context.Context, cfg config.SidecarConfig, svcs []Service, listen func(network, addr string) (net.Listener, error), reg Registrar, ready func(bool)) error {
	return run(ctx, cfg, svcs, listen, reg, ready, sleep, time.Now, (&net.Dialer{Timeout: 10 * time.Second}).DialContext)
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
	wait func(context.Context, time.Duration) bool, now func() time.Time, dial func(context.Context, string, string) (net.Conn, error)) error {
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
	lastAnswer := now() // last round in which the API answered every request (with any status)
	var result error
	for ctx.Err() == nil {
		allOK, allAnswered := true, true
		for _, s := range svcs {
			rctx, cancel := context.WithTimeout(ctx, registerTimeout)
			err := reg.Put(rctx, cfg.NS, s.Name, s.TailnetPort)
			cancel()
			if err != nil && ctx.Err() == nil {
				allOK = false
				var apiErr *APIError
				if !errors.As(err, &apiErr) {
					allAnswered = false
				}
				slog.Warn("registering failed", "ns", cfg.NS, "service", s.Name, "error", err)
			}
		}
		if ctx.Err() != nil {
			break
		}
		ready(allOK)
		if allAnswered {
			lastAnswer = now()
		} else if silent := now().Sub(lastAnswer); cfg.UnreachableExit > 0 && silent >= cfg.UnreachableExit {
			slog.Error("registration API unreachable, exiting", "ns", cfg.NS, "for", silent.Round(time.Second))
			result = ErrUnreachable
			break
		}
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
	return result
}
