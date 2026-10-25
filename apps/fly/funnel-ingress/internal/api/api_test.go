package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
	"funnel-ingress/internal/tsnode"
)

type memPersister struct {
	mu   sync.Mutex
	blob []byte
	fail bool
}

func (m *memPersister) Load() ([]byte, bool, error) { return m.blob, m.blob != nil, nil }
func (m *memPersister) Save(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail {
		return io.ErrClosedPipe
	}
	m.blob = b
	return nil
}

type env struct {
	h     http.Handler
	node  *tsnode.Fake
	table *routes.Table
	p     *memPersister
	ns    *auth.Namespaces
}

func setup(t *testing.T) *env {
	t.Helper()
	node := tsnode.NewFake()
	p := &memPersister{}
	table, err := routes.NewTable(p, 3)
	if err != nil {
		t.Fatal(err)
	}
	ns := auth.NewNamespaces()
	ns.Set(map[string][]config.PortRange{
		"pdl":   {{Lo: 8080, Hi: 8080}, {Lo: 9000, Hi: 9010}},
		"other": {{Lo: 3000, Hi: 3000}},
	})
	node.AddIdentity("100.64.0.5", tsnode.Identity{NodeID: "n1", Name: "box", Tags: []string{"tag:funnel", "tag:ns-pdl"}})
	node.AddIdentity("100.64.0.6", tsnode.Identity{NodeID: "n2", Name: "box2", Tags: []string{"tag:funnel", "tag:ns-pdl"}})
	node.AddIdentity("100.64.0.7", tsnode.Identity{NodeID: "n3", Name: "multi", Tags: []string{"tag:funnel", "tag:ns-pdl", "tag:ns-other"}})
	node.AddIdentity("100.64.0.8", tsnode.Identity{NodeID: "n4", Name: "untagged"})
	node.AddIdentity("100.64.0.9", tsnode.Identity{NodeID: "n5", Name: "funnel-only", Tags: []string{"tag:funnel"}})
	node.AddIdentity("100.64.0.10", tsnode.Identity{NodeID: "n6", Name: "ns-only", Tags: []string{"tag:ns-pdl"}})
	return &env{
		node: node, table: table, p: p, ns: ns,
		h: NewHandler(Deps{Node: node, Table: table, Policy: auth.Policy{FunnelTag: "tag:funnel", NSTagPrefix: "tag:ns-", NS: ns}, APIPort: 9100, ProxyPort: 8080}),
	}
}

type resp struct {
	code int
	body string
}

func (e *env) do(method, path, from, ctype, body string) resp {
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = from + ":4242"
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return resp{w.Code, w.Body.String()}
}

func (e *env) put(from, service, body string) resp {
	return e.do("PUT", "/v1/routes/"+service, from, "application/json", body)
}

func TestPutCreatesRouteWithCallerAddr(t *testing.T) {
	e := setup(t)
	// A body naming a target is ignored: the backend is the caller's own tailnet IP.
	got := e.put("100.64.0.5", "admin", `{"ns":"pdl","port":9000,"target":"10.0.0.1","addr":"10.0.0.2"}`)
	if got.code != 204 || got.body != "" {
		t.Fatalf("put: %+v", got)
	}
	r, ok := e.table.Lookup(routes.Key{NS: "pdl", Service: "admin"})
	if !ok || r.Target != "100.64.0.5" || r.Port != 9000 || r.NodeID != "n1" || r.Scheme != "http" || r.Kind != routes.Dynamic {
		t.Fatalf("stored route: %+v", r)
	}
}

func TestPutImplicitNamespaceAndHTTPS(t *testing.T) {
	e := setup(t)
	if got := e.put("100.64.0.5", "admin", `{"port":9001,"scheme":"https"}`); got.code != 204 {
		t.Fatalf("put: %+v", got)
	}
	r, _ := e.table.Lookup(routes.Key{NS: "pdl", Service: "admin"})
	if r.Scheme != "https" || r.InsecureSkipVerify {
		t.Fatalf("route: %+v", r)
	}
}

func TestPutForbiddenWithoutBothTags(t *testing.T) {
	e := setup(t)
	for _, from := range []string{"100.64.0.8", "100.64.0.9", "100.64.0.10"} {
		got := e.put(from, "admin", `{"ns":"pdl","port":9000}`)
		if got.code != 403 || got.body != "" {
			t.Errorf("%s: %+v", from, got)
		}
	}
	if len(e.table.All()) != 0 {
		t.Fatal("table changed")
	}
}

