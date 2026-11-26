package nodes

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
)

type fakeExpirer struct {
	nodes      []headscale.Node
	keys       []headscale.PreAuthKey
	listErr    error
	expireErr  error
	keyErr     error
	expired    []uint64
	keyExpired []uint64
}

func (f *fakeExpirer) ListNodes(context.Context) ([]headscale.Node, error) { return f.nodes, f.listErr }
func (f *fakeExpirer) ExpireNode(_ context.Context, id uint64) error {
	if f.expireErr != nil {
		return f.expireErr
	}
	f.expired = append(f.expired, id)
	return nil
}
func (f *fakeExpirer) ListPreAuthKeys(context.Context) ([]headscale.PreAuthKey, error) {
	return f.keys, f.keyErr
}
func (f *fakeExpirer) ExpirePreAuthKey(_ context.Context, id uint64) error {
	f.keyExpired = append(f.keyExpired, id)
	return nil
}

func expireHandler(t *testing.T, hs *fakeExpirer, lg *recLog) http.Handler {
	t.Helper()
	m := New(testConfig(t), nil, nil, WithAudit(func() core.AuditLog { return lg }), WithExpirer(hs))
	r := &core.Routes{Admin: core.NewMux(), Tailnet: whoStub{login: "admin@example.net"}}
	if err := m.Register(r); err != nil {
		t.Fatal(err)
	}
	return r.Admin
}

func post(h http.Handler, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	h.ServeHTTP(rec, req)
	return rec
}

func testTailnet() *fakeExpirer {
	return &fakeExpirer{
		nodes: []headscale.Node{
			{ID: 5, Name: "relay-1", GivenName: "relay-1", Tags: []string{"tag:exit"}, Online: true, PreAuthKeyID: 7},
			{ID: 6, Name: "guard", GivenName: "guard-a", Tags: []string{"tag:wg"}, PreAuthKeyID: 8},
			{ID: 9, Name: "phone", UserID: 3},
		},
		keys: []headscale.PreAuthKey{
			{ID: 7, Reusable: true, Used: true, Expiration: time.Now().Add(time.Hour)},
			{ID: 8, Reusable: false, Used: true, Expiration: time.Now().Add(time.Hour)},
		},
	}
}

