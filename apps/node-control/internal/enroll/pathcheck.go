package enroll

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// PathCheck is the edge-to-central path check of a bridge agent: the class of its last run (ok,
// or which part of the path from the bridge's own network to the control host fails), every
// finding, and how many runs in a row gave that class. Known is false when the agent did not
// answer, is older, or runs no check (it is off by default).
type PathCheck struct {
	Known    bool           `json:"-"`
	Class    string         `json:"class"`
	Findings []string       `json:"findings,omitempty"`
	Since    int64          `json:"since,omitempty"`
	Streak   int            `json:"streak"`
	Checked  int64          `json:"checked"`
	Counts   map[string]int `json:"counts,omitempty"` // classes over the agent's history (24 h)
}

const (
	maxPathcheckBody = 256 << 10 // the answer has a short history: a few kB
	maxPathFindings  = 16
	// pathStreakMin is how many runs in a row must give a bad class before an item is raised:
	// one bad run can be a passing glitch.
	pathStreakMin = 2
	pathClassOK   = "ok"
)

var pathClassRe = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// pathcheckReporter is an Agent that reports its path check.
type pathcheckReporter interface {
	PathCheck(ctx context.Context) PathCheck
}

// PathCheck asks GET /v1/pathcheck. Any failure, an older agent (404) or a disabled check gives
// Known false. Class names are checked, so a bridge host cannot put text into the console.
func (a *agentClient) PathCheck(ctx context.Context) PathCheck {
	ctx, cancel := context.WithTimeout(ctx, statsTimeout)
	defer cancel()
	base, err := a.baseURL(ctx)
	if err != nil {
		return PathCheck{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/pathcheck", nil)
	if err != nil {
		return PathCheck{}
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := a.hc.Do(req)
	if err != nil {
		return PathCheck{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return PathCheck{}
	}
	var r struct {
		PathCheck
		Disabled bool `json:"disabled"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxPathcheckBody))
	if dec.Decode(&r) != nil || r.Disabled || r.Checked == 0 || !pathClassRe.MatchString(r.Class) {
		return PathCheck{}
	}
	out := r.PathCheck
	out.Known = true
	out.Findings = cleanClasses(out.Findings)
	counts := map[string]int{}
	for k, v := range out.Counts {
		if pathClassRe.MatchString(k) && v > 0 && len(counts) < maxPathFindings {
			counts[k] = v
		}
	}
	out.Counts = counts
	return out
}

func cleanClasses(in []string) []string {
	var out []string
	for _, c := range in {
		if pathClassRe.MatchString(c) && len(out) < maxPathFindings {
			out = append(out, c)
		}
	}
	return out
}

// pathcheckOf asks every bridge agent that reports a path check, in parallel, by bridge name.
func pathcheckOf(ctx context.Context, targets []SyncTarget) map[string]PathCheck {
	out := map[string]PathCheck{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, t := range targets {
		pr, ok := t.Agent.(pathcheckReporter)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(name string, pr pathcheckReporter) {
			defer wg.Done()
			if p := pr.PathCheck(ctx); p.Known {
				mu.Lock()
				out[name] = p
				mu.Unlock()
			}
		}(t.Name, pr)
	}
	wg.Wait()
	return out
}

// pathText says what each class means, for the console. A class the console does not know is
// shown by name only.
var pathText = map[string]string{
	"edge-uplink-bad": "the bridge's own uplink fails: the reference host is unreachable too",
	"dns-failed":      "the name of the control host does not resolve",
	"dns-tampered":    "the name of the control host resolves to an address that is not expected",
	"ip-blocked":      "the address of the control host is unreachable over TCP",
	"sni-blocked":     "TLS with the name of the control host fails, but works without the name",
	"tls-blocked":     "TLS to the control host fails",
	"app-blocked":     "TLS works, but the headscale health request fails",
	"derp-blocked":    "TLS works, but the DERP connection fails",
	"tailnet-down":    "the tailscaled of the bridge is not running",
	"stalled":         "a transfer from the control host starts and then stops",
	"transfer-failed": "a transfer from the control host fails",
	"throttled":       "a transfer from the control host is much slower than from the reference",
	"udp-blocked":     "STUN over UDP gets no answer while TCP works",
	"derp-only":       "the exit node is reached only through DERP, not directly",
}

// pathCritical are the classes that cut a bridge from the tailnet. The others slow it down or
// are early signs: they raise an info item.
var pathCritical = map[string]bool{"dns-failed": true, "dns-tampered": true, "ip-blocked": true,
	"sni-blocked": true, "tls-blocked": true, "app-blocked": true, "derp-blocked": true,
	"tailnet-down": true}

func describePath(class string) string {
	if t, ok := pathText[class]; ok {
		return t
	}
	return class
}

// pathAttention gives one item per bridge whose path check reports the same bad class at least
// pathStreakMin times in a row, and the "Bridge paths" system line of the overview. With no
// bridge reporting a check, it gives nothing: the check is off by default.
func pathAttention(checks map[string]PathCheck) ([]core.AttentionItem, []core.SystemLine) {
	if len(checks) == 0 {
		return nil, nil
	}
	names := make([]string, 0, len(checks))
	for n := range checks {
		names = append(names, n)
	}
	sort.Strings(names)
	var items []core.AttentionItem
	bad := map[string]int{}
	critical := false
	for _, name := range names {
		pc := checks[name]
		if pc.Class == pathClassOK {
			continue
		}
		bad[pc.Class]++
		if pc.Streak < pathStreakMin {
			continue
		}
		it := pathItem(name, pc)
		critical = critical || it.Level == core.LevelCritical
		items = append(items, it)
	}
	line := core.SystemLine{Name: "Bridge paths", Path: "/nodes", State: "ok",
		Summary: pathSummary(len(names), bad)}
	if critical {
		line.State = "critical"
	}
	return items, []core.SystemLine{line}
}

func pathItem(name string, pc PathCheck) core.AttentionItem {
	level := core.LevelInfo
	if pathCritical[pc.Class] {
		level = core.LevelCritical
	}
	detail := "From the network of the bridge, " + describePath(pc.Class) + "."
	if more := otherFindings(pc); more != "" {
		detail += " Also: " + more + "."
	}
	detail += " Seen in " + strconv.Itoa(pc.Streak) + " checks in a row. The check does not " +
		"use the client tunnel. Its results decide if the traffic to the control host must " +
		"be wrapped."
	it := core.AttentionItem{
		ID: "bridge-path-" + name, Source: bridgeSource, Level: level,
		AckKey: "bridge-path-" + name + "|" + pc.Class + "|" + strconv.FormatInt(pc.Since, 10),
		Title:  name + ": path to the control host is " + pc.Class,
		Detail: detail, ActionLabel: "Open bridge",
		ActionPath: "/nodes?sel=" + url.QueryEscape(name),
	}
	if pc.Since > 0 {
		it.Since = time.Unix(pc.Since, 0)
	}
	return it
}

func otherFindings(pc PathCheck) string {
	var out []string
	for _, f := range pc.Findings {
		if f != pc.Class {
			out = append(out, f)
		}
	}
	return strings.Join(out, ", ")
}

func pathSummary(total int, bad map[string]int) string {
	okCount := total
	var parts []string
	classes := make([]string, 0, len(bad))
	for c := range bad {
		classes = append(classes, c)
	}
	sort.Strings(classes)
	for _, c := range classes {
		okCount -= bad[c]
		parts = append(parts, fmt.Sprintf("%d %s", bad[c], c))
	}
	noun := "bridges"
	if total == 1 {
		noun = "bridge"
	}
	s := fmt.Sprintf("%d of %d %s reach the control host cleanly", okCount, total, noun)
	if len(parts) > 0 {
		s += "; " + strings.Join(parts, ", ")
	}
	return s
}
