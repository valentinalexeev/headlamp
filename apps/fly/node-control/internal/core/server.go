package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"path"
	"strings"
	"time"
)

// hostGuard rejects requests whose Host header (port stripped) matches none of
// allowed, with HTTP 400. An empty allowed list disables the guard. Patterns
// follow Starlette's TrustedHostMiddleware: "*" allows any host; "*.example.com"
// matches every host that ends in ".example.com" (not the bare "example.com",
// list that too if needed); anything else is an exact, case-insensitive match.
// A malformed wildcard (a "*" anywhere but the leading "*.") matches nothing.
func hostGuard(allowed []string, next http.Handler) http.Handler {
	if len(allowed) == 0 {
		return next
	}
	exact := make(map[string]bool, len(allowed))
	var suffixes []string
	for _, h := range allowed {
		h = strings.ToLower(h)
		switch {
		case h == "*":
			return next
		case strings.HasPrefix(h, "*.") && !strings.Contains(h[1:], "*"):
			suffixes = append(suffixes, h[1:])
		default:
			exact[h] = true
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		} else {
			host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
		}
		host = strings.ToLower(host)
		ok := exact[host]
		for _, suf := range suffixes {
			if strings.HasSuffix(host, suf) {
				ok = true
			}
		}
		if !ok {
			http.Error(w, "Invalid host header", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// cleanPathGuard answers an empty 404 to any request whose path ServeMux would redirect to its
// cleaned form (double slashes, dot segments). The public listener answers every unknown path with
// the same empty 404, so a 301 must not distinguish those paths.
func cleanPathGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, p := range []string{r.URL.Path, r.URL.EscapedPath()} {
			if p == "" {
				continue
			}
			c := path.Clean(p)
			if strings.HasSuffix(p, "/") && c != "/" {
				c += "/"
			}
			if c != p {
				w.WriteHeader(http.StatusNotFound)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// newHTTPServer applies connection timeouts. WriteTimeout is generous because
// providers can take 10-20 s to answer; the public (enrolment) server also
// bounds reading the whole request.
func newHTTPServer(h http.Handler, public bool) *http.Server {
	s := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	if public {
		s.ReadTimeout = 30 * time.Second
	}
	return s
}

// EnrolmentEnabled reports whether enrolment is switched on: ENROLL_DB (SQLite on a volume) or ENROLL_REDIS=1 (Redis).
// The core uses it to decide whether a public listener exists, so the enrolment module and the core always agree.
func EnrolmentEnabled(env Env) bool {
	return env.Getenv("ENROLL_DB") != "" || env.Getenv("ENROLL_REDIS") == "1"
}

// shutdownTimeout bounds graceful shutdown; a var so tests can shorten it.
var shutdownTimeout = 5 * time.Second

// Run registers mods, opens listeners, starts modules and serves until ctx
// is cancelled or a module fails to start. Shutdown is graceful.
func Run(ctx context.Context, mods []Module, env Env, tn Tailnet) error {
	log := slog.Default()

	// The enrolment module owns its database; the core only decides whether a
	// public listener exists.
	var public *http.ServeMux
	if EnrolmentEnabled(env) {
		public = http.NewServeMux()
	}

	admin := http.NewServeMux()
	admin.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})

	routes := &Routes{Admin: admin, Public: public, Env: env, Tailnet: tn, Log: log}

	// Stop runs on every return path from here on, after the servers are
	// down (the explicit shutdown below precedes the return). Modules must
	// tolerate Stop without Start.
	defer func() {
		stctx, stcancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer stcancel()
		for _, m := range mods {
			if s, ok := m.(Stopper); ok {
				if err := s.Stop(stctx); err != nil {
					log.Warn("stop module", "module", m.Name(), "err", err)
				}
			}
		}
	}()
	for _, m := range mods {
		if err := m.Register(routes); err != nil {
			return fmt.Errorf("register %s: %w", m.Name(), err)
		}
	}

	// Only modules that registered contribute entries; a disabled module reports none.
	navItems, err := CollectNav(mods)
	if err != nil {
		return err
	}

	// Modules start before any listener exists, so no request can reach a
	// module that has not started. A failed Start leaves nothing open.
	mctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for _, m := range mods {
		if err := m.Start(mctx); err != nil {
			return fmt.Errorf("start %s: %w", m.Name(), err)
		}
	}

	// ENROLL_ALLOWED_HOSTS guards the admin listener only; the public one is
	// reached through an ingress proxy under other Host values.
	allowed := env.List("ENROLL_ALLOWED_HOSTS")
	type srv struct {
		name string
		ln   net.Listener
		s    *http.Server
	}
	var servers []*srv
	closeAll := func() {
		for _, s := range servers {
			s.ln.Close()
		}
	}
	add := func(name string, h http.Handler, listen func() (net.Listener, error)) error {
		ln, err := listen()
		if err != nil {
			return fmt.Errorf("listen %s: %w", name, err)
		}
		servers = append(servers, &srv{name, ln, newHTTPServer(h, name == "public")})
		return nil
	}
	if err := add("admin", hostGuard(allowed, NavMiddleware(navItems, admin)), tn.ListenAdmin); err != nil {
		return err
	}
	if public != nil {
		if err := add("public", cleanPathGuard(public), tn.ListenPublic); err != nil {
			closeAll()
			return err
		}
	}

	errc := make(chan error, len(servers))
	for _, s := range servers {
		go func(s *srv) {
			if err := s.s.Serve(s.ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errc <- fmt.Errorf("serve %s: %w", s.name, err)
			}
		}(s)
	}

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-errc:
	}

	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer scancel()
	for _, s := range servers {
		if err := s.s.Shutdown(sctx); err != nil {
			log.Warn("shutdown", "server", s.name, "err", err)
			s.s.Close()
		}
	}
	return runErr
}
