package enroll

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// activityPeriod is how often the background loop looks at the agents' traffic counters; it is also the resolution of
// "last active". The first look is delayed so startup is not slowed down.
var (
	activityPeriod = 5 * time.Minute
	activityFirst  = 45 * time.Second
	// activityMinGap bounds the writes per device: a busy device is recorded at most this often.
	activityMinGap = time.Minute
)

// ActivityTracker turns the agents' cumulative traffic counters into a "last active" time per device, without any
// connection log and without client addresses: it only notices that a device's total grew between two looks. The
// counters reset when xray restarts, which the agent does whenever the credential set changes, so a total that drops
// but is still above zero also counts as activity (traffic since the reset). The first look at a device only seeds the
// baseline: nothing is known about the time before it.
type ActivityTracker struct {
	store Backend

	mu    sync.Mutex
	prev  map[int64]float64
	wrote map[int64]time.Time
}

// NewActivityTracker returns a tracker that records into store.
func NewActivityTracker(store Backend) *ActivityTracker {
	return &ActivityTracker{store: store, prev: map[int64]float64{}, wrote: map[int64]time.Time{}}
}

// Observe compares the current totals with the previous look and records activity for devices whose total grew. It
// returns the devices it touched with the time it recorded, so a caller can show the new value at once. Callers must
// not pass partial statistics (an ingress that did not answer lowers the totals and would read as a reset).
func (t *ActivityTracker) Observe(devices []Device, traffic map[int64]Traffic) map[int64]int64 {
	now := t.store.Clock()()
	t.mu.Lock()
	defer t.mu.Unlock()
	live := map[int64]bool{}
	touched := map[int64]int64{}
	for _, d := range devices {
		live[d.ID] = true
		tr, ok := traffic[d.ID]
		if !ok {
			continue // no counters for it (xray restarted, or never connected): keep the old baseline
		}
		total := tr.Up + tr.Down
		prev, seen := t.prev[d.ID]
		t.prev[d.ID] = total
		grew := seen && (total > prev || (total < prev && total > 0))
		if !grew || now.Sub(t.wrote[d.ID]) < activityMinGap {
			continue
		}
		if err := t.store.TouchActive(d.ID, now.Unix()); err != nil {
			slog.Warn("recording device activity failed", "error_type", typeName(err))
			continue
		}
		t.wrote[d.ID] = now
		touched[d.ID] = now.Unix()
	}
	for id := range t.prev { // forget deleted devices
		if !live[id] {
			delete(t.prev, id)
			delete(t.wrote, id)
		}
	}
	return touched
}

// ActivityLoop looks at the traffic counters every activityPeriod until ctx ends. A failure of one look (store
// unreadable, an ingress not answering) skips that look; it never takes the loop down.
func ActivityLoop(ctx context.Context, a *Admin) {
	timer := time.NewTimer(activityFirst)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		a.observeOnce(ctx)
		timer.Reset(activityPeriod)
	}
}

func (a *Admin) observeOnce(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("activity look failed", "error_type", typeName(r))
		}
	}()
	cur := a.reg.Current(ctx, false)
	devs, err := a.store.ListDevices()
	if err != nil {
		slog.Warn("activity look skipped: store unreadable", "error_type", errType(err))
		return
	}
	stats, partial := MergedStats(ctx, cur.SyncTargets, cur.Cfg, devs)
	if partial {
		return
	}
	a.activity.Observe(devs, stats)
}
