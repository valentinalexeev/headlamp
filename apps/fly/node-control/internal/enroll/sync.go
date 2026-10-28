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
	ver     uint64 // bumped on every outcome: lets a caller tell "no result yet" from "same result"
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
	s.ver++
}

func (s *SyncState) Failed(state string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.pending {
		t := s.clock().Unix()
		s.since = &t
	}
	s.pending, s.state = true, state
	s.ver++
}

// version counts the outcomes recorded so far.
func (s *SyncState) version() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ver
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

// SyncTarget is one ingress to keep in sync: its agent, its own sync state and its settings.
type SyncTarget struct {
	Name    string
	Agent   Agent
	State   *SyncState
	Ingress Ingress
}

// CredentialsFor maps the store's base credentials to the credentials of one ingress: every UUID
// goes through UUIDFor (the identity for a "base" ingress); mode and not_after are unchanged. The
// first error is returned and no partial set.
func (c IngressConfig) CredentialsFor(ing Ingress, base []Credential) ([]Credential, error) {
	out := make([]Credential, 0, len(base))
	for _, cr := range base {
		u, err := c.UUIDFor(ing, cr.Mode, cr.UUID)
		if err != nil {
			return nil, err
		}
		out = append(out, Credential{UUID: u, Mode: cr.Mode, NotAfter: cr.NotAfter})
	}
	return out, nil
}

// Sync pushes the live credentials to one agent. An empty set is confirmed only when the store has
// devices (all revoked/expired is deliberate); an empty store is never pushed, and an unreadable
// store is a failure, never an empty set. It is the one-target form of SyncAll: a "base" ingress
// without a name, which uses the stored credentials as they are.
func Sync(ctx context.Context, store Backend, agent Agent, state *SyncState) bool {
	return SyncAll(ctx, store, IngressConfig{}, []SyncTarget{{
		Agent: agent, State: state, Ingress: Ingress{UUIDs: uuidsBase}}})
}

// syncLog logs a sync failure; the ingress name is added when there is one.
func syncLog(name, msg string, attrs ...any) {
	if name != "" {
		attrs = append(attrs, "ingress", name)
	}
	slog.Warn(msg, attrs...)
}

// SyncAll reads the store once and pushes each target its own credential set concurrently, each
// push with the agent's own timeout. It returns true only when every target ended "ok" or "empty".
//
// The global lock is held for the read AND all pushes, so rounds are serialised and a stale
// snapshot never lands after a newer one; the price is that one slow or dead ingress delays the
// next round by at most its push timeout (10 s). An unreadable store fails every target and pushes
// nothing; an empty store is never pushed to any target.
func SyncAll(ctx context.Context, store Backend, cfg IngressConfig, targets []SyncTarget) bool {
	syncMu.Lock()
	defer syncMu.Unlock()
	base, err := store.ActiveCredentials()
	var devices []Device
	if err == nil {
		devices, err = store.ListDevices()
	}
	if err != nil {
		for _, t := range targets {
			syncLog(t.Name, "enrolment sync failed: store unreadable", "error_type", errType(err))
			t.State.Failed("error")
		}
		return false
	}
	var wg sync.WaitGroup
	defer wg.Wait() // runs before the deferred unlock: no push outlives the lock, even on a panic
	results := make([]bool, len(targets))
	for i, t := range targets {
		creds, err := safeCredentials(cfg, t, base)
		if err != nil {
			syncLog(t.Name, "enrolment sync failed: credentials unavailable", "error_type", errType(err))
			t.State.Failed("error")
			continue
		}
		if len(creds) == 0 && len(devices) == 0 {
			t.State.OK("empty")
			results[i] = true
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					syncLog(t.Name, "enrolment sync failed: error", "error_type", typeName(r))
					t.State.Failed("error")
				}
			}()
			results[i] = pushTo(ctx, t, creds)
		}()
	}
	wg.Wait()
	for _, ok := range results {
		if !ok {
			return false
		}
	}
	return true
}

// credentialsHook runs before a target's credentials are computed; tests use it to inject a panic.
var credentialsHook func(Ingress)

