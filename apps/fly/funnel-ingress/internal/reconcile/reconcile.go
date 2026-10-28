// Package reconcile removes dynamic routes whose owner has left the tailnet or lost its tags. Being offline
// does not remove a route; there is no heartbeat expiry.
package reconcile

import (
	"context"
	"sync"
	"time"

	"funnel-ingress/internal/auth"
	"funnel-ingress/internal/routes"
	"funnel-ingress/internal/tsnode"
)

// Reconciler compares the dynamic routes with the peer list of the proxy's node.
type Reconciler struct {
	Node          tsnode.Node
	Table         *routes.Table
	Policy        auth.Policy
	MissingChecks int // consecutive ticks an owner may be absent from the peer list before its routes go
	Interval      time.Duration
	Log           func(format string, args ...any)

	mu     sync.Mutex
	misses map[string]int
	last   time.Time
}

func (r *Reconciler) logf(format string, args ...any) {
	if r.Log != nil {
		r.Log(format, args...)
	}
}

// LastTick is the time of the last tick that evaluated a complete peer list; zero before the first one.
func (r *Reconciler) LastTick() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.last
}

// Tick runs one reconciliation and returns the keys removed. Routes are bound to the owner's stable node ID,
// so a node that is deleted and enrolled again under the same name inherits nothing.
func (r *Reconciler) Tick(ctx context.Context) []routes.Key {
	snap, err := r.Node.Snapshot(ctx)
	if err != nil {
		r.logf("reconcile: reading the peer list failed: %v", err)
		return nil
	}
	if !snap.Loaded {
		// After a start there is no complete list yet; cleaning up now would orphan everything.
		return nil
	}
	peers := make(map[string]tsnode.Peer, len(snap.Peers))
	for _, p := range snap.Peers {
		peers[p.NodeID] = p
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.misses == nil {
		r.misses = map[string]int{}
	}
	owners := r.Table.NodeIDs()
	live := make(map[string]bool, len(owners))
	var removed []routes.Key
	for _, id := range owners {
		live[id] = true
		peer, present := peers[id]
		if !present {
			r.misses[id]++
			if r.misses[id] < r.MissingChecks {
				continue
			}
			keys, err := r.Table.RemoveNode(id)
			if err != nil {
				r.logf("reconcile: removing the routes of node %s failed, will retry: %v", id, err)
				continue
			}
			for _, k := range keys {
				r.logf("reconcile: removed route %s/%s: node %s is no longer on the tailnet", k.NS, k.Service, id)
			}
			removed = append(removed, keys...)
			delete(r.misses, id)
			continue
		}
		delete(r.misses, id)
		removed = append(removed, r.dropUnauthorised(id, peer)...)
	}
	for id := range r.misses {
		if !live[id] {
			delete(r.misses, id) // its routes went by other means
		}
	}
	r.last = time.Now()
	return removed
}

// dropUnauthorised removes routes of a present node that no longer carries the funnel tag or the tag of the
// route's namespace.
func (r *Reconciler) dropUnauthorised(id string, peer tsnode.Peer) []routes.Key {
	have := map[string]bool{}
	for _, ns := range r.Policy.NamespacesOf(peer.Tags) {
		have[ns] = true
	}
	mine := r.Table.ByNode(id)
	var bad []routes.Route
	for _, rt := range mine {
		if !have[rt.NS] {
			bad = append(bad, rt)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	var removed []routes.Key
	if len(bad) == len(mine) {
		keys, err := r.Table.RemoveNode(id)
		if err != nil {
			r.logf("reconcile: removing the routes of node %s failed, will retry: %v", id, err)
			return nil
		}
		removed = keys
	} else {
		for _, rt := range bad {
			if err := r.Table.Delete(rt.Key, id); err != nil {
				r.logf("reconcile: removing route %s/%s failed, will retry: %v", rt.NS, rt.Service, err)
				continue
			}
			removed = append(removed, rt.Key)
		}
	}
	for _, k := range removed {
		r.logf("reconcile: removed route %s/%s: node %s no longer carries the required tags", k.NS, k.Service, id)
	}
	return removed
}

// Run ticks immediately and then every Interval until ctx is done.
func (r *Reconciler) Run(ctx context.Context) {
	r.Tick(ctx)
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Tick(ctx)
		}
	}
}
