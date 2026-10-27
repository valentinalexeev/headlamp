package tasks

import (
	"context"
	"log/slog"
	"time"
)

// heartbeatEvery is the scheduler heartbeat interval: a dead-man's switch
// alerts when node-control stops pinging.
const heartbeatEvery = time.Hour

// heartbeatPinger is what the heartbeat needs from the notifier.
type heartbeatPinger interface {
	PingSecret(ctx context.Context, secretName string) string
}

// heartbeat pings the URL held by a secret once at start and then on every
// tick. It reads no Redis of its own (the notifier reads the secret from the
// store's memory per ping) and logs the outcome only when it changes.
type heartbeat struct {
	ping   func(context.Context) string
	every  time.Duration
	ticker func(time.Duration) (<-chan time.Time, func()) // replaced by tests
	log    *slog.Logger
}

func newHeartbeat(p heartbeatPinger, secret string, log *slog.Logger) *heartbeat {
	return &heartbeat{
		ping:  func(ctx context.Context) string { return p.PingSecret(ctx, secret) },
		every: heartbeatEvery,
		ticker: func(d time.Duration) (<-chan time.Time, func()) {
			t := time.NewTicker(d)
			return t.C, t.Stop
		},
		log: log,
	}
}

// run returns when ctx is done.
func (h *heartbeat) run(ctx context.Context) {
	c, stop := h.ticker(h.every)
	defer stop()
	last := ""
	for {
		if out := h.ping(ctx); out != last && ctx.Err() == nil {
			last = out
			// A missing secret is configuration, not an incident: info level.
			if out == "sent" || out == failedNoSecret {
				h.log.Info("ping_result", "task", heartbeatTask, "kind", "ping", "outcome", out)
			} else {
				h.log.Warn("ping_result", "task", heartbeatTask, "kind", "ping", "outcome", out)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-c:
		}
	}
}
