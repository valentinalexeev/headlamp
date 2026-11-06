package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestOutcomesMapStatusCodes(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		outcome string
	}{
		{"401", 401, OutcomeUnauthorized},
		{"403", 403, OutcomeUnauthorized},
		{"409", 409, OutcomeConflict},
		{"503", 503, OutcomeRejected},
		{"400", 400, OutcomeRejected},
		{"302", 302, OutcomeRejected},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
			}))
			defer srv.Close()
			err := NewAgent(srv.URL, "tok", srv.Client()).PutCredentials(context.Background(), nil, false)
			var au *AgentUnavailable
			if !errors.As(err, &au) {
				t.Fatalf("want *AgentUnavailable, got %v", err)
			}
			if au.Outcome != c.outcome || au.Status != c.status {
				t.Fatalf("got %+v", au)
			}
		})
	}
	t.Run("connection refused", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		err := NewAgent(url, "tok", nil).PutCredentials(context.Background(), nil, false)
		var au *AgentUnavailable
		if !errors.As(err, &au) || au.Outcome != OutcomeUnreachable || au.Status != 0 {
			t.Fatalf("got %v", err)
		}
	})
}

func TestPutRequestShape(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT, gotQuery string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath, gotAuth, gotCT = r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("Content-Type")
		gotQuery = r.URL.RawQuery
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &gotBody)
		w.WriteHeader(204)
	}))
	defer srv.Close()
	creds := []Credential{{UUID: "u1", Mode: "auto", NotAfter: 5}}
	a := NewAgent(srv.URL+"/", "tok", srv.Client())
	if err := a.PutCredentials(context.Background(), creds, false); err != nil {
		t.Fatal(err)
	}
	if gotMethod != "PUT" || gotPath != "/v1/credentials" || gotAuth != "Bearer tok" || gotCT != "application/json" || gotQuery != "" {
		t.Fatalf("%s %s %s %s %q", gotMethod, gotPath, gotAuth, gotCT, gotQuery)
	}
	want := map[string]any{"credentials": []any{map[string]any{"uuid": "u1", "mode": "auto", "not_after": float64(5)}}}
	if !reflect.DeepEqual(gotBody, want) {
		t.Fatalf("body %v", gotBody)
	}
	if err := a.PutCredentials(context.Background(), []Credential{}, true); err != nil {
		t.Fatal(err)
	}
	if gotQuery != "confirm_empty=true" {
		t.Fatalf("query %q", gotQuery)
	}
	if !reflect.DeepEqual(gotBody, map[string]any{"credentials": []any{}}) {
		t.Fatalf("empty body %v", gotBody)
	}
	// nil must also marshal to [] not null
	a.PutCredentials(context.Background(), nil, true)
	if !reflect.DeepEqual(gotBody, map[string]any{"credentials": []any{}}) {
		t.Fatalf("nil body %v", gotBody)
	}
}

