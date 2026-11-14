package tailnetdns

import (
	"context"
	_ "embed"
	"fmt"
	"net/http"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

// The DNS page (#285): one page in the console shell, built like the Serve page. The shell adds
// the navigation, the shared stylesheet and hl.js; the page script draws everything from
// GET /dns/state.

//go:embed static/dns.html
var pageHTML []byte

//go:embed static/dns.js
var pageJS []byte

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any script
// or style that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", ui.PageCSP)
		_, _ = w.Write(body)
	}
}

func (m *Module) canView(login string) bool {
	return authz.Can(login, authz.P(authz.FeatureDNS, authz.LevelView), authz.Allowed{})
}

// navOrder places the page after Policy in the configure group.
const navOrder = 55

// Nav puts the page in the top bar, in the configure group. An inactive module shows nothing.
func (m *Module) Nav() []core.NavItem {
	if m.st == nil {
		return nil
	}
	return []core.NavItem{{Label: "DNS", Path: "/dns", Order: navOrder, Group: "configure",
		Icon: "link", Source: "tailnetdns", Allow: m.canView}}
}

// Search lists the records and zones; each opens the DNS page.
func (m *Module) Search(_ context.Context, login string) ([]core.SearchItem, error) {
	if m.st == nil {
		return nil, nil
	}
	if !m.canView(login) {
		return nil, core.ErrNotVisible
	}
	recs, err := m.st.Records()
	if err != nil {
		return nil, fmt.Errorf("tailnet DNS store: %w", err)
	}
	zones, err := m.st.Zones()
	if err != nil {
		return nil, fmt.Errorf("tailnet DNS store: %w", err)
	}
	out := make([]core.SearchItem, 0, len(recs)+len(zones))
	for _, z := range zones {
		out = append(out, core.SearchItem{Label: z.Name, Kind: "DNS zone", Path: "/dns"})
	}
	for _, r := range recs {
		kind := "DNS " + r.Type + " record"
		out = append(out, core.SearchItem{Label: r.Name, Kind: kind, Path: "/dns"})
	}
	return out, nil
}
