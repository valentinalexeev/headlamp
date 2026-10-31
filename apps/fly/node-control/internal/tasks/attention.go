package tasks

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
)

// taskPath is the page of one task: the id goes through query escaping, so it can never break the path.
func taskPath(id string) string { return "/tasks?sel=" + url.QueryEscape(id) }

// Attention reports tasks that are failing or overdue (waiting) and browser sessions that are running
// (info). Only TASKS_ADMIN_USERS see it. An inactive module reports nothing. While the store is not open
// the tasks are unknown, which is said with an item of its own; sessions do not need the store and still
// report.
func (m *Module) Attention(ctx context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if !m.on {
		return nil, nil, nil
	}
	if !authz.ParseAllowed(m.env.Getenv("TASKS_ADMIN_USERS")).Has(login) {
		return nil, nil, core.ErrNotVisible
	}
	items, lines := m.taskAttention()
	si, sl := m.sessionAttention(ctx)
	return append(items, si...), append(lines, sl...), nil
}

func (m *Module) taskAttention() ([]core.AttentionItem, []core.SystemLine) {
	line := core.SystemLine{Name: "Tasks", Path: "/tasks", State: "ok"}
	st, eng := m.current()
	if st == nil || eng == nil {
		// Unknown, but nothing is known to be at risk: the line says it, no red item does.
		line.State, line.Summary = "unknown", "The task store is not available"
		return nil, []core.SystemLine{line}
	}
	cfg := st.Config()
	if cfg == nil || len(cfg.Tasks) == 0 {
		line.Summary = "No tasks configured"
		return nil, []core.SystemLine{line}
	}
	now := m.now()
	var items []core.AttentionItem
	passing := 0
	for _, id := range sortedTasks(cfg) {
		var active *Run
		if r, ok := eng.Running(id); ok {
			active = &r
		}
		runs := st.Runs(id)
		v := buildTaskView(now, id, cfg.Tasks[id], st.State(id), runs, active)
		if v.Streak == 0 && !v.Stale {
			passing++
			continue
		}
		it := core.AttentionItem{ID: "task-" + id, Source: "tasks", Level: core.LevelWaiting, ActionLabel: "Open task", ActionPath: taskPath(id)}
		for _, r := range runs { // newest first
			if realFailure(r) {
				it.Since = r.Ended
				break
			}
		}
		switch {
		case v.Streak == 1:
			it.Title = fmt.Sprintf("Task %s has failed 1 time in a row", id)
		case v.Streak > 1:
			it.Title = fmt.Sprintf("Task %s has failed %d times in a row", id, v.Streak)
		default:
			it.Title = fmt.Sprintf("Task %s is overdue", id)
		}
		switch {
		case v.Streak > 0 && v.Stale:
			it.Detail = "The check is not passing and has not succeeded within twice its interval."
		case v.Streak > 0:
			it.Detail = "The check is not passing."
		default:
			it.Detail = "It has not succeeded within twice its interval."
		}
		items = append(items, it)
	}
	line.Summary = fmt.Sprintf("%d of %d passing", passing, len(cfg.Tasks))
	if len(items) > 0 {
		line.State = "waiting"
	}
	return items, []core.SystemLine{line}
}

// sessionTTL is how long Attention reuses what it learned from the runners, so polling Overview does not
// multiply calls to them.
const sessionTTL = 20 * time.Second

func (m *Module) sessionAttention(ctx context.Context) ([]core.AttentionItem, []core.SystemLine) {
	if m.sess == nil {
		return nil, nil
	}
	m.sessMu.Lock()
	defer m.sessMu.Unlock()
	if now := m.now(); m.sessLines != nil && now.Sub(m.sessAt) < sessionTTL {
		return m.sessItems, m.sessLines
	}
	items, lines := m.readSessions(ctx)
	m.sessAt, m.sessItems, m.sessLines = m.now(), items, lines
	return items, lines
}

func (m *Module) readSessions(ctx context.Context) ([]core.AttentionItem, []core.SystemLine) {
	line := core.SystemLine{Name: "Sessions", Path: "/sessions", State: "ok"}
	nodes, err := m.sess.tagged(ctx)
	if err != nil {
		line.State, line.Summary = "unknown", "The node list is not available"
		return nil, []core.SystemLine{line}
	}
	found := make([]*SessionInfo, len(nodes))
	var wg sync.WaitGroup
	for i, n := range nodes {
		addr, ok := firstIPv4(n)
		if !ok || !n.Online {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, cancel := context.WithTimeout(ctx, sessionNodeTimeout)
			defer cancel()
			if s, err := m.sess.c.Current(c, addr); err == nil && s != nil && s.State == "running" {
				found[i] = s
			}
		}()
	}
	wg.Wait()
	var items []core.AttentionItem
	for i, s := range found {
		if s == nil {
			continue
		}
		name := nodeID(nodes[i])
		it := core.AttentionItem{ID: "session-" + name, Source: "tasks", Level: core.LevelInfo,
			Title: "Browser session running on " + name, Detail: timeLeft(s.ExpiresAt, m.now()),
			ActionLabel: "Open sessions", ActionPath: "/sessions", Since: s.StartedAt, Deadline: s.ExpiresAt}
		items = append(items, it)
	}
	switch len(items) {
	case 0:
		line.Summary = "None running"
	case 1:
		line.Summary = "1 running, " + items[0].Detail
	default:
		line.Summary = fmt.Sprintf("%d running", len(items))
	}
	return items, []core.SystemLine{line}
}

// timeLeft says how long a session has left, without a clock face.
func timeLeft(end, now time.Time) string {
	if end.IsZero() {
		return "No end time set"
	}
	d := end.Sub(now)
	switch {
	case d <= 0:
		return "Ending now"
	case d < time.Minute:
		return "Less than a minute left"
	case d < time.Hour:
		return fmt.Sprintf("%d min left", int(d/time.Minute))
	}
	return fmt.Sprintf("%d h %d min left", int(d/time.Hour), int(d%time.Hour/time.Minute))
}
