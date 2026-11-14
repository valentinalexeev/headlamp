package fleet

import (
	"context"
	"fmt"
	"sort"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/compat"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// compatSource is the Overview source "fleet" (#300): one item for each part whose interface
// version is
// outside the compatibility matrix of this node-control. Critical when COMPAT_STRICT refuses the
// part, else
// waiting. A part that reports no version is not an item; the Fleet page lists it as a note.
const compatSource = "fleet"

// Attention lists the incompatible parts. It reads only what node-control already has: the last
// agent info of
// each bridge, the last funnel report and the last runner health answer. It asks no part.
func (m *module) Attention(ctx context.Context, login string) (
	[]core.AttentionItem, []core.SystemLine, error,
) {
	// The same rule as the Fleet page: nil (no admin policy) lets everyone who reaches the console
	// see it.
	allow := authz.NavAllow(authz.P(authz.FeatureOverview, authz.LevelView))
	if allow != nil && !allow(login) {
		return nil, nil, core.ErrNotVisible
	}
	matrix := compat.Must()
	level, verb := core.LevelWaiting, "is not compatible"
	if m.d.Strict {
		level, verb = core.LevelCritical, "is refused (COMPAT_STRICT)"
	}
	var items []core.AttentionItem
	add := func(id, part string, c compat.Result, action, path string) {
		// Strict mode refuses an unknown part too (#322), so it is an item; without strict mode an
		// unknown part is only a note on the Fleet page.
		refused := c.State == compat.Incompatible || (m.d.Strict && c.State == compat.Unknown)
		if !refused {
			return
		}
		detail := fmt.Sprintf("Its %s version %d is %s. node-control accepts %d to %d. %s",
			c.Interface, c.Reported, c.Hint, c.Accepted.Min, c.Accepted.Max, action)
		if c.State == compat.Unknown {
			detail = fmt.Sprintf("It reports no %s version: it is older than 0.18.0. "+
				"COMPAT_STRICT refuses it. %s", c.Interface, action)
		}
		items = append(items, core.AttentionItem{
			ID: "compat-" + id, Source: compatSource, Level: level,
			AckKey:      fmt.Sprintf("compat-%s|%s|%d", id, c.Interface, c.Reported),
			Title:       part + " " + verb,
			Detail:      detail,
			ActionLabel: "Open Fleet", ActionPath: path,
		})
	}
	if m.d.Bridges != nil {
		for _, v := range m.d.Bridges(ctx) {
			if !v.Active || v.Source != "discovered" || v.AgentVersion == "" && v.AgentAPI == 0 {
				continue
			}
			add("bridge-"+v.Name, "Bridge "+v.Name, matrix.Check(compat.AgentAPI, v.AgentAPI),
				"Run the ingress-agent role on "+v.Name+", or upgrade node-control.", "/fleet")
		}
	}
	if m.d.Funnel != nil {
		if s, ok := m.d.Funnel(); ok {
			add("funnel", "The funnel ingress", matrix.Check(compat.FunnelAPI, s.API),
				"Deploy the funnel-ingress image of this release, or upgrade node-control.",
				"/fleet")
		}
	}
	if m.d.Runners != nil {
		addrs := []string{}
		runners := m.d.Runners()
		for a := range runners {
			addrs = append(addrs, a)
		}
		sort.Strings(addrs)
		for _, a := range addrs {
			add("runner-"+a, "Task runner "+a, matrix.Check(compat.RunnerAPI, runners[a].API),
				"Run the task-runner role on it, or upgrade node-control.", "/fleet")
		}
	}
	line := core.SystemLine{Name: "Fleet", Path: "/fleet", State: "ok",
		Summary: "All parts compatible"}
	if len(items) > 0 {
		line.State = "waiting"
		if m.d.Strict {
			line.State = "critical"
		}
		n := len(items)
		line.Summary = fmt.Sprintf("%d incompatible %s", n, plural(n, "part", "parts"))
		if m.d.Strict {
			line.Summary = fmt.Sprintf("%d %s refused (COMPAT_STRICT)",
				n, plural(n, "part", "parts"))
		}
	}
	return items, []core.SystemLine{line}, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
