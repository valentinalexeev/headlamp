package agent

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSocks is a SOCKS5 server that connects every request to target, or answers reply != 0 (refused).
func fakeSocks(t *testing.T, target string, reply byte) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				b := make([]byte, 3)
				if _, err := io.ReadFull(c, b); err != nil {
					return
				}
				_, _ = c.Write([]byte{5, 0})
				h := make([]byte, 5)
				if _, err := io.ReadFull(c, h); err != nil || h[3] != 3 {
					return
				}
				name := make([]byte, int(h[4])+2)
				_, _ = io.ReadFull(c, name)
				if reply != 0 {
					_, _ = c.Write([]byte{5, reply, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				u, err := net.Dial("tcp", target)
				if err != nil {
					_, _ = c.Write([]byte{5, 4, 0, 1, 0, 0, 0, 0, 0, 0})
					return
				}
				defer u.Close()
				resp := []byte{5, 0, 0, 1, 127, 0, 0, 1}
				resp = binary.BigEndian.AppendUint16(resp, 443)
				_, _ = c.Write(resp)
				go func() { _, _ = io.Copy(u, c) }()
				_, _ = io.Copy(c, u)
			}(c)
		}
	}()
	return ln.Addr().String()
}

func probeEnv(t *testing.T, reply byte) (*Egress, *httptest.Server) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodHead || r.URL.Path != "/generate_204" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	socks := fakeSocks(t, srv.Listener.Addr().String(), reply)
	cfg := srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	cfg.InsecureSkipVerify = false
	// httptest certificates are for example.com: the probe asks the SOCKS for that name.
	e := &Egress{Socks: socks, URL: "https://example.com/generate_204", TLS: cfg, Timeout: 5 * time.Second}
	return e, srv
}

func TestEgressProbeThroughSocks(t *testing.T) {
	e, _ := probeEnv(t, 0)
	r := e.Probe(context.Background())
	if !r.OK || r.Status != 204 || r.Error != "" || r.Target != "example.com" || r.Checked == 0 || r.Since != r.Checked {
		t.Fatalf("%+v", r)
	}
	// a second OK probe keeps Since
	time.Sleep(1100 * time.Millisecond)
	r2 := e.Probe(context.Background())
	if !r2.OK || r2.Since != r.Since || r2.Checked == r.Checked {
		t.Fatalf("since must hold while the state holds: %+v then %+v", r, r2)
	}
}

func TestEgressProbeRefusedByExitNode(t *testing.T) {
	e, _ := probeEnv(t, 2) // "connection not allowed by ruleset", as when the policy drops the traffic
	r := e.Probe(context.Background())
	if r.OK || !strings.Contains(r.Error, "refused") || r.Status != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestEgressProbeNoSocks(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()
	e := &Egress{Socks: addr, URL: "https://example.com/", Timeout: 2 * time.Second}
	if r := e.Probe(context.Background()); r.OK || r.Error != "SOCKS port unreachable" {
		t.Fatalf("%+v", r)
	}
}

func TestEgressEndpoint(t *testing.T) {
	get := func(s *Server) map[string]any {
		req := httptest.NewRequest(http.MethodGet, "/v1/egress", nil)
		req.Header.Set("Authorization", "Bearer tok")
		w := httptest.NewRecorder()
		s.Handler().ServeHTTP(w, req)
		if w.Code != 200 {
			t.Fatalf("status %d", w.Code)
		}
		var out map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	if out := get(&Server{Token: "tok"}); out["disabled"] != true {
		t.Fatalf("without a probe: %v", out)
	}
	e, _ := probeEnv(t, 0)
	e.Probe(context.Background())
	if out := get(&Server{Token: "tok", Egress: e}); out["ok"] != true || out["status"] != float64(204) {
		t.Fatalf("%v", out)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/egress", nil)
	w := httptest.NewRecorder()
	(&Server{Token: "tok", Egress: e}).Handler().ServeHTTP(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("without the token: %d", w.Code)
	}
}

func TestLoadEgress(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if e, err := LoadEgress(env(nil)); e != nil || err != nil {
		t.Fatalf("off by default: %v %v", e, err)
	}
	e, err := LoadEgress(env(map[string]string{"INGRESS_EGRESS_SOCKS": "127.0.0.1:1055"}))
	if err != nil || e.URL != defaultEgressURL {
		t.Fatalf("%+v %v", e, err)
	}
	for _, bad := range []map[string]string{
		{"INGRESS_EGRESS_SOCKS": "1055"},
		{"INGRESS_EGRESS_SOCKS": "127.0.0.1:1055", "INGRESS_EGRESS_URL": "http://example.com/"},
		{"INGRESS_EGRESS_SOCKS": "127.0.0.1:1055", "INGRESS_EGRESS_URL": "https://u:p@example.com/"},
	} {
		if _, err := LoadEgress(env(bad)); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}
