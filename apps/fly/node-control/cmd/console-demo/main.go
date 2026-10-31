// Command console-demo serves the admin console on loopback with invented data, so the pages can be
// looked at and checked without a tailnet, a headscale server or any provider account.
//
//	go run ./cmd/console-demo -scenario attention|clear|minimal|partial
//
// Everything it talks to is a local fake: a headscale API, an enrolment agent, a task runner, a
// provider, Redis (miniredis) and temp-dir databases. It listens on loopback only, refuses any other
// address and is not part of the container image.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"example.com/node-control/internal/core"
)

const defaultAddr = "127.0.0.1:8099"

type options struct {
	addr     string
	scenario string
}

func main() {
	var o options
	flag.StringVar(&o.addr, "addr", defaultAddr, "loopback address to serve the console on")
	flag.StringVar(&o.scenario, "scenario", "attention", "one of: attention, clear, minimal, partial")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})))

	err := serve(ctx, o, func(admin string) {
		fmt.Printf("console demo (%s) ready: http://%s/\n", o.scenario, admin)
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "console-demo:", err)
		os.Exit(1)
	}
}

// loopbackOnly accepts an address whose host is a loopback IP or "localhost".
func loopbackOnly(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-addr must be host:port: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	return errors.New("-addr must be a loopback address (127.0.0.1, [::1] or localhost): the demo has no authentication")
}

// serve builds the scenario, runs the console until ctx ends and calls ready with the admin address
// once the scenario data is in place.
func serve(ctx context.Context, o options, ready func(admin string)) error {
	if err := loopbackOnly(o.addr); err != nil {
		return err
	}
	sc, ok := scenarios[o.scenario]
	if !ok {
		return fmt.Errorf("unknown scenario %q (want attention, clear, minimal or partial)", o.scenario)
	}
	world, err := newWorld(sc)
	if err != nil {
		return err
	}
	defer world.close()

	tn := &demoTailnet{addr: o.addr, world: world, adminUp: make(chan string, 1)}
	tn.login.Store("alice") // the seeding below acts as an administrator
	mods, env, err := world.modules()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	runDone := make(chan error, 1)
	go func() { runDone <- core.Run(ctx, mods, env, tn) }()

	select {
	case admin := <-tn.adminUp:
		go func() {
			if err := world.seed(ctx, admin); err != nil {
				fmt.Fprintln(os.Stderr, "console-demo: seeding stopped early:", err)
			}
			tn.login.Store(sc.login)
			if ready != nil {
				ready(admin)
			}
		}()
	case err := <-runDone:
		return err
	}
	err = <-runDone
	return err
}

// demoTailnet is a core.Tailnet on loopback. Dial maps the invented tailnet addresses to the local
// fakes and refuses everything else, so nothing leaves the machine.
type demoTailnet struct {
	addr    string
	world   *world
	login   atomic.Value // string: who WhoIs reports
	adminUp chan string

	mu     sync.Mutex
	public net.Listener
}

func (t *demoTailnet) ListenAdmin() (net.Listener, error) {
	ln, err := net.Listen("tcp", t.addr)
	if err != nil {
		return nil, err
	}
	t.adminUp <- ln.Addr().String()
	return ln, nil
}

// ListenPublic binds an ephemeral loopback port: the public enrolment pages are not part of the demo.
func (t *demoTailnet) ListenPublic() (net.Listener, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	t.mu.Lock()
	t.public = ln
	t.mu.Unlock()
	return ln, nil
}

func (t *demoTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	target, err := t.world.route(addr)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	d.Timeout = 3 * time.Second
	return d.DialContext(ctx, network, target)
}

func (t *demoTailnet) WhoIs(context.Context, string) (string, []string, error) {
	l, _ := t.login.Load().(string)
	return l, nil, nil
}
