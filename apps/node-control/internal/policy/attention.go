package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// historyScan is how many history rows Attention reads to find the pending change's apply time.
const historyScan = 20

// Attention reports the change that waits for confirmation (timed) and a revert that is stuck (critical).
// Only POLICY_ADMIN_USERS see it. An inactive console reports nothing. A store that cannot be read is an
// error, which the core shows as "unknown".
func (m *Module) Attention(_ context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.store == nil {
		return nil, nil, nil
	}
	if !authz.ParseAllowed(m.env.Getenv("POLICY_ADMIN_USERS")).Has(login) {
		return nil, nil, core.ErrNotVisible
	}
	now := time.Now
	if m.svc != nil {
		now = m.svc.clock
	}
	p, pending, err := m.store.Pending()
	if err != nil {
		return nil, nil, fmt.Errorf("policy store: %w", err)
	}
	hist, err := m.store.History(historyScan)
	if err != nil {
		return nil, nil, fmt.Errorf("policy store: %w", err)
	}
	line := core.SystemLine{Name: "Policy", Path: "/policy", State: "ok", Summary: "No changes yet"}
	if len(hist) > 0 {
		line.Summary = fmt.Sprintf("Last change #%d, %s", hist[0].ID, hist[0].State)
	}
	if !pending {
		return nil, []core.SystemLine{line}, nil
	}

	since := p.AppliedAt
	for _, e := range hist {
		if e.ID == p.ID && since.IsZero() {
			since = e.At
		}
	}
	it := core.AttentionItem{
		ID: fmt.Sprintf("policy-pending-%d", p.ID), Source: "policy", ActionPath: "/policy",
		Since: since, AppliedAt: p.AppliedAt, Deadline: p.Deadline, ChangeID: p.ID,
	}
	if now().After(p.Deadline) && p.Attempts > 0 {
		it.Level = core.LevelCritical
		it.Title = fmt.Sprintf("Policy change #%d is neither confirmed nor reverted", p.ID)
		it.Detail = fmt.Sprintf("The automatic revert is failing and keeps retrying (failed attempts: %d). Open the policy page to revert it or confirm it.", p.Attempts)
		it.ActionLabel = "Open policy"
		line.State, line.Summary = "critical", fmt.Sprintf("Change #%d is stuck", p.ID)
		return []core.AttentionItem{it}, []core.SystemLine{line}, nil
	}
	it.Level = core.LevelTimed
	it.Title = fmt.Sprintf("Policy change #%d is waiting for confirmation", p.ID)
	it.Detail = "Reverts by itself if not confirmed through the new rules."
	if r := strings.TrimRight(strings.TrimSpace(p.Reason), "."); r != "" {
		it.Detail = r + ". " + it.Detail
	}
	it.ActionLabel = "Review and confirm"
	line.State, line.Summary = "waiting", fmt.Sprintf("Change #%d waits for confirmation", p.ID)
	return []core.AttentionItem{it}, []core.SystemLine{line}, nil
}

// HasActivity reports whether the module keeps a change history, which is when the console is active.
func (m *Module) HasActivity() bool { return m.store != nil }

// Activity lists the change history, newest first: each change when it was applied (by whoever applied it) and, from
// the recorded outcomes, each confirmation and each revert with its actor and time: "system" for the automatic revert
// at the deadline, the operator for a revert on request. A reverted change from before outcomes were recorded has
// no record; it reads as a plain revert (change.reverted_earlier) with no actor, since nothing says who, at its
// deadline, or when the change was applied if that deadline is still ahead.
// An old confirmation is not listed: nothing says when it happened. The policy text is never part of an entry.
// Times are whole seconds, so a change's end comes before its application here: when both fall in one second, the
// newest-first sort, which is stable, keeps them in that order.
// Only POLICY_ADMIN_USERS see it.
func (m *Module) Activity(_ context.Context, login string, limit int) ([]core.ActivityEntry, error) {
	if m.store == nil {
		return nil, nil
	}
	if !authz.ParseAllowed(m.env.Getenv("POLICY_ADMIN_USERS")).Has(login) {
		return nil, core.ErrNotVisible
	}
	hist, err := m.store.History(limit)
	if err != nil {
		return nil, fmt.Errorf("policy store: %w", err)
	}
	recs, err := m.store.Records()
	if err != nil {
		return nil, fmt.Errorf("policy store: %w", err)
	}
	now := time.Now
	if m.svc != nil {
		now = m.svc.clock
	}
	var out []core.ActivityEntry
	for _, e := range hist {
		detail := fmt.Sprintf("%d", e.ID)
		if r := strings.TrimRight(strings.TrimSpace(e.Reason), "."); r != "" {
			detail += ": " + r
		}
		id := fmt.Sprintf("%d", e.ID)
		if r, ok := recs[e.ID]; ok {
			action := map[string]string{RecordConfirmed: "change.confirmed", RecordReverted: "change.reverted",
				RecordRevertedOnRequest: "change.reverted_on_request"}[r.Action]
			if action != "" {
				out = append(out, core.ActivityEntry{At: r.At, Actor: r.Actor, Source: "Policy", Action: action, Detail: id})
			}
		} else if e.State == OutcomeReverted {
			at := e.Deadline
			if at.After(now()) {
				at = e.At
			}
			out = append(out, core.ActivityEntry{At: at, Source: "Policy", Action: "change.reverted_earlier", Detail: id})
		}
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "Policy", Action: "change.applied", Detail: detail})
	}
	return out, nil
}
