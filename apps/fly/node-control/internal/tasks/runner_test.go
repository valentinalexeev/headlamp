package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"example.com/node-control/internal/core"
)

// dialTailnet connects every dial to the test server whatever address was
// asked for, and records what was asked.
type dialTailnet struct {
	core.Tailnet
	target string
	mu     sync.Mutex
	asked  []string
	fail   bool
}

func (d *dialTailnet) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	d.mu.Lock()
	d.asked = append(d.asked, addr)
	d.mu.Unlock()
	if d.fail {
		return nil, errors.New("dial " + addr + ": unreachable")
	}
	var nd net.Dialer
	return nd.DialContext(ctx, network, d.target)
}

func newTestClient(t *testing.T, h http.Handler, token string, secrets []string) (Runner, *dialTailnet) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	tn := &dialTailnet{target: srv.Listener.Addr().String()}
	return NewRunnerClient(tn, 8123, token, secrets), tn
}

func TestClientStartStatusCancel(t *testing.T) {
	var gotStart map[string]any
	var cancelled bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/runs", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotStart)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"r1"}`))
	})
	mux.HandleFunc("GET /v1/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"succeeded","result":{"n":2},"error":"","log_tail":"done"}`))
	})
	mux.HandleFunc("DELETE /v1/runs/r1", func(w http.ResponseWriter, r *http.Request) {
		cancelled = true
		w.WriteHeader(http.StatusNoContent)
	})
	c, tn := newTestClient(t, mux, "tok", nil)
	ctx := context.Background()

	id, err := c.Start(ctx, "100.64.0.9", StartReq{
		Script: "echo", Params: map[string]any{"a": "b"},
		Secrets: map[string]string{"S": "v"}, TimeoutSeconds: 30,
	})
	if err != nil || id != "r1" {
		t.Fatalf("Start = %q, %v", id, err)
	}
	if gotStart["script"] != "echo" || gotStart["timeout_seconds"] != float64(30) ||
		gotStart["params"].(map[string]any)["a"] != "b" ||
		gotStart["secrets"].(map[string]any)["S"] != "v" {
		t.Errorf("start body = %v", gotStart)
	}
	if len(tn.asked) != 1 || tn.asked[0] != "100.64.0.9:8123" {
		t.Errorf("dialled %v", tn.asked)
	}

	st, err := c.Status(ctx, "100.64.0.9", "r1")
	if err != nil || st.State != "succeeded" || st.Result["n"] != float64(2) || st.LogTail != "done" {
		t.Fatalf("Status = %+v, %v", st, err)
	}
	if err := c.Cancel(ctx, "100.64.0.9", "r1"); err != nil || !cancelled {
		t.Fatalf("Cancel = %v cancelled=%v", err, cancelled)
	}
}

func TestClientStartNilParamsSendsEmptyObject(t *testing.T) {
	var raw map[string]json.RawMessage
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"x"}`))
	}), "tok", nil)
	if _, err := c.Start(context.Background(), "1.2.3.4", StartReq{Script: "s"}); err != nil {
		t.Fatal(err)
	}
	if string(raw["params"]) != "{}" {
		t.Errorf("params = %s", raw["params"])
	}
}

func TestClientBusyMapsToErrRunnerBusy(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
	}), "tok", nil)
	if _, err := c.Start(context.Background(), "1.2.3.4", StartReq{Script: "s"}); !errors.Is(err, ErrRunnerBusy) {
		t.Errorf("got %v", err)
	}
}

func TestClientRejectedAndUnauthorized(t *testing.T) {
	code := http.StatusUnprocessableEntity
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		_, _ = w.Write([]byte("body text must not leak"))
	}), "tok", nil)
	_, err := c.Start(context.Background(), "1.2.3.4", StartReq{Script: "s"})
	if !errors.Is(err, ErrRunnerRejected) || !strings.Contains(err.Error(), "422") || strings.Contains(err.Error(), "leak") {
		t.Errorf("422: %v", err)
	}
	code = http.StatusUnauthorized
	if _, err := c.Start(context.Background(), "1.2.3.4", StartReq{Script: "s"}); !errors.Is(err, ErrRunnerUnauthorized) {
		t.Errorf("401: %v", err)
	}
	code = http.StatusBadGateway
	_, err = c.Start(context.Background(), "1.2.3.4", StartReq{Script: "s"})
	if err == nil || errors.Is(err, ErrRunnerRejected) || errors.Is(err, ErrRunnerBusy) {
		t.Errorf("5xx: %v", err)
	}
}

func TestClientUnknownRunMapsToErrRunUnknown(t *testing.T) {
	c, _ := newTestClient(t, http.NotFoundHandler(), "tok", nil)
	if _, err := c.Status(context.Background(), "1.2.3.4", "nope"); !errors.Is(err, ErrRunUnknown) {
		t.Errorf("Status: %v", err)
	}
	if err := c.Cancel(context.Background(), "1.2.3.4", "nope"); !errors.Is(err, ErrRunUnknown) {
		t.Errorf("Cancel: %v", err)
	}
}

func TestClientStatusUnknownStateIsError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"weird","result":null}`))
	}), "tok", nil)
	if _, err := c.Status(context.Background(), "1.2.3.4", "r"); err == nil {
		t.Error("unknown state accepted")
	}
}

