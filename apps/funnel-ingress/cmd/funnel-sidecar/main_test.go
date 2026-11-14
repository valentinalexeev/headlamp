package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestRunRefusesBadConfig(t *testing.T) {
	err := run(context.Background(), env(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("got %v", err)
	}
	err = run(context.Background(), env(map[string]string{
		"FUNNEL_NS": "pdl", "FUNNEL_SERVICES": "a=80,a=81", "FUNNEL_API_URL": "http://x:9100", "TS_AUTHKEY": "k",
	}))
	if err == nil || !strings.Contains(err.Error(), "FUNNEL_SERVICES") {
		t.Fatalf("got %v", err)
	}
}

func TestWatchTailnetLogsOnlyChanges(t *testing.T) {
	type st struct {
		state  string
		health []string
		err    error
	}
	script := []st{
		{"Running", nil, nil},
		{"Running", nil, nil},
		{"Running", []string{"unable to connect to the coordination server"}, nil},
		{"Running", []string{"unable to connect to the coordination server"}, nil},
		{"", nil, errors.New("boom")},
		{"NeedsLogin", nil, nil},
	}
	i := 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	status := func(context.Context) (string, []string, error) {
		s := script[i]
		i++
		if i == len(script) {
			cancel()
		}
		return s.state, s.health, s.err
	}
	var lines []string
	logf := func(_ bool, msg string, args ...any) {
		lines = append(lines, fmt.Sprint(append([]any{msg}, args...)...))
	}
	watchTailnet(ctx, status, time.Millisecond, logf)
	if len(lines) != 4 {
		t.Fatalf("logged %d lines, want 4 (start, health change, read error, state change): %q", len(lines), lines)
	}
	if !strings.Contains(lines[1], "coordination server") || !strings.Contains(lines[3], "NeedsLogin") {
		t.Fatalf("lines = %q", lines)
	}
}

func TestReadyzReportsRegistrationAge(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var st readiness
	h := st.handler(func() time.Time { return now })

	get := func() (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
		if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
			t.Fatalf("content type %q", ct)
		}
		return rec.Code, body
	}

	code, body := get()
	if code != 503 || body["ready"] != false || body["last_registered"] != nil || body["age_seconds"] != nil {
		t.Fatalf("before any registration: %d %v", code, body)
	}

	st.set(now.Add(-90*time.Second), true)
	code, body = get()
	if code != 200 || body["ready"] != true || body["last_registered"] != "2026-01-01T11:58:30Z" || body["age_seconds"] != float64(90) {
		t.Fatalf("registered: %d %v", code, body)
	}

	st.setAddresses(netip.MustParseAddr("100.64.0.7"), netip.Addr{})
	if _, body = get(); fmt.Sprint(body["addresses"]) != "[100.64.0.7]" {
		t.Fatalf("addresses: %v", body)
	}

	st.set(now, false) // a failed round keeps the time of the last success
	code, body = get()
	if code != 503 || body["ready"] != false || body["age_seconds"] != float64(90) {
		t.Fatalf("after a failed round: %d %v", code, body)
	}
}
