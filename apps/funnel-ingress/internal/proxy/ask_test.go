package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/config"
	"github.com/valentinalexeev/headlamp/apps/funnel-ingress/internal/routes"
)

func TestTLSAsk(t *testing.T) {
	table, err := routes.NewTable(nopPersister{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	table.SetStatic([]routes.Route{route("admin", "pdl", "100.64.0.9", 8080)})
	h := NewHandler(table, nil, func() string { return "example.com" }, config.Config{})
	ask := func(host, domain string) int {
		r := httptest.NewRequest("GET", askPath+"?domain="+domain, nil)
		r.Host = host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}
	for _, c := range []struct {
		host, domain string
		want         int
	}{
		{"funnel-ingress:8080", "admin.pdl.example.com", 200},
		{"funnel-ingress:8080", "ADMIN.pdl.example.com.", 200},
		{"funnel-ingress:8080", "other.pdl.example.com", 403}, // no route
		{"funnel-ingress:8080", "admin.nope.example.com", 403},
		{"funnel-ingress:8080", "pdl.example.com", 403},
		{"funnel-ingress:8080", "a.admin.pdl.example.com", 403},
		{"funnel-ingress:8080", "admin.pdl.example.org", 403},
		{"funnel-ingress:8080", "evil.com", 403},
		{"funnel-ingress:8080", "", 403},
		// from the internet through the edge: the public Host never reaches the check
		{"admin.pdl.example.com", "admin.pdl.example.com", 404},
	} {
		if got := ask(c.host, c.domain); got != c.want {
			t.Errorf("Host %q domain %q: %d, want %d", c.host, c.domain, got, c.want)
		}
	}
}
