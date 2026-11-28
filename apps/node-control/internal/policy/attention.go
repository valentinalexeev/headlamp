package policy

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policymap"
)

// historyScan is how many history rows Attention reads to find the pending change's apply time.
const historyScan = 20

// Attention reports the change that waits for confirmation (timed), a revert that is stuck (critical), and the tags,
// groups and users of the live policy that match no headscale node (info, #432).
// Only POLICY_ADMIN_USERS see it. An inactive console reports nothing. A store that cannot be read is an
// error, which the core shows as "unknown".
func (m *Module) Attention(ctx context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.store == nil {
		return nil, nil, nil
	}
	if !m.can(authz.LevelView)(login) {
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
	unmatched, err := m.unmatchedItems(ctx)
	if err != nil {
		// The pending change still shows. With nothing pending, the line says that the check could not look, so an
		// unmatched item that is missing now has not cleared.
		slog.Warn("policy attention cannot check the selectors", "cause", errCause(err))
		if !pending {
			line.State, line.Summary = "unknown", "Cannot check which nodes the policy selectors match"
		}
	}
	if !pending {
		return unmatched, []core.SystemLine{line}, nil
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
		return append([]core.AttentionItem{it}, unmatched...), []core.SystemLine{line}, nil
	}
	it.Level = core.LevelTimed
	it.Title = fmt.Sprintf("Policy change #%d is waiting for confirmation", p.ID)
	it.Detail = "Reverts by itself if not confirmed through the new rules."
	if r := strings.TrimRight(strings.TrimSpace(p.Reason), "."); r != "" {
		it.Detail = r + ". " + it.Detail
	}
	it.ActionLabel = "Review and confirm"
	line.State, line.Summary = "waiting", fmt.Sprintf("Change #%d waits for confirmation", p.ID)
	return append([]core.AttentionItem{it}, unmatched...), []core.SystemLine{line}, nil
}

// unmatchedMax caps the unmatched-selector items of one answer; the rest are on the policy map.
const unmatchedMax = 20

// NodeLister is the optional part of the headscale client that lets Attention find the policy selectors that match
// no node.
type NodeLister interface {
	ListNodes(ctx context.Context) ([]headscale.Node, error)
}

// unmatchedItems returns one info item for each tag, group or user of the live policy that matches no headscale node
// (#432, ADR 0005). It returns nothing when the client cannot list nodes and users. The items never carry policy
// text other than the selector names.
func (m *Module) unmatchedItems(ctx context.Context) ([]core.AttentionItem, error) {
	if m.svc == nil {
		return nil, nil
	}
	nodes, okNodes := m.svc.api.(NodeLister)
	users, okUsers := m.svc.api.(UserLister)
	if !okNodes || !okUsers {
		return nil, nil
	}
	text, _, err := m.svc.Current(ctx)
	if err != nil {
		return nil, fmt.Errorf("read the policy: %w", err)
	}
	if len(text) > m.svc.maxBytes {
		return nil, ErrTooLarge
	}
	g, err := policymap.Build(text)
	if err != nil {
		return nil, err
	}
	hn, err := nodes.ListNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	hu, err := users.ListUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	var out []core.AttentionItem
	for _, n := range policymap.Unmatched(g, devices(hn, hu)) {
		if len(out) == unmatchedMax {
			break
		}
		it := core.AttentionItem{
			ID: "policy-unmatched-" + n.ID, Source: "policy", Level: core.LevelInfo,
			ActionLabel: "Open policy map", ActionPath: "/policy/map?select=" + url.QueryEscape(n.ID),
		}
		switch n.Kind {
		case policymap.KindTag:
			it.Title = fmt.Sprintf("No node has the policy tag %s", n.ID)
			it.Detail = "The rules that name this tag apply to no node."
		case policymap.KindGroup:
			it.Title = fmt.Sprintf("The policy group %s matches no node", n.ID)
			it.Detail = "No member of this group has a node without a tag. The rules that name this group apply to no node."
		default:
			it.Title = fmt.Sprintf("The policy user %s matches no node", n.ID)
			it.Detail = "This user has no node without a tag. The rules that name this user apply to no node."
		}
		out = append(out, it)
	}
	return out, nil
}

// devices converts the headscale nodes for policymap.Unmatched. A tagged node belongs to its tags only.
func devices(nodes []headscale.Node, users []headscale.User) []policymap.Device {
	byID := make(map[uint64]headscale.User, len(users))
	for _, u := range users {
		byID[u.ID] = u
	}
	out := make([]policymap.Device, 0, len(nodes))
	for _, n := range nodes {
		d := policymap.Device{Tags: n.Tags}
		if u, ok := byID[n.UserID]; ok && len(n.Tags) == 0 && n.UserID != 0 {
			for _, form := range []string{u.Name + "@", u.Name, u.Email} {
				if form != "" && form != "@" {
					d.Owner = append(d.Owner, form)
				}
			}
		}
		out = append(out, d)
	}
	return out
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
	if !m.can(authz.LevelView)(login) {
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
