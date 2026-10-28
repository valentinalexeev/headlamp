//go:build integration

package tsnode

import (
	"context"
	"os"
	"testing"
	"time"

	"funnel-ingress/internal/config"
)

// Run against a real headscale with a tagged pre-auth key:
//
//	FUNNEL_TEST_CONTROL_URL=https://headscale.example.com FUNNEL_TEST_AUTHKEY=... \
//	  go test -tags integration -run TestTSNetJoinsAndReportsSelf ./internal/tsnode/
//
// It skips when the variables are unset.
func TestTSNetJoinsAndReportsSelf(t *testing.T) {
	control, key := os.Getenv("FUNNEL_TEST_CONTROL_URL"), os.Getenv("FUNNEL_TEST_AUTHKEY")
	if control == "" || key == "" {
		t.Skip("FUNNEL_TEST_CONTROL_URL / FUNNEL_TEST_AUTHKEY not set")
	}
	n := NewTSNet(config.Config{Hostname: "funnel-ingress-itest", AuthKey: key, ControlURL: control, StateDir: t.TempDir()}, nil)
	defer n.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := n.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if !n.Self().Addr.IsValid() || n.Self().NodeID == "" {
		t.Fatalf("self = %+v", n.Self())
	}
	snap, err := n.Snapshot(ctx)
	if err != nil || !snap.Loaded {
		t.Fatalf("snapshot: %+v %v", snap, err)
	}
}
