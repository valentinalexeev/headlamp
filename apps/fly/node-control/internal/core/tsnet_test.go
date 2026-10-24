package core

import (
	"errors"
	"strings"
	"testing"

	"tailscale.com/ipn"
	"tailscale.com/ipn/store/mem"
)

func TestTsnetConfigDefaults(t *testing.T) {
	c := TsnetConfigFromEnv(MapEnv(map[string]string{
		"HEADSCALE_LOGIN_SERVER": "https://hs.example.com",
		"TAILSCALE_AUTH_KEY":     "k",
	}))
	if c.ControlURL != "https://hs.example.com" || c.Hostname != "node-control" ||
		c.AuthKey != "k" || c.Dir != "/var/lib/tailscale" {
		t.Fatalf("%+v", c)
	}
	c = TsnetConfigFromEnv(MapEnv(map[string]string{"TAILNET_HOSTNAME": "node-a"}))
	if c.Hostname != "node-a" {
		t.Fatalf("%+v", c)
	}
}

func TestTsnetVerboseSwitch(t *testing.T) {
	if TsnetConfigFromEnv(MapEnv(nil)).Verbose {
		t.Fatal("verbose by default")
	}
	if !TsnetConfigFromEnv(MapEnv(map[string]string{"TSNET_VERBOSE": "1"})).Verbose {
		t.Fatal("TSNET_VERBOSE=1 not honoured")
	}
}

func TestTsnetLogWiring(t *testing.T) {
	quiet := NewTsnet(TsnetConfig{Hostname: "h", Dir: t.TempDir()})
	if quiet.srv.UserLogf == nil {
		t.Fatal("user-facing logs (login URL) must reach slog")
	}
	if quiet.srv.Logf != nil {
		t.Fatal("backend logs must stay off by default")
	}
	loud := NewTsnet(TsnetConfig{Hostname: "h", Dir: t.TempDir(), Verbose: true})
	if loud.srv.Logf == nil || loud.srv.UserLogf == nil {
		t.Fatal("TSNET_VERBOSE must enable backend logs")
	}
}

func TestScrubAuthKeyFromUpError(t *testing.T) {
	err := upError(errors.New("login failed for key tskey-abc123 at control"), "tskey-abc123")
	if strings.Contains(err.Error(), "tskey-abc123") || !strings.Contains(err.Error(), "login failed") {
		t.Fatalf("%v", err)
	}
	if upError(errors.New("x"), "").Error() == "" {
		t.Fatal("empty")
	}
}

func TestStoreIsPassedToTsnet(t *testing.T) {
	st := new(mem.Store)
	tn := NewTsnet(TsnetConfig{Hostname: "h", Dir: t.TempDir(), Store: st})
	if tn.srv.Store != ipn.StateStore(st) {
		t.Fatal("TsnetConfig.Store not set on the tsnet server")
	}
	if tn.srv.Dir == "" {
		t.Fatal("Dir must stay set")
	}
	if NewTsnet(TsnetConfig{Hostname: "h", Dir: t.TempDir()}).srv.Store != nil {
		t.Fatal("nil Store must stay nil")
	}
}
