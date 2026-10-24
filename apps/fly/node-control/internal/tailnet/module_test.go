package tailnet

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

type fakeSrc struct {
	nodes []headscale.Node
	err   error
}

func (f fakeSrc) Nodes(ctx context.Context) ([]headscale.Node, error) { return f.nodes, f.err }

func serve(t *testing.T, src headscale.NodeSource, tracked map[string]bool) (int, map[string]any) {
	t.Helper()
	m := New(src, func(context.Context) map[string]bool { return tracked })
	r := &core.Routes{Admin: http.NewServeMux()}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	r.Admin.ServeHTTP(rec, httptest.NewRequest("GET", "/tailnet/status", nil))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body.String())
	}
	return rec.Code, out
}

func TestTailnetStatusShape(t *testing.T) {
	ls := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	code, out := serve(t, fakeSrc{nodes: []headscale.Node{{
		Name: "a", Online: true, LastSeen: &ls, IPAddresses: []string{"100.64.0.1"},
		ApprovedRoutes: []string{"x"}, AvailableRoutes: []string{"x", "y"},
	}, {Name: "b"}}}, map[string]bool{"a": true, "b": true})
	if code != 200 {
		t.Fatal(code)
	}
	e := out["a"].(map[string]any)
	if len(e) != 6 || e["name"] != "a" || e["online"] != true || e["last_seen"] != "2026-01-02T03:04:05Z" ||
		e["approved_route_count"] != float64(1) || e["available_route_count"] != float64(2) {
		t.Fatalf("%v", e)
	}
	if ips := e["ip_addresses"].([]any); len(ips) != 1 || ips[0] != "100.64.0.1" {
		t.Fatalf("%v", e)
	}
	b := out["b"].(map[string]any)
	if v, ok := b["last_seen"]; !ok || v != nil {
		t.Fatalf("last_seen should be null: %v", b)
	}
	if ips, ok := b["ip_addresses"].([]any); !ok || len(ips) != 0 {
		t.Fatalf("ip_addresses should be []: %v", b)
	}
}

func TestDuplicateSuffixOnlineWins(t *testing.T) {
	_, out := serve(t, fakeSrc{nodes: []headscale.Node{
		{Name: "a", Online: false}, {Name: "a-1", Online: true},
	}}, map[string]bool{"a": true})
	e := out["a"].(map[string]any)
	if e["name"] != "a-1" || e["online"] != true {
		t.Fatalf("%v", e)
	}
	// first seen wins when neither/both online
	_, out = serve(t, fakeSrc{nodes: []headscale.Node{
		{Name: "a"}, {Name: "a-2"},
	}}, map[string]bool{"a": true})
	if out["a"].(map[string]any)["name"] != "a" {
		t.Fatalf("%v", out)
	}
}

func TestUntrackedSkipped(t *testing.T) {
	_, out := serve(t, fakeSrc{nodes: []headscale.Node{{Name: "a"}, {Name: "z"}, {Name: "z-1"}}},
		map[string]bool{"a": true})
	if len(out) != 1 || out["a"] == nil {
		t.Fatalf("%v", out)
	}
}

func TestNotConfigured(t *testing.T) {
	code, out := serve(t, fakeSrc{err: headscale.ErrNotConfigured}, nil)
	if code != 200 || out["status"] != "not_configured" || out["detail"] != "HEADSCALE_API_KEY is not set" {
		t.Fatalf("%d %v", code, out)
	}
}

func TestAPIError502(t *testing.T) {
	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "kaput", 500)
	}))
	defer hs.Close()
	src := &headscale.Client{BaseURL: hs.URL, APIKey: "k"}
	code, out := serve(t, headscale.Cached(src, time.Second), map[string]bool{"a": true})
	if code != 502 || out["detail"] == nil || out["detail"] == "" {
		t.Fatalf("%d %v", code, out)
	}
	code, _ = serve(t, fakeSrc{err: errors.New("x")}, nil)
	if code != 502 {
		t.Fatal(code)
	}
}

func TestTrackedGenuineSuffixNameIsKept(t *testing.T) {
	// "relay-1" is a real node name with no "relay" registration.
	code, out := serve(t, fakeSrc{nodes: []headscale.Node{{Name: "relay-1", Online: true}}}, map[string]bool{"relay-1": true})
	if code != 200 || out["relay-1"] == nil {
		t.Fatalf("%d %v", code, out)
	}
}

func TestCollapsedRegistrationsReportUnderTrackedName(t *testing.T) {
	code, out := serve(t, fakeSrc{nodes: []headscale.Node{{Name: "node-a"}, {Name: "node-a-1", Online: true}}}, map[string]bool{"node-a": true})
	e, _ := out["node-a"].(map[string]any)
	if code != 200 || e == nil || e["online"] != true || e["name"] != "node-a-1" || len(out) != 1 {
		t.Fatalf("%d %v", code, out)
	}
}

func TestTrackedReceivesRequestContext(t *testing.T) {
	type key struct{}
	var got any
	m := New(fakeSrc{}, func(ctx context.Context) map[string]bool { got = ctx.Value(key{}); return nil })
	r := &core.Routes{Admin: http.NewServeMux()}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/tailnet/status", nil)
	r.Admin.ServeHTTP(httptest.NewRecorder(), req.WithContext(context.WithValue(req.Context(), key{}, "v")))
	if got != "v" {
		t.Fatalf("tracked did not get the request context: %v", got)
	}
}
