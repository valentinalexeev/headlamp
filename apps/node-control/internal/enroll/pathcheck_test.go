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

// pathAgent is a stub agent that also reports its path check.
type pathAgent struct {
	stubAgent
	p PathCheck
}

func (a *pathAgent) PathCheck(context.Context) PathCheck { return a.p }

func TestEnrollAttentionBridgePath(t *testing.T) {
	pa := &pathAgent{}
	f := newRegFx(t, func(rc *RegistryConfig) { rc.Agents = map[string]Agent{"ingress-1": pa} })
	attention := func() ([]core.AttentionItem, []core.SystemLine) {
		items, lines, err := attentionFor(f).Attention(context.Background(), "alice@")
		if err != nil {
			t.Fatal(err)
		}
		return items, lines
	}
	pathLine := func(lines []core.SystemLine) *core.SystemLine {
		for i := range lines {
			if lines[i].Name == "Bridge paths" {
				return &lines[i]
			}
		}
		return nil
	}

	// off (no agent reports a check): no item and no line
	items, lines := attention()
	if itemWithID(items, "bridge-path-ingress-1") != nil || pathLine(lines) != nil {
		t.Fatalf("off: %+v %+v", items, lines)
	}

	// ok: a line, no item
	pa.p = PathCheck{Known: true, Class: "ok", Streak: 5, Checked: 1000}
	items, lines = attention()
	if l := pathLine(lines); l == nil || l.State != "ok" || !strings.Contains(l.Summary, "1 of 1") {
		t.Fatalf("ok line: %+v", lines)
	}
	if itemWithID(items, "bridge-path-ingress-1") != nil {
		t.Fatal("ok raised an item")
	}

	// one bad run: counted in the line, no item yet
	pa.p = PathCheck{Known: true, Class: "sni-blocked", Streak: 1, Checked: 1000, Since: 900}
	items, lines = attention()
	if itemWithID(items, "bridge-path-ingress-1") != nil ||
		!strings.Contains(pathLine(lines).Summary, "1 sni-blocked") {
		t.Fatalf("one run: %+v %+v", items, lines)
	}

	// blocked twice in a row: critical item and critical line
	pa.p = PathCheck{Known: true, Class: "sni-blocked", Findings: []string{"sni-blocked",
		"udp-blocked"}, Streak: 2, Checked: 1000, Since: 900}
	items, lines = attention()
	it := itemWithID(items, "bridge-path-ingress-1")
	if it == nil || it.Level != core.LevelCritical || !strings.Contains(it.Title, "sni-blocked") ||
		!strings.Contains(it.Detail, "works without the name") ||
		!strings.Contains(it.Detail, "Also: udp-blocked") || it.Since.Unix() != 900 ||
		it.ActionPath != "/nodes?sel=ingress-1" || pathLine(lines).State != "critical" {
		t.Fatalf("blocked: %+v %+v", it, lines)
	}

	// throttled: an info item, the line stays ok
	pa.p = PathCheck{Known: true, Class: "throttled", Streak: 3, Checked: 1000, Since: 800}
	items, lines = attention()
	if it := itemWithID(items, "bridge-path-ingress-1"); it == nil ||
		it.Level != core.LevelInfo || pathLine(lines).State != "ok" {
		t.Fatalf("throttled: %+v %+v", it, lines)
	}

	// a bridge out of sync is not asked
	f.reg.Current(context.Background(), true).SyncTargets[0].State.Failed("unreachable")
	if items, _ := attention(); itemWithID(items, "bridge-path-ingress-1") != nil {
		t.Error("an out-of-sync bridge must get the sync item only")
	}
}

func TestAgentClientPathCheck(t *testing.T) {
	var body string
	var status int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/pathcheck" || r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	a := NewAgent(srv.URL, "tok", srv.Client()).(pathcheckReporter)
	for _, c := range []struct {
		status int
		body   string
		known  bool
		class  string
	}{
		{200, `{"class":"throttled","findings":["throttled","<b>x</b>"],"streak":3,` +
			`"checked":5,"since":4,"counts":{"ok":90,"throttled":6,"bad key!":1}}`, true, "throttled"},
		{200, `{"class":"ok","streak":1,"checked":5}`, true, "ok"},
		{200, `{"disabled":true}`, false, ""},
		{200, `{"class":"ok","checked":0}`, false, ""}, // no run yet
		{200, `{"class":"Not A Class","checked":5}`, false, ""},
		{404, ``, false, ""}, // an older agent
		{200, `not json`, false, ""},
	} {
		status, body = c.status, c.body
		got := a.PathCheck(context.Background())
		if got.Known != c.known || got.Class != c.class {
			t.Errorf("%d %s: got %+v", c.status, c.body, got)
		}
		if c.class == "throttled" && (len(got.Findings) != 1 || len(got.Counts) != 2 ||
			got.Streak != 3 || got.Since != 4) {
			t.Errorf("cleaned fields: %+v", got)
		}
	}
}
