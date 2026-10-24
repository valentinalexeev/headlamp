package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
)

type fakeProvider struct {
	mu    sync.Mutex
	calls []string
	stat  func(ctx context.Context, n Target) (Status, error)
	act   func(op string, n Target) (string, string, error)
}

func (f *fakeProvider) Status(ctx context.Context, n Target) (Status, error) {
	return f.stat(ctx, n)
}
func (f *fakeProvider) Start(ctx context.Context, n Target) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "start")
	f.mu.Unlock()
	return f.act("start", n)
}
func (f *fakeProvider) Stop(ctx context.Context, n Target) (string, string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, "stop")
	f.mu.Unlock()
	return f.act("stop", n)
}

type fakeSrc struct {
	ns  []headscale.Node
	err error
}

func (s fakeSrc) Nodes(context.Context) ([]headscale.Node, error) { return s.ns, s.err }

func testConfig(t *testing.T) *Config {
	t.Helper()
	cfg, err := ParseConfig([]byte(`{"title":"t","nodes":{
		"up":{"provider":"yandex","tailnetName":"u","yandex":{"instance_id":"SECRET"}},
		"bad":{"provider":"oci"},
		"down":{"provider":"fly","fly":{"app":"x"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func newHandler(t *testing.T, cfg *Config, src headscale.NodeSource, provs map[string]Provider, timeout time.Duration) http.Handler {
	t.Helper()
	m := New(cfg, src, provs)
	if timeout > 0 {
		m.(*Module).timeout = timeout
	}
	r := &core.Routes{Admin: http.NewServeMux()}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	return r.Admin
}

func do(h http.Handler, method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func body(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("bad json %q: %v", rec.Body.String(), err)
	}
	return m
}

func okProvider() *fakeProvider {
	return &fakeProvider{
		stat: func(context.Context, Target) (Status, error) { return Status{"status": "RUNNING"}, nil },
		act:  func(string, Target) (string, string, error) { return "RUNNING", "STOPPING", nil },
	}
}

func TestStatusIsolatesFailingNode(t *testing.T) {
	slow := &fakeProvider{stat: func(ctx context.Context, n Target) (Status, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	bad := &fakeProvider{stat: func(context.Context, Target) (Status, error) {
		return nil, ErrNotConfigured
	}}
	boom := &fakeProvider{stat: func(context.Context, Target) (Status, error) {
		return nil, &APIError{Status: 500, Message: "boom"}
	}}
	h := newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": okProvider(), "oci": bad, "fly": boom}, 0)
	b := body(t, do(h, "GET", "/status"))
	if b["up"].(map[string]any)["status"] != "RUNNING" {
		t.Fatalf("up: %v", b["up"])
	}
	if b["bad"].(map[string]any)["status"] != "not_configured" {
		t.Fatalf("bad: %v", b["bad"])
	}
	d := b["down"].(map[string]any)
	if d["status"] != "error" || !strings.Contains(d["detail"].(string), "boom") {
		t.Fatalf("down: %v", d)
	}

	h = newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": okProvider(), "oci": slow, "fly": boom}, 50*time.Millisecond)
	start := time.Now()
	b = body(t, do(h, "GET", "/status"))
	if time.Since(start) > 2*time.Second {
		t.Fatal("slow node was not cut off")
	}
	if b["bad"].(map[string]any)["status"] != "error" {
		t.Fatalf("slow: %v", b["bad"])
	}
	if b["up"].(map[string]any)["status"] != "RUNNING" {
		t.Fatalf("up: %v", b["up"])
	}
}

func TestUnknownNode404(t *testing.T) {
	h := newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": okProvider()}, 0)
	for _, c := range [][2]string{{"GET", "/nodes/nope/status"}, {"POST", "/nodes/nope/start"}, {"POST", "/nodes/nope/stop"}} {
		rec := do(h, c[0], c[1])
		if rec.Code != 404 {
			t.Fatalf("%v: %d", c, rec.Code)
		}
		if got := body(t, rec)["detail"]; got != "unknown node 'nope'" {
			t.Fatalf("detail %v", got)
		}
	}
	if do(h, "POST", "/up/start").Code != 404 {
		t.Fatal("route outside /nodes must 404")
	}
}

func TestStartIdempotentWhenStarted(t *testing.T) {
	p := &fakeProvider{act: func(string, Target) (string, string, error) { return "RUNNING", "RUNNING", nil }}
	h := newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": p}, 0)
	rec := do(h, "POST", "/nodes/up/start")
	if rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	b := body(t, rec)
	if b["node"] != "up" || b["action"] != "start" || b["previous_status"] != "RUNNING" || b["current_status"] != "RUNNING" {
		t.Fatalf("%v", b)
	}
	rec = do(newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": okProvider()}, 0), "POST", "/nodes/up/stop")
	b = body(t, rec)
	if b["action"] != "stop" || b["current_status"] != "STOPPING" {
		t.Fatalf("%v", b)
	}
}

func TestStartMapsErrors(t *testing.T) {
	bad := &fakeProvider{act: func(string, Target) (string, string, error) { return "", "", ErrNotConfigured }}
	boom := &fakeProvider{act: func(string, Target) (string, string, error) { return "", "", &APIError{Status: 500, Message: "boom"} }}
	h := newHandler(t, testConfig(t), nil, map[string]Provider{"oci": bad, "fly": boom}, 0)
	if rec := do(h, "POST", "/nodes/bad/start"); rec.Code != 503 || body(t, rec)["detail"] == nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/nodes/down/start"); rec.Code != 502 || !strings.Contains(body(t, rec)["detail"].(string), "boom") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "POST", "/nodes/up/stop"); rec.Code != 503 { // no yandex provider injected
		t.Fatalf("%d", rec.Code)
	}
}

func TestStatusOnlyTarget(t *testing.T) {
	src := fakeSrc{ns: []headscale.Node{{Name: "node-a", Tags: []string{"tag:exit"}}}}
	h := newHandler(t, &Config{Nodes: map[string]Entry{}}, src, nil, 0)
	b := body(t, do(h, "GET", "/status"))
	if b["node-a"].(map[string]any)["status"] != "unmanaged" {
		t.Fatalf("%v", b)
	}
	rec := do(h, "POST", "/nodes/node-a/start")
	if rec.Code != 409 || body(t, rec)["detail"] != "node has no cloud provider tag" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := do(h, "GET", "/nodes/node-a/status"); body(t, rec)["status"] != "unmanaged" {
		t.Fatal(rec.Body)
	}
}

func TestSourceErrorFallsBackToExplicit(t *testing.T) {
	h := newHandler(t, testConfig(t), fakeSrc{err: errors.New("down")}, map[string]Provider{"yandex": okProvider()}, 0)
	rec := do(h, "GET", "/nodes/up/status")
	if rec.Code != 200 || body(t, rec)["status"] != "RUNNING" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestAliasResolves(t *testing.T) {
	cfg, err := ParseConfig([]byte(`{"aliases":{"old":"node-a"}}`))
	if err != nil {
		t.Fatal(err)
	}
	src := fakeSrc{ns: []headscale.Node{{Name: "node-a", Tags: []string{"tag:exit", "tag:cloud-yandex"}}}}
	h := newHandler(t, cfg, src, map[string]Provider{"yandex": okProvider()}, 0)
	rec := do(h, "POST", "/nodes/old/stop")
	if rec.Code != 200 || body(t, rec)["node"] != "node-a" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestNodesJSONHasNoConnectionDetails(t *testing.T) {
	h := newHandler(t, testConfig(t), nil, nil, 0)
	rec := do(h, "GET", "/nodes.json")
	if strings.Contains(rec.Body.String(), "SECRET") {
		t.Fatal("leak")
	}
	nodes := body(t, rec)["nodes"].(map[string]any)
	if len(nodes) != 3 || nodes["up"].(map[string]any)["tailnetName"] != "u" {
		t.Fatalf("%v", nodes)
	}
}

func TestIndexServesEmbeddedPage(t *testing.T) {
	want, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	rec := do(newHandler(t, testConfig(t), nil, nil, 0), "GET", "/")
	if rec.Body.String() != string(want) {
		t.Fatal("page differs")
	}
	if !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatal(rec.Header())
	}
	if !strings.Contains(rec.Body.String(), "page-title") {
		t.Fatal("no page-title")
	}
}

func TestTargets(t *testing.T) {
	src := fakeSrc{ns: []headscale.Node{{Name: "node-a", Tags: []string{"tag:exit"}}}}
	m := New(&Config{Nodes: map[string]Entry{}}, src, nil).(*Module)
	ts := m.Targets(context.Background())
	if len(ts) != 1 || ts[0].ID != "node-a" {
		t.Fatalf("%v", ts)
	}
}

func TestProviderPanicIsContained(t *testing.T) {
	boom := &fakeProvider{
		stat: func(context.Context, Target) (Status, error) { panic("kaboom") },
		act:  func(string, Target) (string, string, error) { panic("kaboom") },
	}
	h := newHandler(t, testConfig(t), nil, map[string]Provider{"yandex": okProvider(), "fly": boom}, 0)
	b := body(t, do(h, "GET", "/status"))
	d := b["down"].(map[string]any)
	if d["status"] != "error" || !strings.Contains(d["detail"].(string), "provider panicked") {
		t.Fatalf("down: %v", d)
	}
	if b["up"].(map[string]any)["status"] != "RUNNING" {
		t.Fatalf("up: %v", b["up"])
	}
	rec := do(h, "POST", "/nodes/down/start")
	if rec.Code != 502 || !strings.Contains(body(t, rec)["detail"].(string), "provider panicked") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestProviderReceivesCanonicalNameForCollapsedDuplicate(t *testing.T) {
	var gotName string
	fp := &fakeProvider{stat: func(_ context.Context, n Target) (Status, error) {
		gotName = n.Name
		return Status{"status": "RUNNING"}, nil
	}}
	src := fakeSrc{ns: []headscale.Node{
		{Name: "node-a", Tags: []string{"tag:exit", "tag:cloud-oci"}},
		{Name: "node-a-1", Online: true, Tags: []string{"tag:exit", "tag:cloud-oci"}},
	}}
	h := newHandler(t, &Config{Nodes: map[string]Entry{}}, src, map[string]Provider{"oci": fp}, 0)
	if rec := do(h, "GET", "/nodes/node-a/status"); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if gotName != "node-a" {
		t.Fatalf("provider looked the VM up by %q", gotName)
	}
}

type flakySrc struct {
	mu  sync.Mutex
	ns  []headscale.Node
	err error
}

func (s *flakySrc) Nodes(context.Context) ([]headscale.Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ns, s.err
}

func TestHeadscaleBlipKeepsLastGoodNodes(t *testing.T) {
	src := &flakySrc{ns: []headscale.Node{{Name: "node-a", Online: true, Tags: []string{"tag:exit", "tag:cloud-oci"}}}}
	fp := &fakeProvider{
		stat: func(context.Context, Target) (Status, error) { return Status{"status": "RUNNING"}, nil },
		act:  func(string, Target) (string, string, error) { return "STOPPED", "RUNNING", nil },
	}
	h := newHandler(t, &Config{Nodes: map[string]Entry{}}, src, map[string]Provider{"oci": fp}, 0)
	if rec := do(h, "GET", "/nodes.json"); !strings.Contains(rec.Body.String(), "node-a") {
		t.Fatalf("first load: %s", rec.Body.String())
	}
	src.mu.Lock()
	src.ns, src.err = nil, errors.New("headscale down")
	src.mu.Unlock()
	if rec := do(h, "GET", "/nodes.json"); !strings.Contains(rec.Body.String(), "node-a") {
		t.Fatalf("discovered node vanished during a blip: %s", rec.Body.String())
	}
	if rec := do(h, "POST", "/nodes/node-a/start"); rec.Code != 200 {
		t.Fatalf("start during a blip: %d %s", rec.Code, rec.Body.String())
	}
}

func TestHeadscaleDownFromTheStartFallsBackToExplicitTargets(t *testing.T) {
	src := &flakySrc{err: errors.New("headscale down")}
	cfg := mustParse(t, `{"nodes":{"ghost":{"provider":"fly","fly":{"app":"g"}}}}`)
	h := newHandler(t, cfg, src, nil, 0)
	rec := do(h, "GET", "/nodes.json")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ghost") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
