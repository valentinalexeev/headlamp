package funnel

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"example.com/node-control/internal/core"
)

// Attention reports a stale or erroring ingress report and namespaces whose change was reverted. Only
// FUNNEL_ADMIN_USERS see it. An inactive module reports nothing; an unreadable store is an error.
//
// The text of the ingress's errors is data from another node: it is counted here, never repeated.
func (m *Module) Attention(_ context.Context, login string) ([]core.AttentionItem, []core.SystemLine, error) {
	if m.st == nil || m.s == nil {
		return nil, nil, nil
	}
	if !m.s.Admins.Has(login) {
		return nil, nil, core.ErrNotVisible
	}
	// Settling first makes a namespace whose change was just reverted read "failed" now.
	nss, err := settle(m.st, m.outcome)
	if err != nil {
		return nil, nil, fmt.Errorf("funnel store: %w", err)
	}
	body, at, haveReport, err := m.st.LatestState()
	if err != nil {
		return nil, nil, fmt.Errorf("funnel store: %w", err)
	}

	line := core.SystemLine{Name: "Funnel", Path: "/funnel", State: "unknown", Summary: "No ingress report yet"}
	var items []core.AttentionItem
	if haveReport {
		var rep struct {
			Routes []json.RawMessage `json:"routes"`
			Errors []json.RawMessage `json:"errors"`
		}
		if json.Unmarshal(body, &rep) != nil {
			return nil, nil, fmt.Errorf("funnel: the stored ingress report is unreadable")
		}
		age := m.now().Sub(at)
		line.State = "ok"
		line.Summary = fmt.Sprintf("%d %s, ingress reported %s ago", len(rep.Routes), plural(len(rep.Routes), "route", "routes"), ageText(age))
		it := core.AttentionItem{ID: "funnel-ingress", Source: "funnel", Level: core.LevelWaiting, Since: at,
			ActionLabel: "Open Funnel", ActionPath: "/funnel"}
		switch {
		case age > staleAfter:
			it.Title = "The funnel ingress has not reported for " + ageText(age)
			it.Detail = "Public routes cannot be verified until it reports again."
			items = append(items, it)
		case len(rep.Errors) > 0:
			n := len(rep.Errors)
			it.Title = fmt.Sprintf("The funnel ingress reports %d %s", n, plural(n, "error", "errors"))
			it.Detail = "Public routes may not be working. The report lists them."
			items = append(items, it)
		}
	}
	for _, n := range nss {
		if n.State != StateFailed {
			continue
		}
		items = append(items, core.AttentionItem{
			ID: "funnel-ns-" + n.Name, Source: "funnel", Level: core.LevelWaiting,
			Title:       fmt.Sprintf("Namespace %s failed", n.Name),
			Detail:      "Its change was not confirmed in time and was reverted.",
			ActionLabel: "Open namespace", ActionPath: "/funnel/config",
		})
	}
	if len(items) > 0 {
		line.State = "waiting"
	}
	return items, []core.SystemLine{line}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ageText is a short age: seconds under two minutes, then minutes, hours and days.
func ageText(d time.Duration) string {
	s := int(d.Seconds())
	switch {
	case s < 0:
		return "0 s"
	case s < 120:
		return fmt.Sprintf("%d s", s)
	case s < 2*3600:
		return fmt.Sprintf("%d min", s/60)
	case s < 2*86400:
		return fmt.Sprintf("%d h", s/3600)
	}
	return fmt.Sprintf("%d d", s/86400)
}