func TestClientStatusNullResultIsNil(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"running","result":null,"error":"","log_tail":""}`))
	}), "tok", nil)
	st, err := c.Status(context.Background(), "1.2.3.4", "r")
	if err != nil || st.State != "running" || st.Result != nil {
		t.Errorf("%+v %v", st, err)
	}
}

func TestClientHealthFalseWhenBusy(t *testing.T) {
	busy := true
	code := http.StatusOK
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(code)
		if code == http.StatusOK {
			b, _ := json.Marshal(map[string]any{"version": "v", "busy": busy, "mem_available_mb": 100})
			_, _ = w.Write(b)
		}
	}), "tok", nil)
	ctx := context.Background()
	if ok, err := c.Health(ctx, "1.2.3.4"); ok || err != nil {
		t.Errorf("busy: %v %v", ok, err)
	}
	busy = false
	if ok, err := c.Health(ctx, "1.2.3.4"); !ok || err != nil {
		t.Errorf("idle: %v %v", ok, err)
	}
	code = http.StatusInternalServerError
	if ok, err := c.Health(ctx, "1.2.3.4"); ok || err != nil {
		t.Errorf("500: %v %v", ok, err)
	}
	code = http.StatusUnauthorized
	if ok, err := c.Health(ctx, "1.2.3.4"); ok || !errors.Is(err, ErrRunnerUnauthorized) {
		t.Errorf("401: %v %v", ok, err)
	}
}

func TestClientHealthTransportError(t *testing.T) {
	tn := &dialTailnet{fail: true}
	c := NewRunnerClient(tn, 1, "tok", nil)
	if ok, err := c.Health(context.Background(), "1.2.3.4"); ok || err == nil {
		t.Errorf("got %v %v", ok, err)
	}
}

func TestClientErrorsNeverContainTokenOrSecrets(t *testing.T) {
	const token, secret = "TOKEN-abc123", "SECRET-xyz789"
	// The 5xx body echoes both; the dial error echoes the address, which here
	// carries them too.
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte("boom " + token + " " + secret))
	}), token, []string{secret})
	ctx := context.Background()
	check := func(what string, err error) {
		t.Helper()
		if err == nil {
			t.Errorf("%s: no error", what)
			return
		}
		if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), secret) {
			t.Errorf("%s leaks: %v", what, err)
		}
	}
	_, err := c.Start(ctx, "1.2.3.4", StartReq{Script: "s", Secrets: map[string]string{"K": secret}})
	check("start", err)
	_, err = c.Status(ctx, "1.2.3.4", "r")
	check("status", err)
	check("cancel", c.Cancel(ctx, "1.2.3.4", "r"))
	_, err = c.Health(ctx, "1.2.3.4")
	_ = err // 500 on health is (false, nil)

	bad := NewRunnerClient(&dialTailnet{fail: true}, 1, token, []string{secret})
	_, err = bad.Start(ctx, token+secret, StartReq{Script: "s"})
	check("dial", err)
	_, err = bad.Health(ctx, secret)
	check("health dial", err)
}

func TestClientSendsBearerToken(t *testing.T) {
	var got []string
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("Authorization"))
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r"}`))
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = w.Write([]byte(`{"state":"running","busy":false}`))
		}
	}), "tok-1", nil)
	ctx := context.Background()
	_, _ = c.Health(ctx, "1.2.3.4")
	_, _ = c.Start(ctx, "1.2.3.4", StartReq{Script: "s"})
	_, _ = c.Status(ctx, "1.2.3.4", "r")
	_ = c.Cancel(ctx, "1.2.3.4", "r")
	if len(got) != 4 {
		t.Fatalf("requests = %d", len(got))
	}
	for i, h := range got {
		if h != "Bearer tok-1" {
			t.Errorf("request %d auth = %q", i, h)
		}
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	hit := false
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/elsewhere" {
			hit = true
		}
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}), "tok", nil)
	if _, err := c.Status(context.Background(), "1.2.3.4", "r"); err == nil {
		t.Error("redirect treated as success")
	}
	if hit {
		t.Error("redirect followed")
	}
}
