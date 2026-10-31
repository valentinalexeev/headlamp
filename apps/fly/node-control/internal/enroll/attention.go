package enroll

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"example.com/node-control/internal/core"
)

// bridgeSource is the attention source of bridge items: bridges are nodes, so the sidebar counts them under
// Nodes. The enrolment module still emits them, since it owns the sync and approval state.
const bridgeSource = "nodes"

const (
	// expiringWithin is how far ahead a device's expiry raises an item.
	expiringWithin = 7 * 24 * time.Hour
	seenWindow     = 24 * time.Hour
)

// syncReason says why the last push to a bridge failed, from its sync state.
var syncReason = map[string]string{
	"unreachable":  "The agent is unreachable.",
	"unauthorized": "The agent refused the credentials.",
	"rejected":     "The agent rejected the credentials.",
	"conflict":     "The agent reports a conflict.",
	"error":        "The sync ended in an error.",
}

// Attention reports bridges that are out of sync (critical), bridges whose approval changed (critical) or
// waits (waiting), and devices that expire within seven days (waiting, one item with the count). Each bridge
// is named in its own item. An inactive module reports nothing; an unreadable device list is an error.
func (m *Module) Attention(ctx context.Context, _ string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.store == nil || m.reg == nil {
		return nil, nil, nil
	}
	now := time.Now()
	if m.now != nil {
		now = m.now()
	}
	devs, err := m.store.ListDevices()
	if err != nil {
		return nil, nil, fmt.Errorf("enrolment store: %w", err)
	}
	cur := m.reg.Current(ctx, true)

	var items []core.AttentionItem
	critical, outOfSync := false, 0
	for _, row := range rowsOf(cur, cur.SyncTargets, snapshotsOf(cur.SyncTargets)) {
		if !row.Sync.Pending {
			continue
		}
		reason, ok := syncReason[row.Sync.State]
		if !ok {
			reason = "The last sync did not succeed."
		}
		it := core.AttentionItem{
			ID: "bridge-sync-" + row.Name, Source: bridgeSource, Level: core.LevelCritical,
			Title:       row.Name + " is out of sync",
			Detail:      "Revocations and extensions are not in effect there. " + reason,
			ActionLabel: "Open bridge", ActionPath: "/nodes?sel=" + url.QueryEscape(row.Name),
		}
		if row.Sync.Since != nil {
			it.Since = time.Unix(*row.Sync.Since, 0)
		}
		items = append(items, it)
		critical = true
		outOfSync++
	}
	waiting := false
	for _, v := range cur.Views {
		approve := "/enroll/approve?name=" + url.QueryEscape(v.Name)
		switch v.Approval {
		case stChanged:
			items = append(items, core.AttentionItem{
				ID: "bridge-approval-" + v.Name, Source: bridgeSource, Level: core.LevelCritical,
				Title:       v.Name + " changed its host identity",
				Detail:      "Its identity differs from what you approved, so it gets no credentials. Compare them before you decide.",
				ActionLabel: "Compare and decide", ActionPath: approve,
			})
			critical = true
		case stPending:
			items = append(items, core.AttentionItem{
				ID: "bridge-approval-" + v.Name, Source: bridgeSource, Level: core.LevelWaiting,
				Title:       v.Name + " is waiting for approval",
				Detail:      "Discovered on the tailnet. It gets no credentials until you compare and approve it.",
				ActionLabel: "Compare and approve", ActionPath: approve,
			})
			waiting = true
		}
	}

	active, seen, expiring := 0, 0, 0
	var next int64
	for _, d := range devs {
		if d.Revoked || d.ExpiresAt <= now.Unix() {
			continue
		}
		active++
		if d.LastActive >= now.Add(-seenWindow).Unix() && d.LastActive > 0 {
			seen++
		}
		if time.Unix(d.ExpiresAt, 0).Sub(now) <= expiringWithin {
			expiring++
			if next == 0 || d.ExpiresAt < next {
				next = d.ExpiresAt
			}
		}
	}
	if expiring > 0 {
		noun, verb := "devices", "expire"
		if expiring == 1 {
			noun, verb = "device", "expires"
		}
		items = append(items, core.AttentionItem{
			ID: "devices-expiring", Source: "enroll", Level: core.LevelWaiting,
			Title:       fmt.Sprintf("%d %s %s within 7 days", expiring, noun, verb),
			Detail:      "They stop working on the expiry date unless extended.",
			ActionLabel: "Review", ActionPath: "/enroll?filter=expiring", Deadline: time.Unix(next, 0),
		})
		waiting = true
	}

	line := core.SystemLine{Name: "Devices", Path: "/enroll", State: "ok", Summary: fmt.Sprintf("%d active, %d seen today", active, seen)}
	switch {
	case critical:
		line.State = "critical"
	case waiting:
		line.State = "waiting"
	}
	if outOfSync > 0 {
		line.Summary += fmt.Sprintf(". %d bridge sync pending", outOfSync)
	}
	return items, []core.SystemLine{line}, nil
}
