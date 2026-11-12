// Package credcheck logs a fingerprint of each configured secret at start (#302).
//
// A fingerprint is the length and the first 8 hex digits of the SHA-256 of the value. An operator
// compares it with the same fingerprint of the source value, to see whether the value reached
// node-control unchanged. Docker Compose, for example, expands "$name" in an unquoted env_file
// value and silently shortens a password. A fingerprint holds no part of the secret.
package credcheck

import (
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// fixed names the secret variables that are not per cloud.
var fixed = []string{
	"HEADSCALE_API_KEY", "TAILSCALE_AUTH_KEY", "ENROLL_SECRET_KEY", "OIDC_CLIENT_SECRET",
	"NODE_CONTROL_STATE_KEY", "FLY_API_TOKEN", "SELECTEL_PASSWORD", "AWS_SECRET_ACCESS_KEY",
	"OCI_PRIVATE_KEY", "YANDEX_SA_KEY_JSON", "TASKS_RUNNER_TOKEN", "TASKS_HEARTBEAT_SECRET",
	"FUNNEL_DNS_CLOUDFLARE_TOKEN", "SERVE_CA_KEY",
}

var prefixRE = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,30}$`)

const shortHash = 8

// Fingerprint is "len=N sha256=XXXXXXXX" for v.
func Fingerprint(v string) string {
	sum := sha256.Sum256([]byte(v))
	return "len=" + strconv.Itoa(len(v)) + " sha256=" + hex.EncodeToString(sum[:])[:shortHash]
}

// Names lists the secret variables that are set in env, in a stable order. It adds
// <NAME>_PASSWORD for each cloud of OPENSTACK_CLOUDS.
func Names(env core.Env) []string {
	names := slices.Clone(fixed)
	for _, e := range strings.Split(env.Getenv("OPENSTACK_CLOUDS"), ",") {
		name, _, ok := strings.Cut(strings.TrimSpace(e), "=")
		if name = strings.TrimSpace(name); ok && prefixRE.MatchString(name) {
			names = append(names, name+"_PASSWORD")
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	out := names[:0]
	for _, n := range names {
		if env.Getenv(n) != "" {
			out = append(out, n)
		}
	}
	return out
}

// Log writes one line for each set secret variable: its name and fingerprint, never the value.
func Log(env core.Env, log *slog.Logger) {
	for _, n := range Names(env) {
		log.Info("credential fingerprint", "name", n, "fingerprint", Fingerprint(env.Getenv(n)))
	}
}
