package nodes

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/headscale"
)

// statusTTL is how long a provider status, read for Attention or for /status, is reused by Attention, so polling
// Overview does not multiply provider calls.
const statusTTL = 20 * time.Second

type cachedStatus struct {
	at time.Time
	st Status
}

func (m *Module) clock() time.Time {
	if m.now != nil {
		return m.now()
	}
	return time.Now()
}

// attentionStatuses returns the provider status of every managed target, from the cache when it is fresh.
func (m *Module) attentionStatuses(ctx context.Context, ts []Target) map[string]Status {
	now := m.clock()
	out := map[string]Status{}
	var stale []Target
	m.stMu.Lock()
	for _, t := range ts {
		if t.Provider == "" {
			continue
		}
		if c, ok := m.stCache[t.ID]; ok && now.Sub(c.at) < statusTTL {
			out[t.ID] = c.st
		} else {
			stale = append(stale, t)
		}
	}
	m.stMu.Unlock()

	fetched := make([]Status, len(stale))
	var wg sync.WaitGroup
	for i, t := range stale {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fetched[i] = m.readStatus(ctx, t)
		}()
	}
	wg.Wait()
	for i, t := range stale {
		out[t.ID] = fetched[i]
	}
	return out
}

// readStatus asks the provider now, sharing a call already running for the node, and leaves the answer for Attention.
// A caller that leaves says nothing about the node: it gets an error status and nothing is cached for it.
func (m *Module) readStatus(ctx context.Context, t Target) Status {
	st, err := m.stRead.Do(ctx, t.ID, func(c context.Context) (Status, error) {
		st := m.statusOf(c, t)
		if t.Provider != "" {
			m.stMu.Lock()
			if m.stCache == nil {
				m.stCache = map[string]cachedStatus{}
			}
			m.stCache[t.ID] = cachedStatus{at: m.clock(), st: st}
			m.stMu.Unlock()
		}
		return st, nil
	})
	if err != nil {
		return Status{"status": "error", "detail": "timed out: " + err.Error()}
	}
	return st
}

func isRunning(st Status) bool {
	s, _ := st["status"].(string)
	switch strings.ToUpper(s) {
	case "RUNNING", "ACTIVE", "STARTED":
		return true
	}
	return false
}

// isAnswer reports whether the provider said what state the node is in.
func isAnswer(st Status) bool {
	s, _ := st["status"].(string)
	switch s {
	case "error", "unknown", "":
		return false
	}
	return true
}

// StoppedNames returns the tailnet names, in lower case, of the managed nodes their provider reports as not running.
// A node whose provider did not answer is not among them.
func (m *Module) StoppedNames(ctx context.Context) map[string]bool {
	ts := m.Targets(ctx)
	statuses := m.attentionStatuses(ctx, ts)
	out := map[string]bool{}
	for _, t := range ts {
		st, ok := statuses[t.ID]
		if !ok || !isAnswer(st) || isRunning(st) {
			continue
		}
		if s, _ := st["status"].(string); s == "not_configured" {
			continue
		}
		name := t.TailnetName
		if name == "" {
			name = t.ID
		}
		out[strings.ToLower(name)] = true
	}
	return out
}

// tailnetNode finds the headscale registration of a target: by its tailnet name, or that name with a
// "-N" re-registration suffix, preferring one that is online.
func tailnetNode(ns []headscale.Node, name string) *headscale.Node {
	var best *headscale.Node
	for i := range ns {
		n := &ns[i]
		base := n.Name
		if g := dupSuffixRE.FindStringSubmatch(n.Name); g != nil {
			base = g[1]
		}
		if !(strings.EqualFold(n.Name, name) || strings.EqualFold(n.GivenName, name) || strings.EqualFold(base, name)) {
			continue
		}
		if best == nil || (n.Online && !best.Online) {
			best = n
		}
	}
	return best
}

