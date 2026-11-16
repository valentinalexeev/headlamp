package agent

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// fakeCentral is a TLS server that answers the headscale health path, a DERP upgrade with a
// server key frame, and a blob of blobSize bytes sent at the given pace.
func fakeCentral(t *testing.T, pace time.Duration, stallAfter int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"pass"}`)
	})
	mux.HandleFunc("GET /derp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") != "DERP" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		c, brw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: DERP\r\n" +
			"Connection: Upgrade\r\n\r\n")
		frame := []byte{derpFrameServerKey, 0, 0, 0, 40}
		frame = append(frame, derpMagic...)
		frame = append(frame, make([]byte, 32)...)
		_, _ = brw.Write(frame)
		_ = brw.Flush()
	})
	mux.HandleFunc("GET /blob", func(w http.ResponseWriter, r *http.Request) {
		chunk := make([]byte, 16<<10)
		sent := 0
		for sent < 256<<10 {
			if stallAfter > 0 && sent >= stallAfter {
				<-r.Context().Done()
				return
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			sent += len(chunk)
			time.Sleep(pace)
		}
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeSTUN answers binding requests (answer true) or reads and drops them.
func fakeSTUN(t *testing.T, answer bool) string {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 512)
		for {
			n, addr, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if !answer || n < stunHeaderLen {
				continue
			}
			resp := make([]byte, stunHeaderLen)
			binary.BigEndian.PutUint16(resp[0:2], stunBindResp)
			copy(resp[4:], buf[4:stunHeaderLen])
			_, _ = pc.WriteTo(resp, addr)
		}
	}()
	return pc.LocalAddr().String()
}

// newCheck points a check at srv as "example.com" (the name of the httptest certificate).
func newCheck(t *testing.T, srv *httptest.Server, stun string) *Pathcheck {
	t.Helper()
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	addr := srv.Listener.Addr().(*net.TCPAddr)
	return &Pathcheck{Host: "example.com", Port: addr.Port, HealthPath: "/health",
		DERPPath: "/derp", STUN: stun, BlobBytes: 256 << 10, BlobEvery: 1, DailyBytes: 1 << 30,
		Interval: time.Minute, StatePath: filepath.Join(t.TempDir(), "pathcheck.json"), TLS: cfg,
		StallAfter: 300 * time.Millisecond,
		Lookup: func(context.Context, string) ([]net.IP, error) {
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}}
}

func TestPathcheckHealthyPath(t *testing.T) {
	srv := fakeCentral(t, 0, 0)
	p := newCheck(t, srv, fakeSTUN(t, true))
	p.BlobURL = srv.URL + "/blob"
	r := p.Run(context.Background())
	if r.Class != pathClassOK {
		b, _ := json.Marshal(r)
		t.Fatalf("class %q, want ok: %s", r.Class, b)
	}
	for name, pr := range map[string]*PathProbe{"dns": r.DNS, "tcp": r.TCP, "tls": r.TLS,
		"no-sni": r.TLSNoSNI, "http": r.HTTP, "derp": r.DERP, "stun": r.STUN} {
		if pr == nil || !pr.OK {
			t.Errorf("%s: %+v", name, pr)
		}
	}
	if r.Blob == nil || !r.Blob.OK || r.Blob.Bytes != 256<<10 {
		t.Fatalf("blob %+v", r.Blob)
	}
}

func TestPathcheckFailures(t *testing.T) {
	srv := fakeCentral(t, 0, 32<<10)
	p := newCheck(t, srv, fakeSTUN(t, false))
	p.BlobURL = srv.URL + "/blob"
	p.DERPPath = "/health" // answers 200, not 101: the DERP upgrade fails
	r := p.Run(context.Background())
	want := []string{PathDERPBlocked, PathStalled, PathUDPBlocked}
	if r.Class != PathDERPBlocked || !slices.Equal(r.Findings, want) {
		t.Fatalf("class %q findings %v, want %v", r.Class, r.Findings, want)
	}
	if !r.Blob.Stalled || r.Blob.Bytes < 32<<10 || r.Blob.Bytes >= 256<<10 {
		t.Fatalf("blob %+v", r.Blob)
	}
}

func TestPathcheckBlockedAddress(t *testing.T) {
	srv := fakeCentral(t, 0, 0)
	p := newCheck(t, srv, "")
	p.Dial = func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", errReset())}
	}
	r := p.Run(context.Background())
	if r.Class != PathIPBlocked || r.TCP.Error != "reset" || r.TLS != nil {
		t.Fatalf("got %q %+v %+v", r.Class, r.TCP, r.TLS)
	}
}

func TestPathcheckDNSTampered(t *testing.T) {
	srv := fakeCentral(t, 0, 0)
	p := newCheck(t, srv, "")
	p.ExpectIPs = []net.IP{net.ParseIP("192.0.2.1")}
	r := p.Run(context.Background())
	if r.Class != PathDNSTampered || r.TCP == nil || !r.TCP.OK {
		t.Fatalf("got %q dns %+v tcp %+v", r.Class, r.DNS, r.TCP)
	}
}

func TestPathcheckTailscaleStatus(t *testing.T) {
	srv := fakeCentral(t, 0, 0)
	p := newCheck(t, srv, "")
	p.Tailscale = []string{"/usr/bin/tailscale", "status", "--json"}
	p.Exec = func(context.Context, []string) ([]byte, error) {
		return []byte(`{"BackendState":"Running","Health":["x"],"Peer":{"k":` +
			`{"ExitNode":true,"CurAddr":"","Relay":"xx","Active":true}}}`), nil
	}
	r := p.Run(context.Background())
	if r.Tailscale.ExitPath != "relay:xx" || r.Tailscale.Health != 1 || r.Class != PathDERPOnly {
		t.Fatalf("got %q %+v", r.Class, r.Tailscale)
	}
	p.Exec = func(context.Context, []string) ([]byte, error) { return nil, errors.New("x") }
	if r := p.Run(context.Background()); r.Class != pathClassOK || r.Tailscale.Error == "" {
		t.Fatalf("an unavailable status must not be a finding: %q %+v", r.Class, r.Tailscale)
	}
}

func TestClassify(t *testing.T) {
	ok, bad := &PathProbe{OK: true}, &PathProbe{Error: "timeout"}
	cases := []struct {
		name string
		r    PathResult
		want string
	}{
		{"uplink", PathResult{Ref: bad, DNS: ok, TCP: bad}, PathEdgeUplinkBad},
		{"ip", PathResult{Ref: ok, DNS: ok, TCP: bad}, PathIPBlocked},
		{"sni", PathResult{TCP: ok, TLS: bad, TLSNoSNI: ok}, PathSNIBlocked},
		{"tls", PathResult{TCP: ok, TLS: bad, TLSNoSNI: bad}, PathTLSBlocked},
		{"app", PathResult{TCP: ok, TLS: ok, HTTP: bad, DERP: ok}, PathAppBlocked},
		{"throttled", PathResult{TCP: ok, TLS: ok, Blob: &PathTransfer{OK: true, Kbps: 100},
			RefBlob: &PathTransfer{OK: true, Kbps: 1000}}, PathThrottled},
		{"not throttled", PathResult{TCP: ok, TLS: ok, Blob: &PathTransfer{OK: true, Kbps: 400},
			RefBlob: &PathTransfer{OK: true, Kbps: 1000}}, pathClassOK},
		{"tailnet", PathResult{TCP: ok, Tailscale: &PathTailscale{State: "Stopped"}},
			PathTailnetDown},
		{"ok", PathResult{DNS: ok, TCP: ok, TLS: ok}, pathClassOK},
	}
	for _, c := range cases {
		if got, _ := Classify(&c.r); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPathcheckStateAndBudget(t *testing.T) {
	srv := fakeCentral(t, 0, 0)
	p := newCheck(t, srv, "")
	p.BlobURL = srv.URL + "/blob"
	p.DailyBytes = 300 << 10 // one transfer fits, the second does not
	now := time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
	p.Now = func() time.Time { return now }
	if r := p.Run(context.Background()); r.Blob == nil {
		t.Fatal("first run: no transfer")
	}
	if r := p.Run(context.Background()); r.Blob != nil {
		t.Fatal("second run: the budget must skip the transfer")
	}
	v := p.View()
	if v.Class != pathClassOK || v.Streak != 2 || v.Counts[pathClassOK] != 2 ||
		v.BytesToday != 256<<10 || len(v.History) != 2 {
		t.Fatalf("view %+v", v)
	}
	q := &Pathcheck{StatePath: p.StatePath, Interval: time.Minute, Now: p.Now}
	q.Restore()
	if w := q.View(); w.Streak != 2 || len(w.History) != 2 || w.Last == nil {
		t.Fatalf("restored view %+v", w)
	}
	st, err := os.Stat(p.StatePath)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("state file mode: %v %v", st, err)
	}
}

func TestLoadPathcheck(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if p, err := LoadPathcheck(env(nil), "/x"); p != nil || err != nil {
		t.Fatalf("off by default: %v %v", p, err)
	}
	p, err := LoadPathcheck(env(map[string]string{pathEnvTarget: "hs.example.com",
		"INGRESS_PATHCHECK_STUN": "-", "INGRESS_PATHCHECK_DERP_PATH": "-"}), "/x")
	if err != nil || p.Host != "hs.example.com" || p.Port != 443 || p.STUN != "" ||
		p.DERPPath != "" || p.HealthPath != "/health" || p.Interval != 15*time.Minute {
		t.Fatalf("got %+v %v", p, err)
	}
	p, _ = LoadPathcheck(env(map[string]string{pathEnvTarget: "hs.example.com:8443"}), "/x")
	if p.Port != 8443 || p.STUN != "hs.example.com:3478" {
		t.Fatalf("got %+v", p)
	}
	for k, v := range map[string]string{
		pathEnvTarget:                        "bad host",
		"INGRESS_PATHCHECK_BLOB_URL":         "http://example.com/x",
		"INGRESS_PATHCHECK_INTERVAL":         "10s",
		"INGRESS_PATHCHECK_ADDRS":            "not-an-ip",
		"INGRESS_PATHCHECK_TAILSCALE":        "tailscale",
		"INGRESS_PATHCHECK_HEALTH_PATH":      "health",
		"INGRESS_PATHCHECK_BLOB_BYTES":       "0",
		"INGRESS_PATHCHECK_DAILY_BYTES":      "-1",
		"INGRESS_PATHCHECK_BLOB_EVERY":       "x",
		"INGRESS_PATHCHECK_REF_URL":          "https://user@example.com/",
		"INGRESS_PATHCHECK_TAILSCALE_SOCKET": "",
	} {
		if v == "" {
			continue
		}
		m := map[string]string{pathEnvTarget: "hs.example.com", k: v}
		_, err := LoadPathcheck(env(m), "/x")
		var ce *InfoConfigError
		if !errors.As(err, &ce) || ce.Var != k || strings.Contains(err.Error(), v) {
			t.Errorf("%s=%q: got %v", k, v, err)
		}
	}
}

func TestPathcheckEndpoint(t *testing.T) {
	s := &Server{Token: "tok"}
	h := s.Handler()
	get := func() map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/v1/pathcheck", nil)
		req.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		var m map[string]any
		_ = json.NewDecoder(bufio.NewReader(w.Body)).Decode(&m)
		return m
	}
	if m := get(); m["disabled"] != true {
		t.Fatalf("disabled: %v", m)
	}
	srv := fakeCentral(t, 0, 0)
	s.Pathcheck = newCheck(t, srv, "")
	s.Pathcheck.Run(context.Background())
	h = s.Handler()
	m := get()
	if m["class"] != pathClassOK || m["last"] == nil {
		t.Fatalf("got %v", m)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/pathcheck", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: %d", w.Code)
	}
}