func TestStats(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    map[string]any
	}{
		{"ok", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != "GET" || r.URL.Path != "/v1/stats" || r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(400)
				return
			}
			w.Write([]byte(`{"abc":{"up":1}}`))
		}, map[string]any{"abc": map[string]any{"up": float64(1)}}},
		{"500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }, map[string]any{}},
		{"401", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) }, map[string]any{}},
		{"list body", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`[1]`)) }, map[string]any{}},
		{"bad json", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{`)) }, map[string]any{}},
		{"null", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`null`)) }, map[string]any{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			defer srv.Close()
			got := NewAgent(srv.URL, "tok", srv.Client()).Stats(context.Background())
			if got == nil || !reflect.DeepEqual(got, c.want) {
				t.Fatalf("got %#v", got)
			}
		})
	}
	t.Run("unreachable", func(t *testing.T) {
		srv := httptest.NewServer(http.NotFoundHandler())
		url := srv.URL
		srv.Close()
		got := NewAgent(url, "tok", nil).Stats(context.Background())
		if got == nil || len(got) != 0 {
			t.Fatalf("got %#v", got)
		}
	})
}

func TestTimeouts(t *testing.T) {
	if putTimeout != 10*time.Second || statsTimeout != 2*time.Second {
		t.Fatalf("put %v stats %v", putTimeout, statsTimeout)
	}
	old := statsTimeout
	statsTimeout = 50 * time.Millisecond
	defer func() { statsTimeout = old }()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer srv.Close()
	defer close(release)
	start := time.Now()
	got := NewAgent(srv.URL, "tok", srv.Client()).Stats(context.Background())
	if len(got) != 0 || time.Since(start) > 2*time.Second {
		t.Fatalf("got %v after %v", got, time.Since(start))
	}
}

func TestHTTPClientForUsesDial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	defer srv.Close()
	var dialed string
	hc := HTTPClientFor(func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed = addr
		var d net.Dialer
		return d.DialContext(ctx, network, srv.Listener.Addr().String())
	})
	if err := NewAgent("http://agent.invalid:8080", "tok", hc).PutCredentials(context.Background(), nil, true); err != nil {
		t.Fatal(err)
	}
	if dialed != "agent.invalid:8080" {
		t.Fatalf("dialed %q", dialed)
	}
}

func TestErrorMessageIsShort(t *testing.T) {
	e := &AgentUnavailable{Outcome: OutcomeRejected, Status: 503}
	if e.Error() != "agent answered 503" {
		t.Fatal(e.Error())
	}
	if (&AgentUnavailable{Outcome: OutcomeUnreachable}).Error() != "agent unreachable" {
		t.Fatal("unreachable msg")
	}
}

func newStatusServer(status int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
}

func dialTo(addr string, seen *[]string) func(ctx context.Context, network, a string) (net.Conn, error) {
	return func(ctx context.Context, network, a string) (net.Conn, error) {
		if seen != nil {
			*seen = append(*seen, a)
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
}

func TestAgentDoesNotFollowRedirects(t *testing.T) {
	var hits int
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(200)
		w.Write([]byte(`{"abcd1234":{"up":1}}`))
	}))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusFound)
	}))
	defer redir.Close()
	host := redir.Listener.Addr().String()
	clients := map[string]*http.Client{
		"nil fallback": nil,
		"HTTPClientFor": HTTPClientFor(func(ctx context.Context, n, a string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, n, a)
		}),
	}
	for name, hc := range clients {
		t.Run(name, func(t *testing.T) {
			a := NewAgent("http://"+host, "tok", hc)
			err := a.PutCredentials(context.Background(), nil, false)
			var au *AgentUnavailable
			if !errors.As(err, &au) || au.Outcome != OutcomeRejected || au.Status != 302 {
				t.Fatalf("put: %v", err)
			}
			if got := a.Stats(context.Background()); len(got) != 0 {
				t.Fatalf("stats followed a redirect: %v", got)
			}
			if hits != 0 {
				t.Fatalf("redirect target was contacted %d times", hits)
			}
		})
	}
}

func TestAgentClientsIgnoreProxyEnvironment(t *testing.T) {
	t.Run("HTTPClientFor", func(t *testing.T) {
		tr := HTTPClientFor(nil).Transport.(*http.Transport)
		if tr.Proxy != nil {
			t.Fatal("Transport.Proxy must be nil")
		}
	})
	t.Run("nil fallback", func(t *testing.T) {
		a := NewAgent("http://agent.test", "tok", nil).(*agentClient)
		tr, ok := a.hc.Transport.(*http.Transport)
		if !ok || tr.Proxy != nil {
			t.Fatalf("fallback transport %T must have no proxy", a.hc.Transport)
		}
	})
	t.Run("dials the agent address", func(t *testing.T) {
		t.Setenv("HTTP_PROXY", "http://proxy.test:3128")
		t.Setenv("http_proxy", "http://proxy.test:3128")
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
		defer srv.Close()
		var seen []string
		hc := HTTPClientFor(dialTo(srv.Listener.Addr().String(), &seen))
		if err := NewAgent("http://agent.test:8080", "tok", hc).PutCredentials(context.Background(), nil, false); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || seen[0] != "agent.test:8080" {
			t.Fatalf("dialed %v", seen)
		}
	})
}

func TestStatsBodyIsLimited(t *testing.T) {
	serve := func(n int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"k":"`))
			w.Write(bytes.Repeat([]byte("a"), n))
			w.Write([]byte(`"}`))
		}))
	}
	t.Run("under the limit", func(t *testing.T) {
		srv := serve(1000)
		defer srv.Close()
		if got := NewAgent(srv.URL, "tok", srv.Client()).Stats(context.Background()); len(got) != 1 {
			t.Fatalf("%v", got)
		}
	})
	t.Run("over the limit", func(t *testing.T) {
		srv := serve(2 << 20)
		defer srv.Close()
		got := NewAgent(srv.URL, "tok", srv.Client()).Stats(context.Background())
		if got == nil || len(got) != 0 {
			t.Fatalf("want empty non-nil map, got %d keys", len(got))
		}
	})
}