// Attention reports a node that runs at its provider but is offline on the tailnet (critical). A stopped
// node is not expected on the tailnet and an unmanaged node has no expectation, so neither raises anything.
// When headscale cannot be read there is one item, not one per node, and the line reads unknown.
func (m *Module) Attention(ctx context.Context, _ string) ([]core.AttentionItem, []core.SystemLine, error) {
	ts := m.Targets(ctx)
	var hs []headscale.Node
	var hsErr error // a node source that was never configured is not a failure
	if m.src != nil {
		hs, hsErr = m.src.Nodes(ctx)
	}
	statuses := m.attentionStatuses(ctx, ts)

	var items []core.AttentionItem
	managed, impaired, stopped, unchecked := 0, 0, 0, 0
	for _, t := range ts {
		if t.Provider == "" {
			continue
		}
		managed++
		st := statuses[t.ID]
		switch {
		case !isAnswer(st):
			unchecked++
		case !isRunning(st):
			if s, _ := st["status"].(string); s != "not_configured" {
				stopped++
			}
		case hsErr == nil:
			if n := tailnetNode(hs, t.TailnetName); n != nil && !n.Online {
				it := core.AttentionItem{
					ID: "node-offline-" + t.ID, Source: "nodes", Level: core.LevelCritical,
					Title:       t.ID + " is running but offline on the tailnet",
					Detail:      "The provider reports it as up, but clients cannot use it.",
					ActionLabel: "Open node", ActionPath: "/nodes?sel=" + url.QueryEscape(t.ID),
				}
				if n.LastSeen != nil {
					it.Since = *n.LastSeen
				}
				items = append(items, it)
				impaired++
			}
		}
	}

	line := core.SystemLine{Name: "Nodes", Path: "/nodes", State: "ok"}
	switch {
	case hsErr != nil:
		line.State, line.Summary = "unknown", "Tailnet status unavailable"
		items = []core.AttentionItem{{
			ID: "tailnet-unavailable", Source: "nodes", Level: core.LevelWaiting, Title: "Tailnet status unavailable",
			Detail:      "Nodes that run at their provider cannot be checked against the tailnet until headscale answers.",
			ActionLabel: "Open Nodes", ActionPath: "/nodes",
		}}
	case impaired > 0:
		line.State, line.Summary = "critical", fmt.Sprintf("%d of %d impaired", impaired, managed)
	case unchecked > 0:
		line.State, line.Summary = "unknown", fmt.Sprintf("%d could not be checked", unchecked)
	case managed == 0:
		line.Summary = "No managed nodes"
	case m.src == nil:
		line.Summary = fmt.Sprintf("%d managed", managed)
	default:
		line.Summary = fmt.Sprintf("All %d reachable", managed)
	}
	if stopped > 0 && hsErr == nil {
		line.Summary += fmt.Sprintf(", %d stopped", stopped)
	}
	return items, []core.SystemLine{line}, nil
}

// auditRead is how many entries of the shared log Activity reads before it keeps the node actions: the log is
// bounded, and the node actions are only some of it.
const auditRead = 1000

func (m *Module) auditLog() core.AuditLog {
	if m.audit == nil {
		return nil
	}
	return m.audit()
}

// HasActivity reports whether node actions are kept anywhere that can be read back. Without the shared audit log
// (no enrolment store) they only go to the process log, so the console has no activity to show.
func (m *Module) HasActivity() bool { return m.auditLog() != nil }

// Activity lists the node actions ("node." prefix) of the shared audit log, newest first, as "Nodes". Without the
// log it is empty. The detail names the node and the outcome, never a provider's error text.
func (m *Module) Activity(_ context.Context, _ string, limit int) ([]core.ActivityEntry, error) {
	al := m.auditLog()
	if al == nil {
		return nil, nil
	}
	log, err := al.AuditLog(auditRead)
	if err != nil {
		return nil, fmt.Errorf("audit log: %w", err)
	}
	var out []core.ActivityEntry
	for _, e := range log {
		if !strings.HasPrefix(e.Action, "node.") {
			continue
		}
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "Nodes", Action: e.Action, Detail: e.Detail})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Search lists the node ids, each opening the Nodes page on that node.
func (m *Module) Search(ctx context.Context, _ string) ([]core.SearchItem, error) {
	ts := m.Targets(ctx)
	out := make([]core.SearchItem, 0, len(ts))
	for _, t := range ts {
		out = append(out, core.SearchItem{Label: t.ID, Kind: "Node", Path: "/nodes?sel=" + url.QueryEscape(t.ID)})
	}
	return out, nil
}
