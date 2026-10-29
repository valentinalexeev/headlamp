// Command task-runner serves the runner API on a node. Logs carry run ids,
// states and durations only, never scripts, params, secrets or the token.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"example.com/task-runner/internal/runner"
)

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	token, image := os.Getenv("RUNNER_TOKEN"), os.Getenv("RUNNER_IMAGE")
	if token == "" || image == "" {
		log.Error("RUNNER_TOKEN and RUNNER_IMAGE are required")
		os.Exit(2)
	}
	pids, err := strconv.Atoi(env("RUNNER_PIDS_LIMIT", "512"))
	if err != nil {
		log.Error("RUNNER_PIDS_LIMIT is not a number")
		os.Exit(2)
	}
	sessPort, err := strconv.Atoi(env("RUNNER_SESSION_PORT", "6080"))
	if err != nil {
		log.Error("RUNNER_SESSION_PORT is not a number")
		os.Exit(2)
	}
	srv, err := runner.New(runner.Config{
		Token:        token,
		Image:        image,
		StateDir:     env("RUNNER_STATE_DIR", "/var/lib/task-runner"),
		ContainerBin: env("RUNNER_CONTAINER_BIN", "podman"),
		Memory:       env("RUNNER_MEMORY", "1g"),
		PidsLimit:    pids,
		SessionBind:  os.Getenv("RUNNER_SESSION_BIND"),
		SessionPort:  sessPort,
		Logger:       log,
	})
	if err != nil {
		log.Error("init failed", "err", err.Error())
		os.Exit(1)
	}

	addr := env("RUNNER_LISTEN", "127.0.0.1:9100")
	hs := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		srv.Shutdown(sctx) // cancel the active run or session first, then stop serving
		_ = hs.Shutdown(sctx)
	}()
	log.Info("listening", "addr", addr, "version", runner.Version)
	if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve failed", "err", err.Error())
		os.Exit(1)
	}
}
