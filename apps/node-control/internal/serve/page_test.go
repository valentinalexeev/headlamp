package serve

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

// The Serve page (#129): the page and its script are served with the console's security headers, to viewers only.
func TestPageServedWithSecurityHeaders(t *testing.T) {
	r := newRig(t, nil)
	for path, ctype := range map[string]string{"/serve": "text/html", "/serve/static/serve.js": "text/javascript"} {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = ipViewer + ":40000"
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, req)
		h := rec.Header()
		if rec.Code != 200 || !strings.HasPrefix(h.Get("Content-Type"), ctype) {
			t.Fatalf("%s: %d %s", path, rec.Code, h.Get("Content-Type"))
		}
		if h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Security-Policy") != ui.PageCSP {
			t.Errorf("%s headers: %v", path, h)
		}
		for _, ip := range []string{ipPlain, ipProxyA} {
			if c, _ := r.do("GET", path, ip, nil); c != 403 {
				t.Errorf("%s from %s: %d, want 403", path, ip, c)
			}
		}
	}
}

func TestPageHasNoInlineCodeOrExternalReferences(t *testing.T) {
	html := strings.ToLower(string(pageHTML))
	for _, m := range regexp.MustCompile(`<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", "<link"} {
		if strings.Contains(html, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	js := string(pageJS)
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function", "confirm(", "prompt(", "alert(", "XMLHttpRequest", "localStorage", "sessionStorage"} {
		if strings.Contains(js, bad) {
			t.Errorf("script contains %q", bad)
		}
	}
	if regexp.MustCompile(`https?://(?:[a-z])`).MatchString(strings.ReplaceAll(js, "'FUNNEL_SERVE_URL=http://' +", "")) {
		t.Error("script reaches outside the origin")
	}
}

func TestPageSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "testdata/serve_selftest.js", "../ui/static/hl.js", "static/serve.js").CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/serve.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

// The state tells the page who may change things, and the address the headscale split must name.
func TestStateForThePage(t *testing.T) {
	r := newRig(t, nil)
	r.hs.mu.Lock()
	r.hs.nodes = append(r.hs.nodes, headscale.Node{ID: 99, GivenName: "node-control", IPAddresses: []string{"fd7a:115c:a1e0::1", "100.64.0.99"}})
	r.hs.mu.Unlock()
	var got stateResp
	for ip, admin := range map[string]bool{ipOwner: true, ipViewer: false} {
		c, b := r.do("GET", "/serve/state", ip, nil)
		if c != 200 || json.Unmarshal(b, &got) != nil {
			t.Fatalf("%d %s", c, b)
		}
		if got.CanAdmin != admin || got.DNSServer != "100.64.0.99" || got.KeyTTLHours != 90*24 {
			t.Errorf("from %s: can_admin=%v dns_server=%q ttl=%d", ip, got.CanAdmin, got.DNSServer, got.KeyTTLHours)
		}
	}
}

// Every change is in the activity log with the actor, and the key secret never is.
func TestActivityRecordsChangesWithoutSecrets(t *testing.T) {
	r := newRig(t, nil)
	g := r.create("grafana", "mon")
	c, b := r.do("POST", "/serve/services/"+g.Service.ID+"/keys", ipOwner, nil)
	if c != 201 {
		t.Fatalf("add key: %d %s", c, b)
	}
	var k keyResp
	_ = json.Unmarshal(b, &k)
	if c, _ := r.do("DELETE", "/serve/services/"+g.Service.ID+"/keys/"+itoa(g.Service.Keys[0].ID), ipOwner, nil); c != 204 {
		t.Fatalf("retire: %d", c)
	}
	if c, _ := r.do("DELETE", "/serve/services/"+g.Service.ID, ipOwner, nil); c != 204 {
		t.Fatalf("delete: %d", c)
	}
	es, err := r.mod.Activity(context.Background(), "viewer@", 50)
	if err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Action)
		if e.Actor != "owner" || e.Source != "Serve" {
			t.Errorf("entry %+v", e)
		}
		if strings.Contains(e.Detail, g.AuthKey) || strings.Contains(e.Detail, k.AuthKey) {
			t.Errorf("a key secret is in the log: %+v", e)
		}
	}
	if strings.Join(actions, ",") != "service.delete,key.retire,key.add,service.create" {
		t.Errorf("actions %v", actions)
	}
	if _, err := r.mod.Activity(context.Background(), "plain@", 10); err != core.ErrNotVisible {
		t.Errorf("a caller without serve.view: %v", err)
	}
	if items, err := r.mod.Search(context.Background(), "viewer@"); err != nil || len(items) != 0 {
		t.Errorf("search after delete: %v %v", items, err)
	}
}