func TestPutUnknownNamespaceIs403(t *testing.T) {
	e := setup(t)
	// Another namespace the node has no tag for, and a namespace that does not exist at all.
	for _, ns := range []string{"other", "ghost"} {
		if got := e.put("100.64.0.5", "admin", `{"ns":"`+ns+`","port":3000}`); got.code != 403 || got.body != "" {
			t.Errorf("ns %s: %+v", ns, got)
		}
	}
}

func TestPutAmbiguousNamespace(t *testing.T) {
	e := setup(t)
	if got := e.put("100.64.0.7", "admin", `{"port":3000}`); got.code != 422 || got.body != "" {
		t.Fatalf("%+v", got)
	}
	if got := e.put("100.64.0.7", "admin", `{"ns":"other","port":3000}`); got.code != 204 {
		t.Fatalf("explicit: %+v", got)
	}
}

func TestPutPortNotAllowed(t *testing.T) {
	e := setup(t)
	if got := e.put("100.64.0.5", "admin", `{"ns":"pdl","port":8081}`); got.code != 403 || got.body != "" {
		t.Fatalf("%+v", got)
	}
}

func TestPutBlockedKeyIs403(t *testing.T) {
	e := setup(t)
	e.table.SetBlocked([]routes.Key{{NS: "pdl", Service: "admin"}})
	if got := e.put("100.64.0.5", "admin", `{"ns":"pdl","port":9000}`); got.code != 403 || got.body != "" {
		t.Fatalf("%+v", got)
	}
}

func TestPutConflictOtherNode(t *testing.T) {
	e := setup(t)
	if got := e.put("100.64.0.5", "admin", `{"port":9000}`); got.code != 204 {
		t.Fatal(got)
	}
	if got := e.put("100.64.0.6", "admin", `{"port":9000}`); got.code != 409 {
		t.Fatalf("%+v", got)
	}
	if got := e.put("100.64.0.5", "admin", `{"port":9000}`); got.code != 204 {
		t.Fatalf("idempotent re-registration: %+v", got)
	}
}

func TestPutStaticConflict(t *testing.T) {
	e := setup(t)
	e.table.SetStatic([]routes.Route{{Key: routes.Key{NS: "pdl", Service: "docs"}, Target: "pdl-1", Port: 8080}})
	if got := e.put("100.64.0.5", "docs", `{"port":9000}`); got.code != 409 {
		t.Fatalf("%+v", got)
	}
}

func TestPutLimitAndPersistFailure(t *testing.T) {
	e := setup(t)
	for _, s := range []string{"a", "b", "c"} {
		if got := e.put("100.64.0.5", s, `{"port":9000}`); got.code != 204 {
			t.Fatal(got)
		}
	}
	if got := e.put("100.64.0.5", "d", `{"port":9000}`); got.code != 429 {
		t.Fatalf("limit: %+v", got)
	}
	e.p.fail = true
	if got := e.put("100.64.0.6", "z", `{"port":9000}`); got.code != 503 || got.body != "" {
		t.Fatalf("persist failure: %+v", got)
	}
	if _, ok := e.table.Lookup(routes.Key{NS: "pdl", Service: "z"}); ok {
		t.Fatal("route kept although it could not be saved")
	}
}

func TestDeleteOwnerOnly(t *testing.T) {
	e := setup(t)
	e.put("100.64.0.5", "admin", `{"port":9000}`)
	if got := e.do("DELETE", "/v1/routes/admin?ns=pdl", "100.64.0.6", "", ""); got.code != 409 {
		t.Fatalf("other node: %+v", got)
	}
	if _, ok := e.table.Lookup(routes.Key{NS: "pdl", Service: "admin"}); !ok {
		t.Fatal("route removed by a non-owner")
	}
	if got := e.do("DELETE", "/v1/routes/admin?ns=pdl", "100.64.0.5", "", ""); got.code != 204 {
		t.Fatalf("owner: %+v", got)
	}
	if got := e.do("DELETE", "/v1/routes/admin", "100.64.0.5", "", ""); got.code != 204 {
		t.Fatalf("absent key with implicit ns: %+v", got)
	}
	if got := e.do("DELETE", "/v1/routes/admin", "100.64.0.7", "", ""); got.code != 422 {
		t.Fatalf("ambiguous ns: %+v", got)
	}
	if got := e.do("DELETE", "/v1/routes/admin?ns=pdl", "100.64.0.8", "", ""); got.code != 403 {
		t.Fatalf("untagged: %+v", got)
	}
}

