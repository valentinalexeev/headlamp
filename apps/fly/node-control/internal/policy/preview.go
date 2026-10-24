package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"example.com/node-control/internal/headscale"
)

// Preview is the dry-run result for a proposed policy.
type Preview struct {
	Valid    bool
	Error    string
	Diff     string
	Warnings []string
}

const (
	diffContext  = 3
	maxDiffCells = 4_000_000
	diffTooLarge = "diff omitted: policies too large"
)

// Preview validates after with headscale, diffs it against the live policy and lists warnings.
// Nothing is changed.
func (s *Service) Preview(ctx context.Context, after string) (Preview, error) {
	if len(after) > s.maxBytes {
		return Preview{}, ErrTooLarge
	}
	cur, _, err := s.api.GetPolicy(ctx)
	if err != nil {
		return Preview{}, err
	}
	p := Preview{Valid: true, Diff: policyDiff(cur, after), Warnings: policyWarnings(cur, after)}
	if err := s.api.CheckPolicy(ctx, after); err != nil {
		var ae *headscale.APIError
		if !errors.As(err, &ae) {
			return Preview{}, err
		}
		p.Valid, p.Error = false, ae.Message
	}
	return p, nil
}

// policyDiff diffs two policies, re-indenting both first when both are JSON.
func policyDiff(a, b string) string {
	if ia, ok := indentJSON(a); ok {
		if ib, ok := indentJSON(b); ok {
			a, b = ia, ib
		}
	}
	return unifiedDiff(a, b)
}

func indentJSON(s string) (string, bool) {
	var buf bytes.Buffer
	if err := json.Indent(&buf, []byte(s), "", "  "); err != nil {
		return "", false
	}
	return buf.String(), true
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

type diffOp struct {
	kind byte // ' ', '-' or '+'
	text string
}

// unifiedDiff returns a unified diff of a against b (empty when equal).
func unifiedDiff(a, b string) string {
	x, y := splitLines(a), splitLines(b)
	if len(x)*len(y) > maxDiffCells {
		return diffTooLarge
	}
	ops := diffOps(x, y)
	changed := false
	for _, op := range ops {
		if op.kind != ' ' {
			changed = true
			break
		}
	}
	if !changed {
		return ""
	}
	var out strings.Builder
	out.WriteString("--- current\n+++ proposed\n")
	for _, h := range hunks(ops) {
		writeHunk(&out, ops, h)
	}
	return out.String()
}

// diffOps computes an edit script from x to y with an LCS table over the lines between the
// common prefix and suffix. The table is uint16: the LCS length is at most min(len) <= 2000
// whenever len(x)*len(y) <= maxDiffCells.
func diffOps(x, y []string) []diffOp {
	pre := 0
	for pre < len(x) && pre < len(y) && x[pre] == y[pre] {
		pre++
	}
	suf := 0
	for suf < len(x)-pre && suf < len(y)-pre && x[len(x)-1-suf] == y[len(y)-1-suf] {
		suf++
	}
	mx, my := x[pre:len(x)-suf], y[pre:len(y)-suf]
	n, m := len(mx), len(my)
	w := m + 1
	t := make([]uint16, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if mx[i] == my[j] {
				t[i*w+j] = t[(i+1)*w+j+1] + 1
			} else {
				t[i*w+j] = max(t[(i+1)*w+j], t[i*w+j+1])
			}
		}
	}
	ops := make([]diffOp, 0, len(x)+len(y))
	for _, l := range x[:pre] {
		ops = append(ops, diffOp{' ', l})
	}
	i, j := 0, 0
	for i < n || j < m {
		switch {
		case i < n && j < m && mx[i] == my[j]:
			ops = append(ops, diffOp{' ', mx[i]})
			i, j = i+1, j+1
		case i < n && (j == m || t[(i+1)*w+j] >= t[i*w+j+1]):
			ops = append(ops, diffOp{'-', mx[i]})
			i++
		default:
			ops = append(ops, diffOp{'+', my[j]})
			j++
		}
	}
	for _, l := range x[len(x)-suf:] {
		ops = append(ops, diffOp{' ', l})
	}
	return ops
}

