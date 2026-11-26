package enroll

import (
	"context"
	"log/slog"
	"net/http"
	"sort"
	"strings"
)

// The device drawer (#437): GET /enroll/devices/{id} gives what an admin needs to check the
// connectivity of one device. No connection log and no client address: only times, byte counts,
// bridge names and the person's tailnet devices as headscale reports them.
//
//	{"device": {...deviceRow},
//	 "profile": {"first", "last", "format"},
//	 "bridges": [{"name", "online", "sync", "traffic": {"up", "down"} | null,
//	              "answered", "credential"}],
//	 "tailnet": [{"name", "online", "last_seen"}] | null,
//	 "timeline": [{"event", "at"}]}
//
// credential is "current" (the bridge accepted a set read after the last change of the device),
// "pending" (it has not, or its sync is pending) or "unknown" (no push accepted since node-control
// started). tailnet is null when the owner is not a self-enrolled person or headscale did not
// answer.

type drawerBridge struct {
	Name       string       `json:"name"`
	Online     *bool        `json:"online"`
	Sync       SyncSnapshot `json:"sync"`
	Traffic    any          `json:"traffic"`
	Answered   bool         `json:"answered"`
	Credential string       `json:"credential"`
}

type drawerNode struct {
	Name     string `json:"name"`
	Online   bool   `json:"online"`
	LastSeen int64  `json:"last_seen"` // unix seconds, 0 = not known
}

type timelineEvent struct {
	Event string `json:"event"`
	At    int64  `json:"at"`
}

// credentialState says whether a bridge has the current credential of the device.
func credentialState(d Device, snap SyncSnapshot, syncedAt int64) string {
	switch {
	case snap.Pending:
		return "pending"
	case syncedAt == 0:
		return "unknown"
	case d.ChangedAt() < syncedAt: // strict: syncedAt is taken before the read, in whole seconds
		return "current"
	}
	return "pending"
}

// timelineOf lists the recorded events of the device, oldest first. A time of 0 is not recorded.
func timelineOf(d Device) []timelineEvent {
	out := []timelineEvent{}
	for _, e := range []timelineEvent{
		{"created", d.CreatedAt}, {"invited", d.InvitedAt}, {"link_used", d.EnrolledAt},
		{"profile_downloaded", d.ProfileAt}, {"last_traffic", d.LastActive},
		{"extended", d.ExtendedAt}, {"revoked", d.RevokedAt},
	} {
		if e.At != 0 {
			out = append(out, e)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At < out[j].At })
	return out
}

// tailnetNodesOf gives the headscale nodes of a self-enrolled owner, or nil.
func (a *Admin) tailnetNodesOf(ctx context.Context, owner string) []drawerNode {
	sub, ok := strings.CutPrefix(owner, OwnerPrefix)
	if !ok || a.self == nil || a.self.hs == nil {
		return nil
	}
	u, err := a.self.users.User(sub)
	if err != nil || u.HSUserID == 0 {
		return nil
	}
	nodes, err := a.self.hs.ListNodes(ctx)
	if err != nil {
		slog.Warn("tailnet devices not read for the device drawer", "error_type", errType(err))
		return nil
	}
	out := []drawerNode{}
	for _, n := range nodes {
		if n.UserID != u.HSUserID {
			continue
		}
		name := n.GivenName
		if name == "" {
			name = n.Name
		}
		var seen int64
		if n.LastSeen != nil {
			seen = n.LastSeen.Unix()
		}
		out = append(out, drawerNode{Name: name, Online: n.Online, LastSeen: seen})
	}
	return out
}

func (a *Admin) deviceDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	d, ok := a.known(w, id)
	if !ok {
		return
	}
	cur := a.reg.Current(r.Context(), true)
	per, answered, _ := BridgeStats(r.Context(), cur.SyncTargets, cur.Cfg, []Device{d})
	snaps := snapshotsOf(cur.SyncTargets)
	online := map[string]*bool{}
	for _, v := range cur.Views {
		online[v.Name] = v.Online
	}
	bridges := make([]drawerBridge, 0, len(cur.SyncTargets))
	for i, t := range cur.SyncTargets {
		var traffic any
		if tr, ok := per[i][d.ID]; ok {
			traffic = map[string]any{"up": tr.Up, "down": tr.Down}
		}
		bridges = append(bridges, drawerBridge{Name: t.Name, Online: online[t.Name], Sync: snaps[i],
			Traffic: traffic, Answered: answered[i],
			Credential: credentialState(d, snaps[i], t.State.SyncedAt())})
	}
	row := deviceRow{d.ID, d.Name, d.Owner, a.ownerEmails()[d.Owner], d.Client, d.ExpiresAt,
		d.Revoked, nil, d.EnrolledAt, d.Enrolments, d.LastActive, d.ProfileAt, d.ProfileLast}
	profile := map[string]any{
		"first": d.ProfileAt, "last": d.ProfileLast, "format": d.ProfileFormat}
	writeJSON(w, http.StatusOK, map[string]any{
		"device":   row,
		"profile":  profile,
		"bridges":  bridges,
		"tailnet":  a.tailnetNodesOf(r.Context(), d.Owner),
		"timeline": timelineOf(d),
	})
}
