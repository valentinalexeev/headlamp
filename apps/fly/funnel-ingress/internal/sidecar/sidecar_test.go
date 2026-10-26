package sidecar

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"funnel-ingress/internal/config"
)

func TestParseServices(t *testing.T) {
	good := []struct {
		in   string
		want []Service
	}{
		{"admin=8080", []Service{{"admin", 8080, "127.0.0.1:8080"}}},
		{"api=9000@127.0.0.1:3000", []Service{{"api", 9000, "127.0.0.1:3000"}}},
		{"a=1,b=2", []Service{{"a", 1, "127.0.0.1:1"}, {"b", 2, "127.0.0.1:2"}}},
		{" a=1 , b=2@svc.ns.svc:80 ", []Service{{"a", 1, "127.0.0.1:1"}, {"b", 2, "svc.ns.svc:80"}}},
		{"x=65535", []Service{{"x", 65535, "127.0.0.1:65535"}}},
	}
	for _, c := range good {
		got, err := ParseServices(c.in)
		if err != nil || len(got) != len(c.want) {
			t.Errorf("%q: %v %v", c.in, got, err)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%q: got %v, want %v", c.in, got[i], c.want[i])
			}
		}
	}
	for _, bad := range []string{
		"", " ", "a", "a=", "=80", "a=0", "a=65536", "a=x", "A=80", "a.b=80", "-a=80", "a=80,a=81", "a=80,b=80",
		"a=80@host", "a=80@:80", "a=80@host:0", "a=80@host:x", "a=80,", "a=80@", "a=80@[::1", "a=-1",
	} {
		if got, err := ParseServices(bad); err == nil {
			t.Errorf("%q accepted: %v", bad, got)
		}
	}
}

func loopback(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

func realDial(ctx context.Context, network, addr string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

func settleGoroutines(t *testing.T, before int) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if runtime.NumGoroutine() <= before+1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	buf := make([]byte, 1<<16)
	t.Fatalf("goroutines did not settle (before %d, now %d):\n%s", before, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
}

func TestForwardCopiesBothWays(t *testing.T) {
	before := runtime.NumGoroutine()
	target := loopback(t)
	targetClosed := make(chan struct{})
	go func() {
		c, err := target.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c) // echo until the peer closes
		c.Close()
		close(targetClosed)
	}()
	fl := loopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Forward(ctx, fl, target.Addr().String(), realDial); close(done) }()

	c, err := net.Dial("tcp", fl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	payload := strings.Repeat("0123456789", 10000)
	go c.Write([]byte(payload))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil || string(got) != payload {
		t.Fatalf("echo through the forwarder: %v", err)
	}
	// Closing the client closes the target side.
	c.Close()
	select {
	case <-targetClosed:
	case <-time.After(5 * time.Second):
		t.Fatal("closing one side did not close the other")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Forward did not return after cancel")
	}
	target.Close()
	settleGoroutines(t, before)
}

func TestForwardCancelClosesOpenConnections(t *testing.T) {
	target := loopback(t)
	defer target.Close()
	go func() {
		for {
			c, err := target.Accept()
			if err != nil {
				return
			}
			go io.Copy(io.Discard, c)
		}
	}()
	fl := loopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { Forward(ctx, fl, target.Addr().String(), realDial); close(done) }()
	c, err := net.Dial("tcp", fl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("x"))
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("connection still open after shutdown")
	}
	if _, err := net.Dial("tcp", fl.Addr().String()); err == nil {
		t.Fatal("listener still open after shutdown")
	}
}