func TestGetOwnRoutesOnly(t *testing.T) {
	e := setup(t)
	e.put("100.64.0.5", "admin", `{"port":9000}`)
	e.put("100.64.0.6", "wiki", `{"port":9001}`)
	got := e.do("GET", "/v1/routes", "100.64.0.5", "", "")
	if got.code != 200 || !strings.Contains(got.body, `"admin"`) || strings.Contains(got.body, "wiki") || strings.Contains(got.body, "100.64") {
		t.Fatalf("%+v", got)
	}
	if got := e.do("GET", "/v1/routes", "100.64.0.8", "", ""); got.code != 403 {
		t.Fatalf("untagged: %+v", got)
	}
	empty := e.do("GET", "/v1/routes", "100.64.0.7", "", "")
	if empty.code != 200 || !strings.Contains(empty.body, `"routes":[]`) {
		t.Fatalf("empty list: %+v", empty)
	}
}

func TestUnknownCallerIs401(t *testing.T) {
	e := setup(t)
	for _, r := range []resp{
		e.put("100.64.9.9", "admin", `{"port":9000}`),
		e.do("DELETE", "/v1/routes/admin?ns=pdl", "100.64.9.9", "", ""),
		e.do("GET", "/v1/routes", "100.64.9.9", "", ""),
	} {
		if r.code != 401 || r.body != "" {
			t.Errorf("%+v", r)
		}
	}
}

func TestRejectsProxyAndAPIPort(t *testing.T) {
	e := setup(t)
	e.ns.Set(map[string][]config.PortRange{"pdl": {{Lo: 1, Hi: 65535}}})
	for _, port := range []string{"9100", "8080"} {
		if got := e.put("100.64.0.5", "admin", `{"port":`+port+`}`); got.code != 422 {
			t.Errorf("port %s: %+v", port, got)
		}
	}
	// A caller whose address is the proxy's own is refused: no request loop through the public listener.
	e.node.AddIdentity("100.64.0.1", tsnode.Identity{NodeID: "self", Addr: netip.MustParseAddr("100.64.0.1"), Tags: []string{"tag:funnel", "tag:ns-pdl"}})
	if got := e.put("100.64.0.1", "loop", `{"port":9000}`); got.code != 403 {
		t.Errorf("self: %+v", got)
	}
}

func TestInvalidJSONAndNames(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct{ svc, body string }{
		"not json":       {"admin", `{`},
		"empty body":     {"admin", ``},
		"wrong type":     {"admin", `{"port":"80"}`},
		"missing port":   {"admin", `{"ns":"pdl"}`},
		"port zero":      {"admin", `{"port":0}`},
		"port too big":   {"admin", `{"port":65536}`},
		"upper service":  {"Admin", `{"port":9000}`},
		"dotted service": {"a.b", `{"port":9000}`},
		"bad ns":         {"admin", `{"ns":"PDL","port":9000}`},
		"bad scheme":     {"admin", `{"port":9000,"scheme":"ftp"}`},
		"long service":   {strings.Repeat("a", 64), `{"port":9000}`},
	} {
		got := e.put("100.64.0.5", c.svc, c.body)
		if got.code != 422 || got.body != "" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestBodySizeLimit(t *testing.T) {
	e := setup(t)
	big := `{"port":9000,"pad":"` + strings.Repeat("x", 1<<20) + `"}`
	if got := e.put("100.64.0.5", "admin", big); got.code != 413 {
		t.Fatalf("%d", got.code)
	}
}

func TestMutationsRequireJSONContentType(t *testing.T) {
	e := setup(t)
	for _, ct := range []string{"application/x-www-form-urlencoded", "text/plain", ""} {
		if got := e.do("PUT", "/v1/routes/admin", "100.64.0.5", ct, `{"port":9000}`); got.code != 415 {
			t.Errorf("content type %q: %+v", ct, got)
		}
	}
	if got := e.do("PUT", "/v1/routes/admin", "100.64.0.5", "application/json; charset=utf-8", `{"port":9000}`); got.code != 204 {
		t.Errorf("charset parameter: %+v", got)
	}
}

func TestNoAdminRoutesExist(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/v1/admin/routes", "/v1/admin", "/admin", "/", "/v1", "/metrics", "/debug/pprof/"} {
		for _, m := range []string{"GET", "PUT", "POST", "DELETE"} {
			if got := e.do(m, p, "100.64.0.5", "application/json", `{}`); got.code != 404 || got.body != "" {
				t.Errorf("%s %s: %+v", m, p, got)
			}
		}
	}
	if got := e.do("POST", "/v1/routes/admin", "100.64.0.5", "application/json", `{}`); got.code != 405 {
		t.Errorf("POST route: %+v", got)
	}
}

func TestResponsesAreNotCacheable(t *testing.T) {
	e := setup(t)
	r := httptest.NewRequest("GET", "/v1/routes", nil)
	r.RemoteAddr = "100.64.0.5:1"
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing Cache-Control: no-store")
	}
}
