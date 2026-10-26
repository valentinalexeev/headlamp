// Package proxy is the public side of funnel-ingress: a reverse proxy that picks the backend from the Host
// header (<service>.<namespace>.<public domain>) and reaches it through the tailnet dialer.
package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"net/url"
	"strconv"
	"strings"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

// ParseHost extracts namespace and service from a Host header value. The host must be exactly two labels
// under publicDomain; it is lower-cased and may carry a port and one trailing dot. Anything else (an IP
// literal, the bare domain, extra labels, another domain, an empty value) is not ok.
func ParseHost(host, publicDomain string) (ns, service string, ok bool) {
	domain := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(publicDomain)), ".")
	if domain == "" || host == "" {
		return "", "", false
	}
	h := strings.ToLower(host)
	if strings.Contains(h, ":") {
		hh, _, err := net.SplitHostPort(h)
		if err != nil {
			return "", "", false
		}
		h = hh
	}
	h = strings.TrimSuffix(h, ".")
	if _, err := netip.ParseAddr(h); err == nil {
		return "", "", false
	}
	rest, found := strings.CutSuffix(h, "."+domain)
	if !found {
		return "", "", false
	}
	labels := strings.Split(rest, ".")
	if len(labels) != 2 || !routes.ValidName(labels[0]) || !routes.ValidName(labels[1]) {
		return "", "", false
	}
	return labels[1], labels[0], true
}

type variant struct {
	https, insecure bool
}

type handler struct {
	table   *routes.Table
	domain  func() string
	cfg     config.Config
	proxies map[variant]*httputil.ReverseProxy
}

// NewHandler returns the public handler. domain returns the current public domain from the configuration
// source; while it is empty (nothing configured yet) every host is a bare 404. GET /healthz answers 200 "ok"
// for hosts that are not service hosts (Fly's health checks), and nothing else is served besides routes.
func NewHandler(table *routes.Table, dial func(ctx context.Context, network, addr string) (net.Conn, error),
	domain func() string, cfg config.Config) http.Handler {
	h := &handler{table: table, domain: domain, cfg: cfg, proxies: map[variant]*httputil.ReverseProxy{}}
	for _, v := range []variant{{false, false}, {true, false}, {true, true}} {
		h.proxies[v] = h.newProxy(v, dial)
	}
	return h
}

func (h *handler) newProxy(v variant, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *httputil.ReverseProxy {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			ctx, cancel := context.WithTimeout(ctx, h.cfg.ConnectTimeout)
			defer cancel()
			c, err := dial(ctx, network, addr)
			if err != nil {
				return nil, &dialError{err}
			}
			return c, nil
		},
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: v.insecure}, //nolint:gosec // explicit per-route opt-in
		ResponseHeaderTimeout: h.cfg.RequestTimeout,
		IdleConnTimeout:       h.cfg.IdleTimeout,
		MaxIdleConnsPerHost:   4,
		ForceAttemptHTTP2:     false,
	}
	return &httputil.ReverseProxy{
		Transport: tr,
		Rewrite: func(pr *httputil.ProxyRequest) {
			rt := pr.In.Context().Value(routeKey{}).(routes.Route)
			scheme := "http"
			if v.https {
				scheme = "https"
			}
			pr.SetURL(&url.URL{Scheme: scheme, Host: net.JoinHostPort(rt.Target, strconv.Itoa(rt.Port))})
			pr.Out.Host = pr.In.Host // the backend sees the name the client asked for
			scrubForwarding(pr.Out.Header)
			pr.Out.Header.Set("X-Forwarded-For", clientIP(pr.In))
			pr.Out.Header.Set("X-Forwarded-Host", pr.In.Host)
			pr.Out.Header.Set("X-Forwarded-Proto", forwardedProto(pr.In))
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var tooBig *http.MaxBytesError
			switch {
			case errors.As(err, &tooBig):
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			case errors.Is(r.Context().Err(), context.Canceled):
				return // the client went away
			}
			rt, _ := r.Context().Value(routeKey{}).(routes.Route)
			slog.Warn("backend error", "ns", rt.NS, "service", rt.Service, "error", err)
			var de *dialError
			if errors.As(err, &de) || errors.Is(err, context.DeadlineExceeded) {
				w.WriteHeader(http.StatusServiceUnavailable) // known route, backend unreachable or offline
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
}

type routeKey struct{}

// dialError marks a failure to reach the backend at all (offline, filtered by the ACL, timed out).
type dialError struct{ err error }

func (e *dialError) Error() string { return e.err.Error() }
func (e *dialError) Unwrap() error { return e.err }

// scrubForwarding removes every header a client could use to claim another address or origin.
func scrubForwarding(h http.Header) {
	for k := range h {
		l := strings.ToLower(k)
		if strings.HasPrefix(l, "x-forwarded-") || l == "forwarded" || l == "x-real-ip" || l == "true-client-ip" || l == "fly-client-ip" ||
			l == "x-client-ip" || l == "cf-connecting-ip" || l == "fly-forwarded-proto" || l == "fly-forwarded-port" || l == "fly-forwarded-ssl" {
			h.Del(k)
		}
	}
}

// clientIP is the address the trusted edge (Fly) reports; the machine accepts traffic only through it. It
// falls back to the connecting peer.
func clientIP(r *http.Request) string {
	if ip, err := netip.ParseAddr(strings.TrimSpace(r.Header.Get("Fly-Client-IP"))); err == nil {
		return ip.Unmap().String()
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func forwardedProto(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	if p := strings.ToLower(r.Header.Get("X-Forwarded-Proto")); p == "http" || p == "https" {
		return p // set by the trusted edge, which overwrites a client-supplied value
	}
	return "http"
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ns, service, ok := ParseHost(r.Host, h.domain())
	if !ok {
		if r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		return
	}
	rt, found := h.table.Lookup(routes.Key{NS: ns, Service: service})
	if !found {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if r.ContentLength > h.cfg.MaxBodyBytes {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, h.cfg.MaxBodyBytes)
	r = r.WithContext(context.WithValue(r.Context(), routeKey{}, rt))
	h.proxies[variant{https: rt.Scheme == "https", insecure: rt.Scheme == "https" && rt.InsecureSkipVerify}].ServeHTTP(w, r)
}