func TestForwardTargetDownClosesTheClient(t *testing.T) {
	fl := loopback(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Forward(ctx, fl, "127.0.0.1:1", realDial)
	c, err := net.Dial("tcp", fl.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, io.ErrNoProgress) {
		t.Fatal("client was left hanging although the target is down")
	}
}

// fakeReg records calls and answers from a script.
type fakeReg struct {
	mu      sync.Mutex
	puts    []string
	deletes []string
	putErr  func(n int) error // n is the 1-based Put call number
	delHang bool
}

func (f *fakeReg) Put(_ context.Context, ns, service string, port int) error {
	f.mu.Lock()
	f.puts = append(f.puts, ns+"/"+service)
	n := len(f.puts)
	fn := f.putErr
	f.mu.Unlock()
	if fn != nil {
		return fn(n)
	}
	return nil
}

func (f *fakeReg) Delete(ctx context.Context, ns, service string) error {
	f.mu.Lock()
	f.deletes = append(f.deletes, ns+"/"+service)
	hang := f.delHang
	f.mu.Unlock()
	if hang {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func (f *fakeReg) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.puts), len(f.deletes)
}

type readyLog struct {
	mu   sync.Mutex
	vals []bool
}

func (r *readyLog) set(v bool) { r.mu.Lock(); r.vals = append(r.vals, v); r.mu.Unlock() }
func (r *readyLog) get() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.vals...)
}

type harness struct {
	cfg   config.SidecarConfig
	svcs  []Service
	reg   *fakeReg
	ready *readyLog
	waits []time.Duration
	lns   []net.Listener
}

func newHarness() *harness {
	return &harness{
		cfg:   config.SidecarConfig{NS: "pdl", ReregisterInterval: 5 * time.Minute},
		svcs:  []Service{{"admin", 8080, "127.0.0.1:1"}, {"api", 9000, "127.0.0.1:1"}},
		reg:   &fakeReg{},
		ready: &readyLog{},
	}
}

// run executes Run until the nth wait, with a fake clock that only records the requested delays.
func (h *harness) run(t *testing.T, waitsBeforeStop int) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	listen := func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		h.lns = append(h.lns, ln)
		return ln, err
	}
	wait := func(c context.Context, d time.Duration) bool {
		h.waits = append(h.waits, d)
		if len(h.waits) >= waitsBeforeStop {
			cancel()
		}
		return c.Err() == nil
	}
	return run(ctx, h.cfg, h.svcs, listen, h.reg, h.ready.set, wait, realDial)
}

func TestRunRegistersEveryServiceOnStart(t *testing.T) {
	h := newHarness()
	if err := h.run(t, 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.reg.puts[:2], ","); got != "pdl/admin,pdl/api" {
		t.Fatalf("puts = %v", h.reg.puts)
	}
	if v := h.ready.get(); len(v) < 2 || !v[0] {
		t.Fatalf("ready = %v, want true after the first complete round", v)
	}
	for _, ln := range h.lns {
		if _, err := ln.Accept(); err == nil {
			t.Fatal("a listener is still open after Run returned")
		}
	}
}

func TestRunListenFailureIsAnError(t *testing.T) {
	h := newHarness()
	calls := 0
	listen := func(network, addr string) (net.Listener, error) {
		calls++
		if calls == 2 {
			return nil, errors.New("port in use")
		}
		return net.Listen("tcp", "127.0.0.1:0")
	}
	err := run(context.Background(), h.cfg, h.svcs, listen, h.reg, h.ready.set, nil, realDial)
	if err == nil || !strings.Contains(err.Error(), "api") {
		t.Fatalf("got %v", err)
	}
	if n, _ := h.reg.counts(); n != 0 {
		t.Fatal("registered although a listener failed")
	}
}

func TestRunReassertsEveryInterval(t *testing.T) {
	h := newHarness()
	h.cfg.ReregisterInterval = 100 * time.Millisecond
	if err := h.run(t, 3); err != nil {
		t.Fatal(err)
	}
	puts, _ := h.reg.counts()
	if puts != 3*len(h.svcs) {
		t.Fatalf("puts = %d, want %d (every service, each of three rounds)", puts, 3*len(h.svcs))
	}
	for _, w := range h.waits {
		if w != 100*time.Millisecond {
			t.Fatalf("waits = %v", h.waits)
		}
	}
}

