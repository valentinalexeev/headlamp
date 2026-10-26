package runner

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func contextWithTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

func TestScrubberLongestFirstAndShortIgnored(t *testing.T) {
	s := newScrubber(map[string]string{"A": "pass", "B": "password123", "C": "ab"})
	got := s.scrub("x password123 y pass z ab")
	if got != "x *** y *** z ab" {
		t.Errorf("got %q", got)
	}
	if newScrubber(nil).scrub("plain") != "plain" {
		t.Error("empty scrubber must be a no-op")
	}
}

func TestRingKeepsLastBytesAndScrubsAcrossWrites(t *testing.T) {
	s := newScrubber(map[string]string{"K": "supersecret"})
	r := newRing(s.maxLen())
	for i := 0; i < 2000; i++ {
		r.Write([]byte("0123456789"))
	}
	r.Write([]byte("super"))
	r.Write([]byte("secret!"))
	out := r.tail(s)
	if len(out) > logTailBytes {
		t.Errorf("tail is %d bytes, max %d", len(out), logTailBytes)
	}
	if strings.Contains(out, "supersecret") || !strings.HasSuffix(out, "***!") {
		t.Errorf("tail not scrubbed: %q", out[len(out)-30:])
	}
}

func TestChildEnvDropsRunnerVars(t *testing.T) {
	t.Setenv("RUNNER_TOKEN", "leakme")
	t.Setenv("HOME", "/h")
	env := childEnv("S", "P")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "leakme") || strings.Contains(joined, "RUNNER_") {
		t.Error("runner vars must not reach the child")
	}
	if !strings.Contains(joined, "HOME=/h") || !strings.Contains(joined, "TASK_SECRETS_JSON=S") || !strings.Contains(joined, "TASK_PARAMS_JSON=P") {
		t.Errorf("env missing entries: %v", env)
	}
}

func TestRingTrimsSecretFragmentAtWindowStart(t *testing.T) {
	const secret = "supersecret"
	s := newScrubber(map[string]string{"K": secret})
	r := newRing(s.maxLen())
	// Window (last 4107 bytes) starts 6 bytes into the first secret; the
	// scrubbed text shrinks, so a naive tail would begin with "ecret".
	r.Write([]byte(secret + strings.Repeat("-", 4079) + secret + secret))
	if !r.overflowed {
		t.Fatal("ring should have overflowed")
	}
	out := r.tail(s)
	for k := 3; k < len(secret); k++ {
		for _, frag := range []string{secret[:k], secret[len(secret)-k:]} {
			if strings.Contains(out, frag) {
				t.Fatalf("fragment %q leaked; tail starts %q", frag, out[:20])
			}
		}
	}
	if !strings.HasSuffix(out, "******") {
		t.Errorf("full occurrences should be masked: %q", out[len(out)-12:])
	}
}

func TestScrubberCoversJSONEscapedForms(t *testing.T) {
	secret := "pa\"ss\u00e9<w"
	s := newScrubber(map[string]string{"K": secret})
	esc, _ := json.Marshal(secret)
	in := "raw " + secret + " json " + string(esc) + " bare " + strings.Trim(string(esc), `"`)
	if got := s.scrub(in); strings.Contains(got, "ss") || strings.Contains(got, "u003c") {
		t.Errorf("not scrubbed: %q", got)
	}
}