// hunks returns [start, end) ranges of ops, each a run of changes padded with context lines;
// runs whose context would touch are merged.
func hunks(ops []diffOp) [][2]int {
	var out [][2]int
	for i := 0; i < len(ops); i++ {
		if ops[i].kind == ' ' {
			continue
		}
		start, end := max(i-diffContext, 0), min(i+diffContext+1, len(ops))
		if n := len(out); n > 0 && start <= out[n-1][1] {
			out[n-1][1] = end
		} else {
			out = append(out, [2]int{start, end})
		}
	}
	return out
}

func writeHunk(out *strings.Builder, ops []diffOp, h [2]int) {
	oldStart, newStart := 1, 1
	for _, op := range ops[:h[0]] {
		if op.kind != '+' {
			oldStart++
		}
		if op.kind != '-' {
			newStart++
		}
	}
	oldN, newN := 0, 0
	for _, op := range ops[h[0]:h[1]] {
		if op.kind != '+' {
			oldN++
		}
		if op.kind != '-' {
			newN++
		}
	}
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(oldStart, oldN), hunkRange(newStart, newN))
	for _, op := range ops[h[0]:h[1]] {
		out.WriteByte(op.kind)
		out.WriteString(op.text)
		out.WriteByte('\n')
	}
}

// hunkRange formats "start,count" the way diff does: the count is dropped when 1 and an empty
// range starts at the line before.
func hunkRange(start, n int) string {
	switch n {
	case 1:
		return fmt.Sprint(start)
	case 0:
		return fmt.Sprintf("%d,0", start-1)
	}
	return fmt.Sprintf("%d,%d", start, n)
}

// policyWarnings inspects a proposed policy that parses as a JSON object; anything else gets none.
func policyWarnings(cur, after string) []string {
	var next map[string]any
	if json.Unmarshal([]byte(after), &next) != nil || next == nil {
		return nil
	}
	var out []string
	acls, _ := next["acls"].([]any)
	if len(acls) == 0 {
		out = append(out, "acls is empty or missing")
	}
	owners, _ := next["tagOwners"].(map[string]any)
	var unknown []string
	for tag := range usedTags(next) {
		if _, ok := owners[tag]; !ok {
			unknown = append(unknown, tag)
		}
	}
	sort.Strings(unknown)
	for _, tag := range unknown {
		out = append(out, tag+" is used but not listed in tagOwners")
	}
	if n := removedRules(cur, acls); n > 0 {
		out = append(out, fmt.Sprintf("%d rule(s) removed", n))
	}
	return out
}

// usedTags collects the tags named in acls src/dst entries and in autoApprovers values.
func usedTags(policy map[string]any) map[string]bool {
	tags := map[string]bool{}
	acls, _ := policy["acls"].([]any)
	for _, r := range acls {
		rule, _ := r.(map[string]any)
		for _, key := range []string{"src", "dst"} {
			list, _ := rule[key].([]any)
			for _, e := range list {
				if s, ok := e.(string); ok {
					addTag(tags, s)
				}
			}
		}
	}
	collectStrings(policy["autoApprovers"], func(s string) { addTag(tags, s) })
	return tags
}

// addTag records "tag:x" from an entry of the form "tag:x" or "tag:x:ports".
func addTag(tags map[string]bool, entry string) {
	rest, ok := strings.CutPrefix(entry, "tag:")
	if !ok {
		return
	}
	name, _, _ := strings.Cut(rest, ":")
	tags["tag:"+name] = true
}

func collectStrings(v any, f func(string)) {
	switch t := v.(type) {
	case string:
		f(t)
	case []any:
		for _, e := range t {
			collectStrings(e, f)
		}
	case map[string]any:
		for _, e := range t {
			collectStrings(e, f)
		}
	}
}

// removedRules counts acls entries of the current policy (by canonical JSON) absent from next.
func removedRules(cur string, next []any) int {
	var old map[string]any
	if json.Unmarshal([]byte(cur), &old) != nil {
		return 0
	}
	oldRules, _ := old["acls"].([]any)
	kept := map[string]bool{}
	for _, r := range next {
		kept[canonical(r)] = true
	}
	n := 0
	for _, r := range oldRules {
		if !kept[canonical(r)] {
			n++
		}
	}
	return n
}

func canonical(v any) string {
	b, _ := json.Marshal(v) // map keys are sorted
	return string(b)
}
