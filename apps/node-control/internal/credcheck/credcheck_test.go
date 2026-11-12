package credcheck

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func TestFingerprintShowsTheLengthAndAShortHash(t *testing.T) {
	// sha256("ab$cdef$1gh") starts with these 8 hex digits; the shortened "ab$1gh" differs.
	if got := Fingerprint("ab$cdef$1gh"); !strings.HasPrefix(got, "len=11 sha256=") || len(got) != len("len=11 sha256=")+8 {
		t.Fatal(got)
	}
	if Fingerprint("ab$cdef$1gh") == Fingerprint("ab$1gh") {
		t.Fatal("a shortened value must have another fingerprint")
	}
}

func TestNamesListsSetSecretsAndCloudPasswords(t *testing.T) {
	env := core.MapEnv(map[string]string{
		"HEADSCALE_API_KEY": "k", "OPENSTACK_CLOUDS": "ACME=https://a, bad entry ,LAB=https://b",
		"ACME_PASSWORD": "p", "LAB_PASSWORD": "", "UNRELATED": "x",
	})
	got := strings.Join(Names(env), ",")
	if got != "ACME_PASSWORD,HEADSCALE_API_KEY" {
		t.Fatal(got)
	}
}

func TestLogNeverWritesTheValue(t *testing.T) {
	secret := "s3cr3t-$value-with-dollar"
	env := core.MapEnv(map[string]string{"OIDC_CLIENT_SECRET": secret, "FLY_API_TOKEN": "fly-token-123"})
	var buf bytes.Buffer
	Log(env, slog.New(slog.NewTextHandler(&buf, nil)))
	out := buf.String()
	for _, v := range []string{secret, "fly-token-123", "s3cr3t", "fly-token"} {
		if strings.Contains(out, v) {
			t.Fatalf("log holds %q: %s", v, out)
		}
	}
	if strings.Count(out, "credential fingerprint") != 2 || !strings.Contains(out, "name=OIDC_CLIENT_SECRET") ||
		!strings.Contains(out, Fingerprint(secret)) {
		t.Fatal(out)
	}
}
