package main

import (
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func TestServeSyncConfig(t *testing.T) {
	ok := map[string]string{
		"SERVE_SYNC_URL": "http://node-control", "SERVE_RECORDS_PATH": "/data/extra-records.json",
		"SERVE_BASE_DOMAIN": "vpn.example.com", "HEADSCALE_LOGIN_SERVER": "https://hs.example.com",
	}
	s, cfg, err := serveSyncConfig(core.MapEnv(ok))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Hostname != "headscale-sync" || cfg.Dir != "/data/serve-sync" || s.Every.Seconds() != 15 {
		t.Fatalf("defaults %+v %v", cfg, s.Every)
	}
	for _, drop := range []string{"SERVE_SYNC_URL", "SERVE_RECORDS_PATH", "SERVE_BASE_DOMAIN", "HEADSCALE_LOGIN_SERVER"} {
		env := map[string]string{}
		for k, v := range ok {
			if k != drop {
				env[k] = v
			}
		}
		if _, _, err := serveSyncConfig(core.MapEnv(env)); err == nil {
			t.Errorf("started without %s", drop)
		}
	}
	for _, bad := range []string{"1", "abc", "99999"} {
		env := map[string]string{"SERVE_SYNC_SECONDS": bad}
		for k, v := range ok {
			env[k] = v
		}
		if _, _, err := serveSyncConfig(core.MapEnv(env)); err == nil || !strings.Contains(err.Error(), "SERVE_SYNC_SECONDS") {
			t.Errorf("SERVE_SYNC_SECONDS=%s: %v", bad, err)
		}
	}
}
