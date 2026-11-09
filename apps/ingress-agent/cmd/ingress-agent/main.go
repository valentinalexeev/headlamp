// Command ingress-agent holds the opaque credentials of a bridge and owns its xray config (#133). It is configured
// by environment only: the same variables as the former Python agent, plus AGENT_LISTEN for the address.
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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/valentinalexeev/headlamp/apps/ingress-agent/internal/agent"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("ingress-agent cannot start: " + err.Error())
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token := os.Getenv("AGENT_TOKEN")
	if token == "" {
		return errors.New("AGENT_TOKEN is required")
	}
	info, err := agent.LoadInfo(os.Getenv) // names the variable, never a value
	if err != nil {
		return err
	}
	state := env("AGENT_STATE_DIR", "/var/lib/ingress-agent")
	runner := &agent.Runner{Bin: env("XRAY_BIN", "/usr/local/bin/xray"), Unit: env("XRAY_UNIT", "xray"),
		API: env("XRAY_API", "127.0.0.1:10085"), RestartCmd: strings.Fields(os.Getenv("XRAY_RESTART_CMD"))}
	a := &agent.Applier{StatePath: filepath.Join(state, "credentials.json"),
		BasePath: env("XRAY_BASE_CONFIG", "/etc/xray/config.base.json"), ConfigPath: env("XRAY_CONFIG", "/etc/xray/config.json"),
		Runner: runner, DirectGeoIP: agent.ParseList(os.Getenv("INGRESS_DIRECT_GEOIP")),
		DirectGeoSite: agent.ParseList(os.Getenv("INGRESS_DIRECT_GEOSITE")),
		ExitDNS:       agent.ParseList(os.Getenv("INGRESS_EXIT_DNS")), Log: log}
	listen := env("AGENT_LISTEN", "127.0.0.1:9000")
	if _, _, err := net.SplitHostPort(listen); err != nil {
		return fmt.Errorf("AGENT_LISTEN must be host:port")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	// Restore before serving: the first answer already reflects the persisted set. A failure is logged and
	// serving goes on; the next push repairs it.
	if err := a.Restore(ctx); err != nil {
		log.Error("startup restore failed")
	}
	go agent.SweepLoop(ctx, a, time.Minute)
	srv := &http.Server{Handler: (&agent.Server{A: a, Token: token, Info: info}).Handler(),
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("ingress-agent listening", "addr", ln.Addr().String())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
