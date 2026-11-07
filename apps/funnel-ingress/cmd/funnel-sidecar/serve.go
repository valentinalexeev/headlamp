package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/config"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/serveclient"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/sidecar"
)

// runServe is serve mode (#117): each port in FUNNEL_SERVICES listens on the tailnet with TLS and forwards plain TCP
// to its target in the pod. The certificate is for the service whose auth key the node joined with; node-control
// knows it and returns the name, so the sidecar names nothing. Nothing is published outside the tailnet.
func runServe(ctx context.Context, cfg config.SidecarConfig, svcs []sidecar.Service, listen func(network, addr string) (net.Listener, error),
	hc *http.Client, ready func(bool)) error {
	c := &serveclient.Client{URL: cfg.ServeURL, HTTP: hc}
	return serveWith(ctx, svcs, listen, c, ready, (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		func(f string, a ...any) { slog.Info(fmt.Sprintf(f, a...)) })
}

// serveWith is runServe with its dependencies passed in, for tests.
func serveWith(ctx context.Context, svcs []sidecar.Service, listen func(network, addr string) (net.Listener, error), c *serveclient.Client,
	ready func(bool), dial func(context.Context, string, string) (net.Conn, error), logf func(string, ...any)) error {
	tlsCfg := c.TLSConfig()
	var lns []net.Listener
	for _, s := range svcs {
		ln, err := listen("tcp", fmt.Sprintf(":%d", s.TailnetPort))
		if err != nil {
			for _, l := range lns {
				l.Close()
			}
			return fmt.Errorf("listening for %s on tailnet port %d: %w", s.Name, s.TailnetPort, err)
		}
		lns = append(lns, tls.NewListener(ln, tlsCfg))
	}
	var wg sync.WaitGroup
	for i, s := range svcs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sidecar.Forward(ctx, lns[i], s.Target, dial)
		}()
	}
	c.Keep(ctx, ready, logf)
	wg.Wait()
	return nil
}
