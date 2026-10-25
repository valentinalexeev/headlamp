package config

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func base() map[string]string {
	return map[string]string{"FUNNEL_CONFIG_URL": "http://100.64.0.1:8080", "TS_AUTHKEY": "tskey-auth-secret"}
}

func with(kv ...string) map[string]string {
	m := base()
	for i := 0; i < len(kv); i += 2 {
		if kv[i+1] == "-" {
			delete(m, kv[i])
		} else {
			m[kv[i]] = kv[i+1]
		}
	}
	return m
}

func goodKey() string { return base64.StdEncoding.EncodeToString(make([]byte, 32)) }

func TestLoadDefaults(t *testing.T) {
	c, err := Load(env(base()))
	if err != nil {
		t.Fatal(err)
	}
	if c.FunnelTag != "tag:funnel" || c.NSTagPrefix != "tag:ns-" || c.SyncInterval != 30*time.Second ||
		c.StateDir != "/data" || c.ProxyAddr != ":8080" || c.APIPort != 9100 || c.MissingChecks != 3 ||
		c.ReconcileInterval != 60*time.Second || c.MaxRoutesPerNode != 20 || c.MaxBodyBytes != 33554432 ||
		c.ConnectTimeout != 10*time.Second || c.IdleTimeout != 120*time.Second || c.RequestTimeout != 300*time.Second ||
		c.Hostname != "funnel-ingress" || c.RedisKey != "funnel-ingress:tsnet" || c.AuthKey != "tskey-auth-secret" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
}

func TestLoadOverrides(t *testing.T) {
	c, err := Load(env(with(
		"FUNNEL_CONFIG_URL", "-", "FUNNEL_CONFIG_FILE", "/etc/funnel.json",
		"FUNNEL_TAG", "tag:pub", "FUNNEL_NS_TAG_PREFIX", "tag:space-", "FUNNEL_SYNC_INTERVAL", "10s",
		"FUNNEL_API_PORT", "9200", "FUNNEL_MAX_BODY_BYTES", "1024", "FLY_APP_NAME", "my-app",
		"REDIS_URL", "redis://:pw@host:6379", "FUNNEL_STATE_KEY", goodKey(), "FUNNEL_CONTROL_URL", "https://hs.example.com",
	)))
	if err != nil {
		t.Fatal(err)
	}
	if c.FunnelTag != "tag:pub" || c.NSTagPrefix != "tag:space-" || c.SyncInterval != 10*time.Second || c.APIPort != 9200 ||
		c.MaxBodyBytes != 1024 || c.ConfigFile != "/etc/funnel.json" || c.RedisKey != "my-app:tsnet" || c.ControlURL != "https://hs.example.com" {
		t.Fatalf("overrides not applied: %+v", c)
	}
}

func TestValidateRanges(t *testing.T) {
	for _, ok := range [][]PortRange{{{8080, 8080}}, {{9000, 9010}}, {{1, 65535}}, nil} {
		if err := ValidateRanges(ok); err != nil {
			t.Errorf("rejected %v: %v", ok, err)
		}
	}
	for _, bad := range [][]PortRange{{{9010, 9000}}, {{0, 5}}, {{1, 65536}}, {{-1, 3}}, {{80, 80}, {0, 0}}} {
		if err := ValidateRanges(bad); err == nil {
			t.Errorf("accepted %v", bad)
		}
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
	}{
		{"neither source", with("FUNNEL_CONFIG_URL", "-")},
		{"both sources", with("FUNNEL_CONFIG_FILE", "/x.json")},
		{"config url not http", with("FUNNEL_CONFIG_URL", "ftp://x")},
		{"config url no host", with("FUNNEL_CONFIG_URL", "http://")},
		{"control url not http", with("FUNNEL_CONTROL_URL", "headscale")},
		{"no auth key", with("TS_AUTHKEY", "-")},
		{"funnel tag without prefix", with("FUNNEL_TAG", "funnel")},
		{"bare funnel tag", with("FUNNEL_TAG", "tag:")},
		{"ns prefix without tag", with("FUNNEL_NS_TAG_PREFIX", "ns-")},
		{"sync interval too short", with("FUNNEL_SYNC_INTERVAL", "2s")},
		{"sync interval garbage", with("FUNNEL_SYNC_INTERVAL", "soon")},
		{"api port zero", with("FUNNEL_API_PORT", "0")},
		{"api port too large", with("FUNNEL_API_PORT", "70000")},
		{"api port garbage", with("FUNNEL_API_PORT", "x")},
		{"missing checks zero", with("FUNNEL_MISSING_CHECKS", "0")},
		{"redis key without colon", with("REDIS_KEY", "tsnet")},
		{"redis key empty app", with("REDIS_KEY", ":tsnet")},
		{"redis key glob star", with("REDIS_KEY", "app:*")},
		{"redis key glob question", with("REDIS_KEY", "app:t?net")},
		{"redis key glob bracket", with("REDIS_KEY", "app:[a]")},
		{"rediss url", with("REDIS_URL", "rediss://:pw@h:1", "FUNNEL_STATE_KEY", goodKey())},
		{"redis url without scheme", with("REDIS_URL", "h:6379", "FUNNEL_STATE_KEY", goodKey())},
		{"redis url without state key", with("REDIS_URL", "redis://:pw@h:6379")},
		{"state key not base64", with("FUNNEL_STATE_KEY", "!!!")},
		{"state key wrong length", with("FUNNEL_STATE_KEY", base64.StdEncoding.EncodeToString(make([]byte, 16)))},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Load(env(c.env)); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestPlaintextOptIn(t *testing.T) {
	m := with("REDIS_URL", "redis://:pw@h:6379")
	if _, err := Load(env(m)); err == nil {
		t.Fatal("redis without a state key must be refused")
	}
	m["STATE_ALLOW_PLAINTEXT"] = "1"
	c, err := Load(env(m))
	if err != nil || !c.AllowPlaintext {
		t.Fatalf("explicit opt-in refused: %v", err)
	}
}

func TestLoadErrorsNeverEchoSecrets(t *testing.T) {
	secrets := []string{"s3cretpass", "tskey-auth-topsecret", "c2VjcmV0LWtleS1ub3QtMzItYnl0ZXM="}
	cases := []map[string]string{
		with("REDIS_URL", "rediss://user:s3cretpass@host:6379"),
		with("REDIS_URL", "redis://user:s3cretpass@host:6379"),
		with("REDIS_URL", "redis://user:s3cretpass@host:6379", "FUNNEL_STATE_KEY", secrets[2]),
		with("FUNNEL_STATE_KEY", secrets[2]),
		with("TS_AUTHKEY", secrets[1], "FUNNEL_CONFIG_URL", "ftp://u:s3cretpass@x"),
		with("TS_AUTHKEY", secrets[1], "FUNNEL_CONTROL_URL", "://user:s3cretpass@x"),
		with("TS_AUTHKEY", secrets[1], "REDIS_KEY", "bad*key"),
		with("TS_AUTHKEY", secrets[1], "FUNNEL_API_PORT", "s3cretpass"),
	}
	for i, m := range cases {
		_, err := Load(env(m))
		if err == nil {
			t.Fatalf("case %d accepted", i)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Errorf("case %d: error leaks %q: %v", i, s, err)
			}
		}
	}
}
