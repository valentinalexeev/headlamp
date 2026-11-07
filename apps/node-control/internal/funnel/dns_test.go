package funnel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func TestDNSFromEnv(t *testing.T) {
	s := &Settings{PublicDomain: "f.example.com", IngressHostname: "edge.example.net"}
	if d, err := dnsFromEnv(core.MapEnv(nil), s, http.DefaultClient); d != nil || err != nil {
		t.Fatalf("unset must mean manual: %v %v", d, err)
	}
	for name, c := range map[string]struct {
		env  map[string]string
		s    Settings
		want string
	}{
		"no target":    {map[string]string{"FUNNEL_DNS_PROVIDER": "cloudflare"}, Settings{PublicDomain: "f.example.com"}, "FUNNEL_INGRESS_HOSTNAME"},
		"fly":          {map[string]string{"FUNNEL_DNS_PROVIDER": "cloudflare"}, Settings{PublicDomain: "f.example.com", IngressHostname: "app.fly.dev"}, "Fly.io"},
		"no zone":      {map[string]string{"FUNNEL_DNS_PROVIDER": "cloudflare"}, *s, "FUNNEL_DNS_ZONE"},
		"no token":     {map[string]string{"FUNNEL_DNS_PROVIDER": "cloudflare", "FUNNEL_DNS_ZONE": "Z"}, *s, "FUNNEL_DNS_CLOUDFLARE_TOKEN"},
		"bad provider": {map[string]string{"FUNNEL_DNS_PROVIDER": "bind", "FUNNEL_DNS_ZONE": "Z"}, *s, "route53 or cloudflare"},
	} {
		if _, err := dnsFromEnv(core.MapEnv(c.env), &c.s, http.DefaultClient); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	d, err := dnsFromEnv(core.MapEnv(map[string]string{"FUNNEL_DNS_PROVIDER": "route53", "FUNNEL_DNS_ZONE": "Z9"}), s, http.DefaultClient)
	if err != nil || d.p.Name() != "route53" || d.rec.Name != "*.f.example.com" || d.rec.Type != "CNAME" {
		t.Fatalf("route53: %+v %v", d, err)
	}
}

// The checklist turns into a status line when node-control manages the record, and shows the error when it fails.
func TestChecklistWithManagedDNS(t *testing.T) {
	ok := true
	cf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !ok {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"success":false,"errors":[{"message":"Authentication error"}]}`))
			return
		}
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"success":true,"result":[]}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&map[string]any{})
		_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
	}))
	defer cf.Close()
	s := &Settings{PublicDomain: "f.example.com", IngressHostname: "203.0.113.7"}
	d, err := dnsFromEnv(core.MapEnv(map[string]string{"FUNNEL_DNS_PROVIDER": "cloudflare", "FUNNEL_DNS_ZONE": "Z1",
		"FUNNEL_DNS_CLOUDFLARE_TOKEN": "t", "FUNNEL_DNS_CLOUDFLARE_URL": cf.URL}), s, cf.Client())
	if err != nil {
		t.Fatal(err)
	}
	m := &Module{s: s, dns: d}
	d.sync(context.Background())
	cl := m.checklistFor("team")
	if cl.Auto == nil || !cl.Auto.InSync || len(cl.Commands) != 0 || cl.Records[0]["type"] != "A" || !strings.Contains(cl.Notes[0], "Nothing to do") {
		t.Fatalf("in sync: %+v", cl)
	}
	ok = false
	d.sync(context.Background())
	cl = m.checklistFor("team")
	if cl.Auto.InSync || !strings.Contains(cl.Notes[0], "Authentication error") {
		t.Fatalf("failing: %+v", cl)
	}
}