func TestRunRetriesWithBackoffOnFailure(t *testing.T) {
	h := newHarness()
	h.svcs = h.svcs[:1]
	h.reg.putErr = func(n int) error {
		if n <= 2 {
			return errors.New("dial tcp: connection refused")
		}
		return nil
	}
	if err := h.run(t, 3); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{time.Second, 2 * time.Second, 5 * time.Minute}
	for i, w := range want {
		if h.waits[i] != w {
			t.Fatalf("waits = %v, want %v", h.waits, want)
		}
	}
	v := h.ready.get()
	if len(v) < 4 || v[0] || v[1] || !v[2] {
		t.Fatalf("ready = %v, want false, false, true", v)
	}
}

func TestRunKeepsRetryingOn403And409(t *testing.T) {
	h := newHarness()
	h.svcs = h.svcs[:1]
	h.reg.putErr = func(n int) error {
		if n%2 == 0 {
			return &APIError{Status: 409}
		}
		return &APIError{Status: 403}
	}
	if err := h.run(t, 10); err != nil {
		t.Fatalf("Run must keep going on API errors, got %v", err)
	}
	want := []time.Duration{1, 2, 4, 8, 16, 32, 60, 60, 60, 60}
	for i, w := range want {
		if h.waits[i] != w*time.Second {
			t.Fatalf("waits = %v", h.waits)
		}
	}
	for _, v := range h.ready.get() {
		if v {
			t.Fatal("ready(true) although no round succeeded")
		}
	}
}

func TestRunDeletesOnShutdown(t *testing.T) {
	h := newHarness()
	if err := h.run(t, 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.reg.deletes, ","); got != "pdl/admin,pdl/api" {
		t.Fatalf("deletes = %v", h.reg.deletes)
	}
	if v := h.ready.get(); v[len(v)-1] {
		t.Fatal("still ready after shutdown")
	}
}

func TestRunShutdownIsBoundedWhenTheAPIHangs(t *testing.T) {
	old := shutdownBudget
	shutdownBudget = 200 * time.Millisecond
	defer func() { shutdownBudget = old }()
	h := newHarness()
	h.reg.delHang = true
	start := time.Now()
	if err := h.run(t, 1); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v", d)
	}
}

func TestRunNoDeleteWithoutAttemptsBeyondShutdown(t *testing.T) {
	h := newHarness()
	h.cfg.ReregisterInterval = time.Millisecond
	if err := h.run(t, 5); err != nil {
		t.Fatal(err)
	}
	if _, dels := h.reg.counts(); dels != len(h.svcs) {
		t.Fatalf("deletes = %d, want one per service at shutdown only", dels)
	}
}

func TestHTTPRegistrarMapsStatus(t *testing.T) {
	var method, path, query, ctype string
	var body string
	status := 204
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path, query, ctype = r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	reg := NewHTTPRegistrar(srv.URL+"/", srv.Client())

	if err := reg.Put(context.Background(), "pdl", "admin", 8080); err != nil {
		t.Fatal(err)
	}
	if method != "PUT" || path != "/v1/routes/admin" || ctype != "application/json" || body != `{"ns":"pdl","port":8080}` {
		t.Fatalf("request: %s %s %q %q", method, path, ctype, body)
	}
	for _, bad := range []string{"target", "addr", "100."} {
		if strings.Contains(body, bad) {
			t.Fatalf("body names an address: %s", body)
		}
	}
	for _, st := range []int{403, 409, 422, 429, 503} {
		status = st
		err := reg.Put(context.Background(), "pdl", "admin", 8080)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != st {
			t.Errorf("status %d: %v", st, err)
		}
	}
	status = 204
	if err := reg.Delete(context.Background(), "pdl", "admin"); err != nil || method != "DELETE" || path != "/v1/routes/admin" || query != "ns=pdl" {
		t.Fatalf("delete: %v %s %s %s", err, method, path, query)
	}
	srv.Close()
	if err := reg.Put(context.Background(), "pdl", "admin", 8080); err == nil {
		t.Fatal("network failure reported as success")
	} else if errors.As(err, new(*APIError)) {
		t.Fatal("network failure reported as an API error")
	}
}
