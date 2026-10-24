package enroll

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

var (
	firstReconcileDelay = 20 * time.Second // after a restart: let the tailnet come up, then sync at once
	reconcilePeriod     = 5 * time.Minute
)

// setReconcileForTest overrides the loop timings and returns a restore function.
func setReconcileForTest(first, period time.Duration) func() {
	of, op := firstReconcileDelay, reconcilePeriod
	firstReconcileDelay, reconcilePeriod = first, period
	return func() { firstReconcileDelay, reconcilePeriod = of, op }
}

// syncFn is the unit the loop runs; tests replace it.
var syncFn = Sync

// syncMu makes "read the store + push" one step, so a stale snapshot never lands last.
var syncMu sync.Mutex

// SyncSnapshot is the sync outcome as shown to the admin page.
type SyncSnapshot struct {
	Pending bool   `json:"pending"`
	State   string `json:"state"`
	Since   *int64 `json:"since"`
}

// SyncState is the in-memory sync outcome. Pending: the last push failed and has not succeeded
// since. State: unknown, ok, empty, unreachable, unauthorized, rejected, conflict, error. Since:
// unix time of the first failure of the current pending streak.
type SyncState struct {
	mu      sync.Mutex
	clock   func() time.Time
	pending bool
	state   string
	since   *int64
}

// NewSyncState returns a state in "unknown". A nil clock means time.Now.
func NewSyncState(clock func() time.Time) *SyncState {
	if clock == nil {
		clock = time.Now
	}
	return &SyncState{clock: clock, state: "unknown"}
}

func (s *SyncState) OK(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending, s.state, s.since = false, state, nil
}

func (s *SyncState) Failed(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		t := s.clock().Unix()
		s.since = &t
	}
	s.pending, s.state = true, state
}

func (s *SyncState) Snapshot() SyncSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := SyncSnapshot{Pending: s.pending, State: s.state}
	if s.since != nil {
		v := *s.since
		snap.Since = &v
	}
	return snap
}

// Sync pushes the live credentials. An empty set is confirmed only when the store has devices
// (all revoked/expired is deliberate); an empty store is never pushed, and an unreadable store is
// a failure, never an empty set.
func Sync(ctx context.Context, store *Store, agent Agent, state *SyncState) bool {
	syncMu.Lock()
	defer syncMu.Unlock()
	creds, err := store.ActiveCredentials()
	if err != nil {
		slog.Warn("enrolment sync failed: store unreadable", "error_type", errType(err))
		state.Failed("error")
		return false
	}
	devices, err := store.ListDevices()
	if err != nil {
		slog.Warn("enrolment sync failed: store unreadable", "error_type", errType(err))
		state.Failed("error")
		return false
	}
	if len(creds) == 0 && len(devices) == 0 {
		state.OK("empty")
		return true
	}
	if err := agent.PutCredentials(ctx, creds, len(creds) == 0); err != nil {
		var au *AgentUnavailable
		if errors.As(err, &au) {
			if au.Status != 0 {
				slog.Warn("enrolment sync failed: "+au.Outcome+" (AgentUnavailable)", "status", au.Status)
			} else {
				slog.Warn("enrolment sync failed: " + au.Outcome + " (AgentUnavailable)")
			}
			state.Failed(au.Outcome)
			return false
		}
		slog.Warn("enrolment sync failed: error", "error_type", errType(err))
		state.Failed("error")
		return false
	}
	state.OK("ok")
	return true
}

func errType(err error) string { return typeName(err) }

// ReconcileLoop retries the sync on a timer until ctx is cancelled. A panic or failure in one run
// is logged by class only and marks the state "error"; the loop continues.
func ReconcileLoop(ctx context.Context, store *Store, agent Agent, state *SyncState) {
	timer := time.NewTimer(firstReconcileDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		runOnce(ctx, store, agent, state)
		timer.Reset(reconcilePeriod)
	}
}

func runOnce(ctx context.Context, store *Store, agent Agent, state *SyncState) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("enrolment reconcile failed", "error_type", typeName(r))
			state.Failed("error")
		}
	}()
	syncFn(ctx, store, agent, state)
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }
