package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"funnel-ingress/internal/config"
	"funnel-ingress/internal/routes"
)

const domain = "example.com"

func TestParseHost(t *testing.T) {
	cases := []struct {
		host   string
		ns, sv string
		ok     bool
	}{
		{"admin.pdl.example.com", "pdl", "admin", true},
		{"ADMIN.PDL.Example.com:443", "pdl", "admin", true},
		{"admin.pdl.example.com.", "pdl", "admin", true},
		{"admin.pdl.example.com.:8080", "pdl", "admin", true},
		{"pdl.example.com", "", "", false},
		{"a.b.pdl.example.com", "", "", false},
		{"example.com", "", "", false},
		{"", "", "", false},
		{"1.2.3.4", "", "", false},
		{"1.2.3.4:80", "", "", false},
		{"[::1]:80", "", "", false},
		{"::1", "", "", false},
		{"admin.pdl.evil.com", "", "", false},
		{"admin.pdl.example.com.evil.com", "", "", false},
		{"adminpdl.example.com", "", "", false},
		{"-bad.pdl.example.com", "", "", false},
		{"admin..example.com", "", "", false},
		{".pdl.example.com", "", "", false},
		{"admin.pdl.example.com:notaport:x", "", "", false},
		{"xadmin.pdl.example.com ", "", "", false},
	}
	for _, c := range cases {
		ns, sv, ok := ParseHost(c.host, domain)
		if ok != c.ok || ns != c.ns || sv != c.sv {
			t.Errorf("ParseHost(%q) = %q, %q, %v; want %q, %q, %v", c.host, ns, sv, ok, c.ns, c.sv, c.ok)
		}
	}
	if _, _, ok := ParseHost("admin.pdl.example.com", ""); ok {
		t.Error("an empty public domain must match nothing")
	}
	if ns, sv, ok := ParseHost("a.b.Example.COM.", "EXAMPLE.com."); !ok || ns != "b" || sv != "a" {
		t.Error("the configured domain is normalised too")
	}
}

type backend struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen *http.Request
	body string
}

