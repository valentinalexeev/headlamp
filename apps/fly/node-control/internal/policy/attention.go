package policy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
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

	var applied time.Time
	for _, e := range hist {
		if e.ID == p.ID {
			applied = e.At
		}
	}
	it := core.AttentionItem{
		ID: fmt.Sprintf("policy-pending-%d", p.ID), Source: "policy", ActionPath: "/policy",
		Since: applied, AppliedAt: applied, Deadline: p.Deadline, ChangeID: p.ID,
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

// Activity lists the change history, newest first: each change when it was applied (by whoever applied it), and each
// change that ended reverted, when its deadline passed (by "system"). The history records neither who confirmed a
// change nor when, so confirmations are not listed. A change reverted on request shows the same entry. The policy
// text and the reason of a revert are not part of it. Only POLICY_ADMIN_USERS see it.
func (m *Module) Activity(_ context.Context, login string, limit int) ([]core.ActivityEntry, error) {
	if m.store == nil {
		return nil, nil
	}
	if !authz.ParseAllowed(m.env.Getenv("POLICY_ADMIN_USERS")).Has(login) {
		return nil, core.ErrNotVisible
	}
	now := time.Now
	if m.svc != nil {
		now = m.svc.clock
	}
	hist, err := m.store.History(limit)
	if err != nil {
		return nil, fmt.Errorf("policy store: %w", err)
	}
	var out []core.ActivityEntry
	for _, e := range hist {
		detail := fmt.Sprintf("%d", e.ID)
		if r := strings.TrimRight(strings.TrimSpace(e.Reason), "."); r != "" {
			detail += ": " + r
		}
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "Policy", Action: "change.applied", Detail: detail})
		if e.State == OutcomeReverted {
			at := e.Deadline
			if at.After(now()) {
				at = now()
			}
			out = append(out, core.ActivityEntry{At: at, Actor: "system", Source: "Policy", Action: "change.reverted", Detail: fmt.Sprintf("%d", e.ID)})
		}
	}
	return out, nil
}
