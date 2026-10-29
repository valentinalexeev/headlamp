package tasks

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestSessionClient(t *testing.T, h http.Handler, token string) SessionClient {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return NewSessionClient(&dialTailnet{target: srv.Listener.Addr().String()}, 8123, token)
}

func TestSessionClientRoundTrip(t *testing.T) {
	var got map[string]int
	var auth string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(201)
		_, _ = w.Write([]byte(`{"id":"s1","port":6080,"expires_at":"2026-01-01T01:00:00Z"}`))
	})
	mux.HandleFunc("GET /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"session":{"id":"s1","state":"running","started_at":"2026-01-01T00:00:00Z","expires_at":"2026-01-01T01:00:00Z","port":6080,"reason":""}}`))
	})
	mux.HandleFunc("DELETE /v1/sessions/s1", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	c := newTestSessionClient(t, mux, "tok")
	ctx := context.Background()
	s, err := c.Start(ctx, "100.64.0.9", 1800, 300)
	if err != nil || s.ID != "s1" || s.Port != 6080 || s.ExpiresAt.IsZero() {
		t.Fatalf("Start = %+v, %v", s, err)
	}
	if got["ttl_seconds"] != 1800 || got["idle_seconds"] != 300 || auth != "Bearer tok" {
		t.Fatalf("sent %v, auth %q", got, auth)
	}
	cur, err := c.Current(ctx, "100.64.0.9")
	if err != nil || cur == nil || cur.State != "running" || cur.Port != 6080 {
		t.Fatalf("Current = %+v, %v", cur, err)
	}
	if err := c.Stop(ctx, "100.64.0.9", "s1"); err != nil {
		t.Fatal(err)
	}
}

func TestSessionClientNoSessionAndErrors(t *testing.T) {
	status := 200
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"session":null}`))
	})
	c := newTestSessionClient(t, mux, "tok-secret")
	ctx := context.Background()
	if cur, err := c.Current(ctx, "100.64.0.9"); err != nil || cur != nil {
		t.Fatalf("Current = %+v, %v", cur, err)
	}
	for code, want := range map[int]error{409: ErrRunnerBusy, 422: ErrRunnerRejected, 401: ErrRunnerUnauthorized, 404: ErrRunUnknown} {
		status = code
		if _, err := c.Start(ctx, "100.64.0.9", 60, 60); !errors.Is(err, want) {
			t.Fatalf("Start %d: %v, want %v", code, err, want)
		}
	}
	status = 404
	if err := c.Stop(ctx, "100.64.0.9", "x"); !errors.Is(err, ErrRunUnknown) {
		t.Fatalf("Stop 404: %v", err)
	}
	if _, err := c.Current(ctx, "100.64.0.9"); err == nil {
		t.Fatal("Current on a node with sessions disabled must be an error")
	}
}

func TestSessionClientScrubsToken(t *testing.T) {
	tn := &dialTailnet{target: "x", fail: true}
	c := NewSessionClient(tn, 8123, "tok-secret-abc")
	_, err := c.Current(context.Background(), "tok-secret-abc")
	if err == nil || strings.Contains(err.Error(), "tok-secret-abc") {
		t.Fatalf("err = %v", err)
	}
}
