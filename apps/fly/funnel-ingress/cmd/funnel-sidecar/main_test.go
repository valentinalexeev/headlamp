package main

import (
	"context"
	"strings"
	"testing"
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
