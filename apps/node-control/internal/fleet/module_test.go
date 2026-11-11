package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/enroll"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

type nodes []headscale.Node

func (n nodes) Nodes(context.Context) ([]headscale.Node, error) { return n, nil }

type hsVer string

func (h hsVer) Version(context.Context) (string, error) {
	if h == "" {
		return "", errors.New("headscale version: the server did not answer")
	}
	return string(h), nil
}

// tn is a tailnet that knows peer versions by address.
type tn struct {
	core.Tailnet
	v map[string]string
}

func (t tn) PeerVersion(_ context.Context, ip string) (string, error) {
	if v, ok := t.v[ip]; ok {
		return v, nil
	}
	return "", errors.New("no peer")
}

func (tn) ListenAdmin() (net.Listener, error) { return nil, errors.New("x") }

func TestReport(t *testing.T) {
	yes := true
	m := New(Deps{
		Nodes: nodes{
			{Name: "relay-1", GivenName: "relay-1", Online: true, IPAddresses: []string{"fd7a::1", "100.64.0.1"}, Tags: []string{"tag:relay"}},
			{Name: "exit-a", Online: false, IPAddresses: []string{"100.64.0.2"}},
			{Name: "phone", IPAddresses: []string{"100.64.0.3"}},
			{Name: "new", IPAddresses: []string{"100.64.0.4"}},
		},
		Headscale: hsVer("v0.29.7"),
		Bridges: func(context.Context) []enroll.IngressView {
			return []enroll.IngressView{{Name: "br-1", Source: "discovered", Online: &yes, AgentVersion: "0.16.0+abc1234", XrayVersion: "25.10.15"},
				{Name: "old", Source: "configured"}}
		},
		TailscalePin: "1.104.5",
	})
	mux := core.NewMux()
	if err := m.Register(&core.Routes{Admin: mux, Tailnet: tn{v: map[string]string{
		"100.64.0.1": "1.104.5-t0123abcd-g4567", "100.64.0.2": "1.96.4", "100.64.0.4": "1.106.0"}}}); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet.json", nil))
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err, rec.Body.String())
	}
	if r.Headscale != "v0.29.7" || r.TailscalePin != "1.104.5" || r.NodeControl == "" {
		t.Errorf("head %+v", r)
	}
	want := map[string][2]string{"relay-1": {"1.104.5", "current"}, "exit-a": {"1.96.4", "older"},
		"phone": {"", "unknown"}, "new": {"1.106.0", "newer"}}
	if len(r.Nodes) != 4 {
		t.Fatalf("nodes %+v", r.Nodes)
	}
	for _, n := range r.Nodes {
		if w := want[n.Name]; n.Tailscale != w[0] || n.Status != w[1] {
			t.Errorf("%s: %q %q, want %v", n.Name, n.Tailscale, n.Status, w)
		}
	}
	if len(r.Bridges) != 2 || r.Bridges[0].Agent != "0.16.0+abc1234" || r.Bridges[0].Xray != "25.10.15" || r.Bridges[1].Agent != "" {
		t.Errorf("bridges %+v", r.Bridges)
	}
}

func TestReportWithoutSources(t *testing.T) {
	m := New(Deps{Headscale: hsVer("")})
	mux := core.NewMux()
	_ = m.Register(&core.Routes{Admin: mux})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet.json", nil))
	var r Report
	_ = json.Unmarshal(rec.Body.Bytes(), &r)
	if rec.Code != 200 || r.HeadscaleError == "" || r.Nodes == nil || r.Bridges == nil {
		t.Errorf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestCompare(t *testing.T) {
	for _, c := range [][3]string{{"1.104.5", "1.104.5", "current"}, {"1.9.0", "1.10.0", "older"}, {"2.0.0", "1.99.9", "newer"},
		{"", "1.0.0", "unknown"}, {"1.0", "1.0.0", "unknown"}} {
		if got := compare(c[0], c[1]); got != c[2] {
			t.Errorf("%v: %s", c, got)
		}
	}
}

func TestFleetSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/fleet_selftest.js", "../ui/static/hl.js",
		"static/fleet.js").CombinedOutput()
	if err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
}

func TestProblems(t *testing.T) {
	no, yes := false, true
	m := New(Deps{
		Nodes: nodes{
			{Name: "relay-1", IPAddresses: []string{"100.64.0.1"}, Tags: []string{"tag:relay"}},
			{Name: "phone", IPAddresses: []string{"100.64.0.2"}},
		},
		Headscale: hsVer("v0.29.7"),
		Bridges: func(context.Context) []enroll.IngressView {
			return []enroll.IngressView{
				{Name: "down", Source: "discovered", Active: true, Online: &no, AgentVersion: "0.16.0"},
				{Name: "behind", Source: "discovered", Active: true, Online: &yes,
					Sync: &enroll.SyncSnapshot{Pending: true}, AgentVersion: "0.16.0"},
				{Name: "old", Source: "discovered", Active: true, Online: &yes,
					Sync: &enroll.SyncSnapshot{State: "ok"}},
				{Name: "waiting", Source: "discovered", Online: &no},
			}
		},
		Funnel: func() (funnel.IngressStatus, bool) {
			return funnel.IngressStatus{Stale: true, ReportedAt: time.Unix(0, 0)}, true
		},
		TailscalePin: "1.104.5",
	})
	mux := core.NewMux()
	_ = m.Register(&core.Routes{Admin: mux, Tailnet: tn{v: map[string]string{
		"100.64.0.1": "1.96.0", "100.64.0.2": "1.90.0"}}})
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet.json", nil))
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"relay-1: tailscale 1.96.0 is older than the pin 1.104.5",
		"bridge down: offline",
		"bridge behind: credentials not in sync",
		"funnel ingress: no report since 1970-01-01T00:00:00Z",
	}
	if !slices.Equal(r.Problems, want) {
		t.Errorf("problems\n got %q\nwant %q", r.Problems, want)
	}
	if !slices.Equal(r.Notes, []string{"bridge down: reports no agent-api version (before 0.18.0)",
		"bridge behind: reports no agent-api version (before 0.18.0)",
		"bridge old: reports no agent-api version (before 0.18.0)",
		"funnel ingress: reports no funnel-api version (before 0.18.0)"}) {
		t.Errorf("notes %q", r.Notes)
	}
	if r.Funnel == nil || !r.Funnel.Stale {
		t.Errorf("funnel %+v", r.Funnel)
	}
}

func TestPageAndNav(t *testing.T) {
	m := New(Deps{})
	mux := core.NewMux()
	_ = m.Register(&core.Routes{Admin: mux})
	for path, ct := range map[string]string{"/fleet": "text/html", "/fleet/fleet.js": "text/javascript"} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), ct) ||
			rec.Header().Get("Content-Security-Policy") == "" {
			t.Errorf("%s: %d %v", path, rec.Code, rec.Header())
		}
	}
	if nav := m.(interface{ Nav() []core.NavItem }).Nav(); nav != nil {
		t.Errorf("no other pages: want no entry, got %+v", nav)
	}
}
