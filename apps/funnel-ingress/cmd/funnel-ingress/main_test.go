package main

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func stateKey() string {
	return base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
}

func TestRunRefusesBadConfig(t *testing.T) {
	err := run(context.Background(), env(map[string]string{}))
	if err == nil || !strings.Contains(err.Error(), "configuration") {
		t.Fatalf("got %v", err)
	}
	err = run(context.Background(), env(map[string]string{"FUNNEL_CONFIG_FILE": "/x", "TS_AUTHKEY": "k", "FUNNEL_PROXY_ADDR": "nonsense"}))
	if err == nil || !strings.Contains(err.Error(), "FUNNEL_PROXY_ADDR") {
		t.Fatalf("got %v", err)
	}
}

func TestRunExitsBeforeTheNodeWhenRedisIsDown(t *testing.T) {
	mr := miniredis.RunT(t)
	url := "redis://:topsecret@" + mr.Addr()
	mr.Close()
	done := make(chan error, 1)
	go func() {
		done <- run(context.Background(), env(map[string]string{
			"FUNNEL_CONFIG_FILE": "/x", "TS_AUTHKEY": "tskey-secret", "REDIS_URL": url, "FUNNEL_STATE_KEY": stateKey(),
			"FUNNEL_STATE_DIR": t.TempDir(),
		}))
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("started with Redis down")
		}
		for _, s := range []string{"topsecret", "tskey-secret", stateKey()} {
			if strings.Contains(err.Error(), s) {
				t.Fatalf("error leaks a secret: %v", err)
			}
		}
	case <-time.After(30 * time.Second):
		t.Fatal("run did not fail fast")
	}
}

func TestRunRefusesACorruptRouteBlob(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "funnel-kv.json"), []byte(`{"funnel/routes":"Z2FyYmFnZQ=="}`), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(context.Background(), env(map[string]string{"FUNNEL_CONFIG_FILE": "/x", "TS_AUTHKEY": "k", "FUNNEL_STATE_DIR": dir}))
	if err == nil || !strings.Contains(err.Error(), "unreadable") {
		t.Fatalf("got %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "funnel-kv.json"))
	if string(b) != `{"funnel/routes":"Z2FyYmFnZQ=="}` {
		t.Fatal("the corrupt blob was modified")
	}
}

func TestPortOf(t *testing.T) {
	for in, want := range map[string]int{":8080": 8080, "0.0.0.0:80": 80, "[::]:443": 443} {
		if got, err := portOf(in); err != nil || got != want {
			t.Errorf("portOf(%q) = %d, %v", in, got, err)
		}
	}
	for _, in := range []string{"8080", ":0", ":70000", ":abc", ""} {
		if _, err := portOf(in); err == nil {
			t.Errorf("portOf(%q) accepted", in)
		}
	}
}

func TestRunReportsAnUnusableStateDirectory(t *testing.T) {
	mr := miniredis.RunT(t)
	err := run(context.Background(), env(map[string]string{
		"FUNNEL_CONFIG_FILE": "/x", "TS_AUTHKEY": "tskey-secret", "REDIS_URL": "redis://" + mr.Addr(), "FUNNEL_STATE_KEY": stateKey(),
		"FUNNEL_STATE_DIR": "/dev/null/nope",
	}))
	if err == nil || !strings.Contains(err.Error(), "FUNNEL_STATE_DIR") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(err.Error(), "tskey-secret") {
		t.Fatal("the auth key leaked")
	}
}