func newBackend(t *testing.T, h func(w http.ResponseWriter, r *http.Request)) *backend {
	t.Helper()
	b := &backend{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bs, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.seen, b.body = r, string(bs)
		b.mu.Unlock()
		if h != nil {
			h(w, r)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *backend) last() (*http.Request, string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen, b.body
}

type dialRecorder struct {
	mu    sync.Mutex
	addrs []string
	to    string
	err   error
}

func (d *dialRecorder) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.addrs = append(d.addrs, addr)
	to, err := d.to, d.err
	d.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return (&net.Dialer{}).DialContext(ctx, network, to)
}

func testCfg() config.Config {
	return config.Config{MaxBodyBytes: 1 << 20, ConnectTimeout: 2 * time.Second, IdleTimeout: 5 * time.Second, RequestTimeout: 5 * time.Second}
}

// rig wires table, dialer and the handler behind an "edge" shim that sets Fly-Client-IP like Fly's proxy.
func rig(t *testing.T, backendAddr string, cfg config.Config, rs ...routes.Route) (*httptest.Server, *dialRecorder, *routes.Table) {
	t.Helper()
	table, err := routes.NewTable(nopPersister{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	table.SetStatic(rs)
	d := &dialRecorder{to: backendAddr}
	h := NewHandler(table, d.dial, func() string { return domain }, cfg)
	edge := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Header.Set("Fly-Client-IP", "203.0.113.7")
		r.Header.Set("X-Forwarded-Proto", "https")
		h.ServeHTTP(w, r)
	})
	srv := httptest.NewServer(edge)
	t.Cleanup(srv.Close)
	return srv, d, table
}

type nopPersister struct{}

func (nopPersister) Load() ([]byte, bool, error) { return nil, false, nil }
func (nopPersister) Save([]byte) error           { return nil }

func route(svc, ns, target string, port int) routes.Route {
	return routes.Route{Key: routes.Key{NS: ns, Service: svc}, Target: target, Port: port}
}

func get(t *testing.T, srv *httptest.Server, host, path string, hdr map[string]string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Host = host
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func TestUnknownHost404EmptyBody(t *testing.T) {
	srv, d, _ := rig(t, "127.0.0.1:1", testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	for _, host := range []string{"nope.pdl.example.com", "admin.nope.example.com", "example.com", "pdl.example.com", "1.2.3.4", "admin.pdl.evil.com", ""} {
		resp, body := get(t, srv, host, "/", nil)
		if resp.StatusCode != 404 || body != "" {
			t.Errorf("host %q: %d %q", host, resp.StatusCode, body)
		}
	}
	if len(d.addrs) != 0 {
		t.Fatalf("dialed for an unknown host: %v", d.addrs)
	}
}

func TestNothingConfiguredYetIsAll404(t *testing.T) {
	table, _ := routes.NewTable(nopPersister{}, 10)
	table.SetStatic([]routes.Route{route("admin", "pdl", "100.64.0.5", 9000)})
	srv := httptest.NewServer(NewHandler(table, nil, func() string { return "" }, testCfg()))
	defer srv.Close()
	resp, body := get(t, srv, "admin.pdl.example.com", "/", nil)
	if resp.StatusCode != 404 || body != "" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestHealthzOnlyForNonServiceHosts(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "from backend") })
	srv, _, _ := rig(t, b.srv.Listener.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	resp, body := get(t, srv, "internal-host.fly.dev", "/healthz", nil)
	if resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("healthz: %d %q", resp.StatusCode, body)
	}
	if _, body := get(t, srv, "admin.pdl.example.com", "/healthz", nil); body != "from backend" {
		t.Fatalf("a service's own /healthz must reach the service, got %q", body)
	}
	if resp, _ := get(t, srv, "internal-host.fly.dev", "/other", nil); resp.StatusCode != 404 {
		t.Fatalf("other paths on non-service hosts: %d", resp.StatusCode)
	}
}

func TestOfflineBackend503NoDetail(t *testing.T) {
	srv, d, _ := rig(t, "", testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	d.err = errors.New("dial tcp 100.64.0.5:9000: node box-17 is offline")
	resp, body := get(t, srv, "admin.pdl.example.com", "/", nil)
	if resp.StatusCode != 503 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	for _, leak := range []string{"100.64", "box-17", "offline", "dial"} {
		if strings.Contains(body, leak) {
			t.Errorf("body leaks %q: %q", leak, body)
		}
	}
	if len(d.addrs) != 1 || d.addrs[0] != "100.64.0.5:9000" {
		t.Fatalf("dialed %v", d.addrs)
	}
}

func TestBackendBreakingTheProtocolIs502(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Write([]byte("not http at all\r\n\r\n"))
			c.Close()
		}
	}()
	srv, _, _ := rig(t, ln.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	resp, body := get(t, srv, "admin.pdl.example.com", "/", nil)
	if resp.StatusCode != 502 || body != "" {
		t.Fatalf("%d %q", resp.StatusCode, body)
	}
}

func TestForwardsToBackendWithOriginalHost(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "yes")
		w.WriteHeader(201)
		io.WriteString(w, "created")
	})
	srv, d, _ := rig(t, b.srv.Listener.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	req, _ := http.NewRequest("POST", srv.URL+"/a/b?x=1&y=2", strings.NewReader("payload"))
	req.Host = "admin.pdl.example.com"
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 201 || string(body) != "created" || resp.Header.Get("X-Backend") != "yes" {
		t.Fatalf("response: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	seen, got := b.last()
	if seen.Host != "admin.pdl.example.com" || seen.Method != "POST" || seen.URL.RequestURI() != "/a/b?x=1&y=2" || got != "payload" {
		t.Fatalf("backend saw host=%q method=%s uri=%s body=%q", seen.Host, seen.Method, seen.URL.RequestURI(), got)
	}
	if seen.Header.Get("X-Forwarded-Host") != "admin.pdl.example.com" || seen.Header.Get("X-Forwarded-Proto") != "https" {
		t.Fatalf("forwarding headers: %v", seen.Header)
	}
	if d.addrs[0] != "100.64.0.5:9000" {
		t.Fatalf("dialed %v", d.addrs)
	}
}

func TestStripsClientForwardedHeaders(t *testing.T) {
	b := newBackend(t, nil)
	srv, _, _ := rig(t, b.srv.Listener.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	get(t, srv, "admin.pdl.example.com", "/", map[string]string{
		"X-Forwarded-For":   "6.6.6.6",
		"X-Forwarded-Host":  "evil.example",
		"X-Forwarded-Port":  "1",
		"Forwarded":         "for=6.6.6.6",
		"X-Real-Ip":         "6.6.6.6",
		"True-Client-Ip":    "6.6.6.6",
		"Cf-Connecting-Ip":  "6.6.6.6",
		"X-Forwarded-Proto": "javascript",
	})
	seen, _ := b.last()
	if got := seen.Header.Get("X-Forwarded-For"); got != "203.0.113.7" {
		t.Fatalf("X-Forwarded-For = %q", got)
	}
	for _, h := range []string{"Forwarded", "X-Real-Ip", "True-Client-Ip", "Cf-Connecting-Ip", "X-Forwarded-Port", "Fly-Client-Ip"} {
		if v := seen.Header.Get(h); v != "" {
			t.Errorf("%s reached the backend: %q", h, v)
		}
	}
	for _, v := range seen.Header.Values("X-Forwarded-For") {
		if strings.Contains(v, "6.6.6.6") {
			t.Errorf("spoofed address in X-Forwarded-For: %q", v)
		}
	}
	if got := seen.Header.Get("X-Forwarded-Host"); got != "admin.pdl.example.com" {
		t.Errorf("X-Forwarded-Host = %q", got)
	}
}

func TestForwardedForFallsBackToPeer(t *testing.T) {
	b := newBackend(t, nil)
	table, _ := routes.NewTable(nopPersister{}, 10)
	table.SetStatic([]routes.Route{route("admin", "pdl", "100.64.0.5", 9000)})
	d := &dialRecorder{to: b.srv.Listener.Addr().String()}
	srv := httptest.NewServer(NewHandler(table, d.dial, func() string { return domain }, testCfg())) // no edge shim
	defer srv.Close()
	get(t, srv, "admin.pdl.example.com", "/", map[string]string{"Fly-Client-IP": "not-an-ip", "X-Forwarded-For": "6.6.6.6"})
	seen, _ := b.last()
	if got := seen.Header.Get("X-Forwarded-For"); got != "127.0.0.1" {
		t.Fatalf("X-Forwarded-For = %q", got)
	}
}

func TestWebSocketUpgrade(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil || !strings.EqualFold(req.Header.Get("Upgrade"), "websocket") {
			return
		}
		fmt.Fprint(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		buf := make([]byte, 5)
		if _, err := io.ReadFull(br, buf); err == nil {
			c.Write([]byte(strings.ToUpper(string(buf))))
		}
	}()
	srv, _, _ := rig(t, ln.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	u, _ := url.Parse(srv.URL)
	c, err := net.Dial("tcp", u.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	fmt.Fprint(c, "GET /ws HTTP/1.1\r\nHost: admin.pdl.example.com\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: x\r\nSec-WebSocket-Version: 13\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "HELLO" {
		t.Fatalf("echo: %q %v", buf, err)
	}
}

func TestBodyLimit413(t *testing.T) {
	b := newBackend(t, nil)
	cfg := testCfg()
	cfg.MaxBodyBytes = 1024
	srv, _, _ := rig(t, b.srv.Listener.Addr().String(), cfg, route("admin", "pdl", "100.64.0.5", 9000))
	post := func(body io.Reader, length int64) int {
		req, _ := http.NewRequest("POST", srv.URL+"/", body)
		req.Host = "admin.pdl.example.com"
		req.ContentLength = length
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		io.Copy(io.Discard, resp.Body)
		return resp.StatusCode
	}
	if code := post(strings.NewReader(strings.Repeat("x", 1024)), 1024); code != 200 {
		t.Errorf("at the limit: %d", code)
	}
	if code := post(strings.NewReader(strings.Repeat("x", 2048)), 2048); code != 413 {
		t.Errorf("declared length over the limit: %d", code)
	}
	if code := post(io.MultiReader(strings.NewReader(strings.Repeat("x", 4096))), -1); code != 413 {
		t.Errorf("chunked body over the limit: %d", code)
	}
}

func TestHopByHopStripped(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Resp-Hop")
		w.Header().Set("X-Resp-Hop", "secret")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-End-To-End", "kept")
	})
	srv, _, _ := rig(t, b.srv.Listener.Addr().String(), testCfg(), route("admin", "pdl", "100.64.0.5", 9000))
	resp, _ := get(t, srv, "admin.pdl.example.com", "/", map[string]string{"Connection": "X-Req-Hop", "X-Req-Hop": "1", "Keep-Alive": "5"})
	seen, _ := b.last()
	for _, h := range []string{"X-Req-Hop", "Keep-Alive"} {
		if seen.Header.Get(h) != "" {
			t.Errorf("request header %s reached the backend", h)
		}
	}
	if resp.Header.Get("X-Resp-Hop") != "" || resp.Header.Get("Keep-Alive") != "" {
		t.Errorf("response hop-by-hop headers leaked: %v", resp.Header)
	}
	if resp.Header.Get("X-End-To-End") != "kept" {
		t.Error("end-to-end header lost")
	}
}

func TestHTTPSBackendOptIn(t *testing.T) {
	b := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "secure") }))
	defer b.Close()
	addr := b.Listener.Addr().String()
	r := route("admin", "pdl", "100.64.0.5", 9000)

	// Plain http route to a TLS backend does not work (the backend answers 400 to a clear-text request).
	srv, _, _ := rig(t, addr, testCfg(), r)
	if resp, _ := get(t, srv, "admin.pdl.example.com", "/", nil); resp.StatusCode == 200 {
		t.Errorf("http route to a TLS backend: %d", resp.StatusCode)
	}
	// https verifies the certificate by default (the test certificate is for 127.0.0.1/example.com, not the target).
	r.Scheme = "https"
	srv, _, _ = rig(t, addr, testCfg(), r)
	if resp, _ := get(t, srv, "admin.pdl.example.com", "/", nil); resp.StatusCode != 502 {
		t.Errorf("https with verification and an untrusted certificate: %d", resp.StatusCode)
	}
	// The per-route opt-in skips verification.
	r.InsecureSkipVerify = true
	srv, _, _ = rig(t, addr, testCfg(), r)
	if resp, body := get(t, srv, "admin.pdl.example.com", "/", nil); resp.StatusCode != 200 || body != "secure" {
		t.Errorf("https with insecure_skip_verify: %d %q", resp.StatusCode, body)
	}
}

func TestRouteChangesTakeEffectWithoutRestart(t *testing.T) {
	b := newBackend(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") })
	srv, _, table := rig(t, b.srv.Listener.Addr().String(), testCfg())
	if resp, _ := get(t, srv, "late.pdl.example.com", "/", nil); resp.StatusCode != 404 {
		t.Fatalf("before: %d", resp.StatusCode)
	}
	if err := table.Put(routes.Route{Key: routes.Key{NS: "pdl", Service: "late"}, Target: "100.64.0.5", Port: 9000, NodeID: "n1"}); err != nil {
		t.Fatal(err)
	}
	if resp, body := get(t, srv, "late.pdl.example.com", "/", nil); resp.StatusCode != 200 || body != "ok" {
		t.Fatalf("after: %d %q", resp.StatusCode, body)
	}
}