func TestTailnetNodesList(t *testing.T) {
	h := expireHandler(t, testTailnet(), &recLog{})
	rec := do(h, "GET", "/nodes/tailnet.json")
	if rec.Code != 200 {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	var out struct {
		Nodes []hsNodeView `json:"nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Nodes) != 3 || out.Nodes[0].Name != "guard-a" || out.Nodes[0].ID != "6" || out.Nodes[2].Name != "relay-1" {
		t.Fatalf("nodes = %+v", out.Nodes)
	}
	if !strings.Contains(out.Nodes[2].Consequence, "exit node") || !strings.Contains(out.Nodes[2].Consequence, "new one-time key") {
		t.Errorf("exit consequence = %q", out.Nodes[2].Consequence)
	}
	if !strings.Contains(out.Nodes[0].Consequence, "tailguard") {
		t.Errorf("generic consequence = %q", out.Nodes[0].Consequence)
	}
	if out.Nodes[1].Tags == nil || !strings.Contains(out.Nodes[1].Consequence, "device of a person") {
		t.Errorf("person node = %+v", out.Nodes[1])
	}
}

func TestExpireNode(t *testing.T) {
	hs, lg := testTailnet(), &recLog{}
	h := expireHandler(t, hs, lg)

	rec := post(h, "/nodes/tailnet/5/expire", `{"name":"relay-1"}`)
	if rec.Code != 200 || body(t, rec)["ok"] != true || body(t, rec)["expired_key"] != "7" {
		t.Fatalf("expire: %d %s", rec.Code, rec.Body)
	}
	if len(hs.expired) != 1 || hs.expired[0] != 5 || len(hs.keyExpired) != 1 || hs.keyExpired[0] != 7 {
		t.Fatalf("expired nodes %v keys %v", hs.expired, hs.keyExpired)
	}
	// a one-time key is used up: only the node is expired
	if rec := post(h, "/nodes/tailnet/6/expire", `{"name":"guard-a"}`); rec.Code != 200 || body(t, rec)["ok"] != true {
		t.Fatalf("expire guard: %d %s", rec.Code, rec.Body)
	}
	if len(hs.keyExpired) != 1 {
		t.Errorf("a one-time key was expired: %v", hs.keyExpired)
	}
	want := []core.AuditEntry{
		{Actor: "admin@example.net", Action: "node.expire", Detail: "relay-1 (id 5): ok, key 7 expired"},
		{Actor: "admin@example.net", Action: "node.expire", Detail: "guard-a (id 6): ok"},
	}
	if len(lg.entries) != len(want) {
		t.Fatalf("audit = %+v", lg.entries)
	}
	for i, w := range want {
		if lg.entries[i] != w {
			t.Errorf("audit %d = %+v, want %+v", i, lg.entries[i], w)
		}
	}
}

func TestExpireNodeRefusals(t *testing.T) {
	hs, lg := testTailnet(), &recLog{}
	h := expireHandler(t, hs, lg)
	for _, c := range []struct {
		path, body string
		code       int
	}{
		{"/nodes/tailnet/5/expire", `{"name":"guard-a"}`, 409}, // the page is out of date
		{"/nodes/tailnet/5/expire", `{}`, 422},
		{"/nodes/tailnet/5/expire", `not json`, 422},
		{"/nodes/tailnet/77/expire", `{"name":"relay-1"}`, 404},
		{"/nodes/tailnet/x/expire", `{"name":"relay-1"}`, 404},
	} {
		if rec := post(h, c.path, c.body); rec.Code != c.code {
			t.Errorf("%s %s: %d, want %d (%s)", c.path, c.body, rec.Code, c.code, rec.Body)
		}
	}
	if len(hs.expired) != 0 || len(lg.entries) != 0 {
		t.Fatalf("refused requests acted: %v %+v", hs.expired, lg.entries)
	}
}

func TestExpireNodeFailures(t *testing.T) {
	hs, lg := testTailnet(), &recLog{}
	hs.expireErr = errors.New("headscale said token=secret")
	h := expireHandler(t, hs, lg)
	if rec := post(h, "/nodes/tailnet/5/expire", `{"name":"relay-1"}`); rec.Code != 502 {
		t.Fatalf("failed expire: %d", rec.Code)
	}
	if len(hs.keyExpired) != 0 {
		t.Error("key expired although the node was not")
	}

	hs.expireErr, hs.keyErr = nil, errors.New("down")
	rec := post(h, "/nodes/tailnet/5/expire", `{"name":"relay-1"}`)
	if rec.Code != 200 || body(t, rec)["ok"] != false || !strings.Contains(body(t, rec)["detail"].(string), "key 7") {
		t.Fatalf("key step failed: %d %s", rec.Code, rec.Body)
	}
	want := []string{"relay-1 (id 5): failed (headscale_error)", "relay-1 (id 5): ok, key not expired (headscale_error)"}
	if len(lg.entries) != 2 || lg.entries[0].Detail != want[0] || lg.entries[1].Detail != want[1] {
		t.Fatalf("audit = %+v", lg.entries)
	}
	for _, e := range lg.entries {
		if strings.Contains(e.Detail, "secret") {
			t.Errorf("audit copies headscale text: %q", e.Detail)
		}
	}

	hs.listErr = headscale.ErrNotConfigured
	if rec := do(h, "GET", "/nodes/tailnet.json"); rec.Code != 503 {
		t.Errorf("not configured list: %d", rec.Code)
	}
}

func TestExpireRoutesOnlyWithExpirer(t *testing.T) {
	h := newHandler(t, testConfig(t), nil, nil, 0)
	if rec := do(h, "GET", "/nodes/tailnet.json"); rec.Code != 404 {
		t.Errorf("list without an expirer: %d", rec.Code)
	}
}
