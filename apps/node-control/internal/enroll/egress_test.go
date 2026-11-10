package enroll

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// egressAgent is a stub agent that also reports its egress self-test (#196).
type egressAgent struct {
	stubAgent
	e Egress
}

func (a *egressAgent) Egress(context.Context) Egress { return a.e }

func TestEnrollAttentionBridgeEgressFails(t *testing.T) {
	ea := &egressAgent{}
	f := newRegFx(t, func(rc *RegistryConfig) { rc.Agents = map[string]Agent{"ingress-1": ea} })
	attention := func() []core.AttentionItem {
		items, _, err := attentionFor(f).Attention(context.Background(), "alice@")
		if err != nil {
			t.Fatal(err)
		}
		return items
	}
	ea.e = Egress{Known: true, OK: false, Error: "the exit node refused the connection (SOCKS reply 1)", Checked: 1000, Since: 900}
	items := attention()
	it := itemWithID(items, "bridge-egress-ingress-1")
	if it == nil || it.Level != core.LevelCritical || !strings.Contains(it.Title, "cannot reach the internet") ||
		!strings.Contains(it.Detail, "SOCKS reply 1") || !strings.Contains(it.Detail, "autogroup:internet") ||
		it.ActionPath != "/nodes?sel=ingress-1" || it.Since.Unix() != 900 {
		t.Fatalf("%+v", items)
	}

	// a passing, unknown or disabled self-test raises nothing
	for _, e := range []Egress{{Known: true, OK: true, Checked: 1000}, {}} {
		ea.e = e
		if items := attention(); itemWithID(items, "bridge-egress-ingress-1") != nil {
			t.Errorf("%+v raised an item", e)
		}
	}
	// a bridge out of sync gets only the sync item: the egress item would say the same, less precisely
	ea.e = Egress{Known: true, OK: false, Error: "x", Checked: 1000, Since: 900}
	f.reg.Current(context.Background(), true).SyncTargets[0].State.Failed("unreachable")
	if items := attention(); itemWithID(items, "bridge-egress-ingress-1") != nil || itemWithID(items, "bridge-sync-ingress-1") == nil {
		t.Errorf("an out-of-sync bridge must get the sync item only: %+v", items)
	}
}

func TestAgentClientEgress(t *testing.T) {
	var body string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/egress" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	a := NewAgent(srv.URL, "tok", srv.Client()).(egressReporter)
	for _, c := range []struct {
		status int
		body   string
		want   Egress
	}{
		{200, `{"ok":false,"checked":5,"since":4,"error":"SOCKS port unreachable"}`, Egress{Known: true, Error: "SOCKS port unreachable", Checked: 5, Since: 4}},
		{200, `{"ok":true,"checked":5,"since":4}`, Egress{Known: true, OK: true, Checked: 5, Since: 4}},
		{200, `{"disabled":true,"ok":false,"checked":0}`, Egress{}},
		{200, `{"ok":false,"checked":0}`, Egress{}}, // no probe yet
		{404, ``, Egress{}}, // an agent before #196
		{200, `not json`, Egress{}},
	} {
		status, body = c.status, c.body
		if got := a.Egress(context.Background()); got != c.want {
			t.Errorf("%d %s: got %+v, want %+v", c.status, c.body, got, c.want)
		}
	}
}

func TestListIngressesShowsEgress(t *testing.T) {
	ea := &egressAgent{e: Egress{Known: true, OK: false, Error: "SOCKS port unreachable", Checked: 7, Since: 6}}
	f := newRegFx(t, func(rc *RegistryConfig) { rc.Agents = map[string]Agent{"ingress-1": ea} })
	got := egressOf(context.Background(), f.reg.Current(context.Background(), true).SyncTargets)
	if len(got) != 1 || got["ingress-1"].Error != "SOCKS port unreachable" || got["ingress-1"].Checked != 7 {
		t.Fatalf("%+v", got)
	}
	ea.e = Egress{}
	if got := egressOf(context.Background(), f.reg.Current(context.Background(), true).SyncTargets); len(got) != 0 {
		t.Fatalf("an unknown self-test must not be listed: %+v", got)
	}
}