func TestAttention(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	svcs := []Service{
		{ID: "ok", Name: "ok.z", Keys: []Key{{ID: 1, Expires: now.Add(60 * 24 * time.Hour)}}},
		{ID: "soon", Name: "soon.z", Keys: []Key{{ID: 2, Expires: now.Add(5 * 24 * time.Hour)}}},
		{ID: "gone", Name: "gone.z", Keys: []Key{{ID: 3, Expires: now.Add(-time.Hour)}}},
		// rotating: the new key is fine, the old one about to expire is not a problem
		{ID: "rot", Name: "rot.z", Keys: []Key{{ID: 5, Expires: now.Add(80 * 24 * time.Hour)}, {ID: 4, Expires: now.Add(24 * time.Hour)}}},
	}
	nodes := []headscale.Node{
		{PreAuthKeyID: 1, Online: true}, {PreAuthKeyID: 2, Online: true}, {PreAuthKeyID: 3, Online: true},
		{PreAuthKeyID: 4, Online: false},
	}
	items, online := attention(svcs, nodes, true, now)
	got := map[string]core.AttentionLevel{}
	for _, it := range items {
		got[it.ID] = it.Level
	}
	want := map[string]core.AttentionLevel{"serve-key-soon": core.LevelTimed, "serve-key-gone": core.LevelCritical, "serve-offline-rot": core.LevelWaiting}
	if online != 3 || len(got) != len(want) {
		t.Fatalf("online=%d items=%v", online, got)
	}
	for id, lvl := range want {
		if got[id] != lvl {
			t.Errorf("%s: %q, want %q", id, got[id], lvl)
		}
	}
	// headscale did not answer: nothing is called offline
	items, _ = attention(svcs, nil, false, now)
	for _, it := range items {
		if strings.HasPrefix(it.ID, "serve-offline-") {
			t.Errorf("offline item without node data: %+v", it)
		}
	}
}

func TestSelfAddr(t *testing.T) {
	nodes := []headscale.Node{
		{GivenName: "node-control", IPAddresses: []string{"fd7a::1", "100.64.0.9"}},
		{GivenName: "nc-prod", Tags: []string{"tag:node-control"}, IPAddresses: []string{"100.64.0.1"}},
	}
	if got := selfAddr(nodes, "node-control"); got != "100.64.0.1" {
		t.Errorf("the tagged node wins: %q", got)
	}
	nodes = append(nodes, headscale.Node{GivenName: "nc-2", Tags: []string{"tag:node-control"}, IPAddresses: []string{"100.64.0.2"}})
	if got := selfAddr(nodes, "node-control"); got != "100.64.0.9" {
		t.Errorf("two tagged nodes: by name, got %q", got)
	}
	if got := selfAddr(nil, "x"); got != "" {
		t.Errorf("nothing known: %q", got)
	}
}

func TestNavOnlyWhenActive(t *testing.T) {
	r := newRig(t, nil)
	nav := r.mod.Nav()
	if len(nav) != 1 || nav[0].Path != "/serve" || nav[0].Allow == nil || !nav[0].Allow("viewer@") || nav[0].Allow("plain@") {
		t.Fatalf("nav %+v", nav)
	}
	if (&Module{}).Nav() != nil {
		t.Error("an inactive module shows a page")
	}
}

func itoa(v uint64) string { b, _ := json.Marshal(v); return string(b) }
