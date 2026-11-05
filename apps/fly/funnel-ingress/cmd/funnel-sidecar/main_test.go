package main

import (
	"context"
	"errors"
	"fmt"
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
