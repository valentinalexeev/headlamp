package enroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sync"
)

// Traffic is the up and down byte counts of one device.
type Traffic struct{ Up, Down float64 }

// maxTraffic bounds one reported value and every per-device sum (2^53, the largest integer a
// float64 holds exactly): a hostile agent cannot push a total to +Inf, which encoding/json refuses.
const maxTraffic = 9007199254740992

// numeric reads a JSON number (decoded as float64, or an integer type) as a finite, non-negative
// float no larger than maxTraffic.
func numeric(v any) (float64, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case float32:
		f = float64(n)
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	case uint64:
		f = float64(n)
	default:
		return 0, false
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > maxTraffic {
		return 0, false
	}
	return f, true
}

// MergedStats asks every agent for its traffic and sums it per device across ingresses. Each
// ingress reports under the opaque id of ITS OWN uuid for the device, so the lookup goes through
// UUIDFor per target. Entries that are not objects with numeric up/down are ignored. partial is
// true when a target answered nothing while its sync is pending (the agent client answers an empty
// map on any failure) or when an id could not be computed.
func MergedStats(ctx context.Context, targets []SyncTarget, cfg IngressConfig, devices []Device) (map[int64]Traffic, bool) {
	answers := make([]map[string]any, len(targets))
	panicked := make([]bool, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { // a broken agent client must not take the page down
				if recover() != nil {
					panicked[i] = true
				}
			}()
			answers[i] = t.Agent.Stats(ctx)
		}()
	}
	wg.Wait()
	out := map[int64]Traffic{}
	partial := false
	for i, t := range targets {
		if panicked[i] || (len(answers[i]) == 0 && t.State.Snapshot().Pending) {
			partial = true
		}
		for _, d := range devices {
			auto := d.UUIDs["auto"]
			if auto == "" {
				continue
			}
			u, err := cfg.UUIDFor(t.Ingress, "auto", auto)
			if err != nil {
				partial = true
				continue
			}
			sum := sha256.Sum256([]byte(u))
			entry, ok := answers[i][hex.EncodeToString(sum[:])[:8]].(map[string]any)
			if !ok {
				continue
			}
			up, ok1 := numeric(entry["up"])
			down, ok2 := numeric(entry["down"])
			if !ok1 || !ok2 {
				continue
			}
			tr := out[d.ID]
			tr.Up = math.Min(tr.Up+up, maxTraffic)
			tr.Down = math.Min(tr.Down+down, maxTraffic)
			out[d.ID] = tr
		}
	}
	return out, partial
}
