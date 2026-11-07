package proxy

import (
	"net/http"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/routes"
)

// askPath is the TLS edge's on-demand certificate check (#127). Caddy calls it with ?domain=<host> before it gets a
// certificate for a name it has not seen. The answer is 200 only for <service>.<namespace>.<public domain> with a
// registered route, so a client cannot make the edge request certificates for arbitrary names.
const askPath = "/.well-known/headlamp/tls-ask"

func (h *handler) ask(w http.ResponseWriter, r *http.Request) {
	// Only the edge asks, with its own Host (for example funnel-ingress:8080). A request whose Host is a public name
	// came from the internet through the edge: it gets 404, so the path is not an oracle for which routes exist.
	if _, _, isRoute := ParseHost(r.Host, h.domain()); isRoute {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ns, service, ok := ParseHost(r.URL.Query().Get("domain"), h.domain())
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if _, found := h.table.Lookup(routes.Key{NS: ns, Service: service}); !found {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	w.WriteHeader(http.StatusOK)
}
