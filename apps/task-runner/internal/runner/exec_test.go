package runner

import (
	"context"
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