// safeCredentials computes a target's set; a panic becomes an error that carries only its type.
func safeCredentials(cfg IngressConfig, t SyncTarget, base []Credential) (creds []Credential, err error) {
	defer func() {
		if r := recover(); r != nil {
			creds, err = nil, fmt.Errorf("panic: %s", typeName(r))
		}
	}()
	if credentialsHook != nil {
		credentialsHook(t.Ingress)
	}
	return cfg.CredentialsFor(t.Ingress, base)
}

// pushTo pushes one target's set and records the outcome in its state.
func pushTo(ctx context.Context, t SyncTarget, creds []Credential) bool {
	if err := t.Agent.PutCredentials(ctx, creds, len(creds) == 0); err != nil {
		var au *AgentUnavailable
		if errors.As(err, &au) {
			if au.Status != 0 {
				syncLog(t.Name, "enrolment sync failed: "+au.Outcome+" (AgentUnavailable)", "status", au.Status)
			} else {
				syncLog(t.Name, "enrolment sync failed: "+au.Outcome+" (AgentUnavailable)")
			}
			t.State.Failed(au.Outcome)
			return false
		}
		syncLog(t.Name, "enrolment sync failed: error", "error_type", errType(err))
		t.State.Failed("error")
		return false
	}
	t.State.OK("ok")
	return true
}

// stateRank orders sync states from worst to best; unknown names rank as "error".
var stateRank = map[string]int{"error": 0, "unauthorized": 1, "rejected": 2, "conflict": 3,
	"unreachable": 4, "unknown": 5, "ok": 6, "empty": 7}

// Summary folds the targets' states: pending if any is pending, the worst state, and the earliest
// "since" among the pending ones.
func Summary(targets []SyncTarget) SyncSnapshot {
	snaps := make([]SyncSnapshot, len(targets))
	for i, t := range targets {
		snaps[i] = t.State.Snapshot()
	}
	return summarize(snaps)
}

// summarize is Summary over snapshots already taken.
func summarize(snaps []SyncSnapshot) SyncSnapshot {
	out := SyncSnapshot{State: "empty"}
	worst := len(stateRank)
	for _, s := range snaps {
		r, known := stateRank[s.State]
		if !known {
			r = 0
		}
		if r < worst {
			worst, out.State = r, s.State
		}
		if s.Pending {
			out.Pending = true
			if s.Since != nil && (out.Since == nil || *s.Since < *out.Since) {
				v := *s.Since
				out.Since = &v
			}
		}
	}
	return out
}

func errType(err error) string { return typeName(err) }

// ReconcileLoop retries the sync on a timer until ctx is cancelled. A panic or failure in one run
// is logged by class only and marks the state "error"; the loop continues.
func ReconcileLoop(ctx context.Context, store Backend, agent Agent, state *SyncState) {
	reconcile(ctx, func(ctx context.Context) { runOnce(ctx, store, agent, state) })
}

// ReconcileLoopAll is ReconcileLoop for every CURRENT target (the set can change at runtime).
func ReconcileLoopAll(ctx context.Context, store Backend, cfg IngressConfig, targets func() []SyncTarget) {
	reconcile(ctx, func(ctx context.Context) {
		var ts []SyncTarget
		defer func() {
			if r := recover(); r != nil {
				slog.Error("enrolment reconcile failed", "error_type", typeName(r))
				for _, t := range ts {
					t.State.Failed("error")
				}
			}
		}()
		ts = targets()
		SyncAll(ctx, store, cfg, ts)
	})
}

func reconcile(ctx context.Context, run func(context.Context)) {
	timer := time.NewTimer(firstReconcileDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		run(ctx)
		timer.Reset(reconcilePeriod)
	}
}

func runOnce(ctx context.Context, store Backend, agent Agent, state *SyncState) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("enrolment reconcile failed", "error_type", typeName(r))
			state.Failed("error")
		}
	}()
	syncFn(ctx, store, agent, state)
}

func typeName(v any) string { return fmt.Sprintf("%T", v) }
