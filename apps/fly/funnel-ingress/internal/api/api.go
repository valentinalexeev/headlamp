// Package api is the registration API of funnel-ingress. It listens on the tailnet only. The caller's
// identity always comes from the control plane (WhoIs on the connecting address), never from the request,
// and the backend of a route is always the caller's own tailnet address.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"time"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/routes"
	"funnel-ingress/internal/tsnode"
)

// Deps are the collaborators of the handler.
type Deps struct {
	Node      tsnode.Node
	Table     *routes.Table
	Policy    auth.Policy
	APIPort   int // a route may not point at the API listener ...
	ProxyPort int // ... or at the public proxy listener
}

const maxBody = 64 << 10

type putBody struct {
	NS     string `json:"ns"`
	Port   int    `json:"port"`
	Scheme string `json:"scheme"`
}

type routeView struct {
	NS      string `json:"ns"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	Scheme  string `json:"scheme"`
}

type handler struct{ d Deps }

// NewHandler returns the API. There are no admin routes: operators work in node-control.
func NewHandler(d Deps) http.Handler {
	h := &handler{d}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/routes/{service}", h.route)
	mux.HandleFunc("/v1/routes", h.list)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNotFound) })
	return headers(mux)
}

func headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// caller resolves the identity of the connecting node. On failure it has answered 401 with an empty body.
func (h *handler) caller(w http.ResponseWriter, r *http.Request) (tsnode.Identity, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	id, err := h.d.Node.WhoIs(ctx, r.RemoteAddr)
	if err != nil || id.NodeID == "" || !id.Addr.IsValid() {
		w.WriteHeader(http.StatusUnauthorized)
		return tsnode.Identity{}, false
	}
	return id, true
}

func (h *handler) route(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		h.put(w, r)
	case http.MethodDelete:
		h.del(w, r)
	default:
		w.Header().Set("Allow", "PUT, DELETE")
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *handler) put(w http.ResponseWriter, r *http.Request) {
	id, ok := h.caller(w, r)
	if !ok {
		return
	}
	if !isJSON(r) {
		w.WriteHeader(http.StatusUnsupportedMediaType)
		return
	}
	var b putBody
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	// Unknown fields are ignored on purpose: a body naming "target" must not change where traffic goes.
	if err := dec.Decode(&b); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	service := r.PathValue("service")
	if !routes.ValidName(service) || b.Port < 1 || b.Port > 65535 || (b.NS != "" && !routes.ValidName(b.NS)) ||
		(b.Scheme != "" && b.Scheme != "http" && b.Scheme != "https") ||
		b.Port == h.d.APIPort || b.Port == h.d.ProxyPort {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	if self := h.d.Node.Self(); self.Addr.IsValid() && id.Addr == self.Addr {
		w.WriteHeader(http.StatusForbidden) // the proxy must not route to itself
		return
	}
	ns, err := h.d.Policy.Authorize(id.Tags, b.NS, b.Port)
	switch {
	case errors.Is(err, auth.ErrNamespaceRequired):
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	case err != nil:
		w.WriteHeader(http.StatusForbidden)
		return
	}
	err = h.d.Table.Put(routes.Route{
		Key:    routes.Key{NS: ns, Service: service},
		Target: id.Addr.String(), // always the caller's own address
		Port:   b.Port,
		Scheme: b.Scheme,
		NodeID: id.NodeID,
	})
	w.WriteHeader(statusFor(err, http.StatusNoContent))
}

func (h *handler) del(w http.ResponseWriter, r *http.Request) {
	id, ok := h.caller(w, r)
	if !ok {
		return
	}
	mine := h.d.Policy.NamespacesOf(id.Tags)
	if len(mine) == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	ns := r.URL.Query().Get("ns")
	switch {
	case ns == "" && len(mine) == 1:
		ns = mine[0]
	case ns == "" || !routes.ValidName(ns) || !routes.ValidName(r.PathValue("service")):
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	err := h.d.Table.Delete(routes.Key{NS: ns, Service: r.PathValue("service")}, id.NodeID)
	w.WriteHeader(statusFor(err, http.StatusNoContent))
}

func (h *handler) list(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	id, ok := h.caller(w, r)
	if !ok {
		return
	}
	if len(h.d.Policy.NamespacesOf(id.Tags)) == 0 {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	out := struct {
		Routes []routeView `json:"routes"`
	}{Routes: []routeView{}}
	for _, rt := range h.d.Table.ByNode(id.NodeID) {
		out.Routes = append(out.Routes, routeView{NS: rt.NS, Service: rt.Service, Port: rt.Port, Scheme: rt.Scheme})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func isJSON(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	return err == nil && mt == "application/json"
}

// statusFor maps table errors to status codes; the body stays empty.
func statusFor(err error, ok int) int {
	switch {
	case err == nil:
		return ok
	case errors.Is(err, routes.ErrBlocked):
		return http.StatusForbidden
	case errors.Is(err, routes.ErrStaticConflict), errors.Is(err, routes.ErrOwnedByOther):
		return http.StatusConflict
	case errors.Is(err, routes.ErrLimit):
		return http.StatusTooManyRequests
	case errors.Is(err, routes.ErrBadName):
		return http.StatusUnprocessableEntity
	default: // ErrPersist and anything unexpected: refused, nothing changed
		return http.StatusServiceUnavailable
	}
}
