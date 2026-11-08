package enroll

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policy"
)

// PolicyService is the part of the policy console that the tailnet ACL entry needs (#132).
type PolicyService interface {
	Current(ctx context.Context) (text, updatedAt string, err error)
	Change(
		ctx context.Context,
		actor string,
		transform func(current string) (string, error),
		reason string,
	) (int64, time.Time, error)
}

// WithPolicy gives the module the policy console. fn returns nil when the console is not active.
func WithPolicy(fn func() PolicyService) Option { return func(m *Module) { m.pol = fn } }

const defaultIngressTag = "tag:ingress"

// tailnetACL is the generated entry: one rule "accept src [Tag] dst Dst". Dst comes from ENROLL_TAILNET_DST
// (comma-separated ACL destinations such as "tag:lan:*"). The rule is wanted only while a bridge grants
// tailnet access; otherwise it is removed.
type tailnetACL struct {
	Tag string
	Dst []string
	pol func() PolicyService
}

func parseTailnetDst(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// want is the dst the phase 1 rule should have now: none when no bridge is in mode "all" or nothing
// is configured. A bridge in grants mode does not count (#139).
func (t *tailnetACL) want(ings []Ingress, doc TailnetDoc) []string {
	if anyGrant(ings, doc) {
		return t.Dst
	}
	return nil
}

// aclChange is one line of the preview: a generated rule to add, change or remove.
type aclChange struct {
	Op  string   `json:"op"` // "add", "change" or "remove"
	Src string   `json:"src"`
	Old []string `json:"old,omitempty"`
	New []string `json:"new,omitempty"`
}

// compose applies every generated entry to the policy text: the phase 1 rule and the group rules.
func (t *tailnetACL) compose(
	cur string,
	want1 []string,
	groups map[string][]string,
) (string, error) {
	next, err := funnel.ComposeRule(cur, t.Tag, want1)
	if err != nil {
		return "", err
	}
	return funnel.ComposeGroupRules(next, GroupTagPrefix, t.Tag, groups)
}

// preview lists the changes from the live generated rules to the wanted ones, sorted by src.
func (t *tailnetACL) preview(
	cur string,
	want1 []string,
	groups map[string][]string,
) ([]aclChange, error) {
	live, err := funnel.GroupRules(cur, GroupTagPrefix)
	if err != nil {
		return nil, err
	}
	p1, err := funnel.GroupRules(cur, t.Tag)
	if err != nil {
		return nil, err
	}
	have := map[string][]string{}
	for k, v := range live {
		have[k] = v
	}
	if v, ok := p1[t.Tag]; ok {
		have[t.Tag] = v
	}
	want := map[string][]string{}
	for k, v := range groups {
		want[k] = v
	}
	if len(want1) > 0 {
		want[t.Tag] = sortedCopy(want1)
	}
	return diffRules(have, want), nil
}

func sortedCopy(in []string) []string {
	out := slices.Clone(in)
	slices.Sort(out)
	return slices.Compact(out)
}

func diffRules(have, want map[string][]string) []aclChange {
	out := []aclChange{}
	for src, n := range want {
		o, ok := have[src]
		switch {
		case !ok:
			out = append(out, aclChange{Op: "add", Src: src, New: n})
		case !slices.Equal(o, n):
			out = append(out, aclChange{Op: "change", Src: src, Old: o, New: n})
		}
	}
	for src, o := range have {
		if _, ok := want[src]; !ok {
			out = append(out, aclChange{Op: "remove", Src: src, Old: o})
		}
	}
	slices.SortFunc(out, func(a, b aclChange) int { return strings.Compare(a.Src, b.Src) })
	return out
}

// state reports the entry for the console: the wanted dst, the group rules, the preview and whether
// the live policy has them.
func (t *tailnetACL) state(
	ctx context.Context,
	ings []Ingress,
	doc TailnetDoc,
	groups map[string][]string,
) map[string]any {
	want := t.want(ings, doc)
	out := map[string]any{
		"tag":     t.Tag,
		"dst":     nonNil(want),
		"granted": want != nil || anyGrant(ings, doc),
		"groups":  groups,
		"changes": []aclChange{},
	}
	svc := t.service()
	if svc == nil {
		out["status"] = "no_console"
		return out
	}
	cur, _, err := svc.Current(ctx)
	if err != nil {
		out["status"] = "unknown"
		return out
	}
	next, err := t.compose(cur, want, groups)
	switch {
	case errors.Is(err, funnel.ErrNoTagOwner):
		out["status"] = "no_tag_owner"
	case err != nil:
		out["status"] = "not_plain_json"
	case next == cur:
		out["status"] = "in_sync"
	default:
		out["status"] = "out_of_sync"
	}
	if ch, err := t.preview(cur, want, groups); err == nil {
		out["changes"] = ch
	}
	return out
}

func anyGrant(ings []Ingress, doc TailnetDoc) bool {
	for _, in := range ings {
		if doc.EffectiveMode(in) == TailnetAll {
			return true
		}
	}
	return false
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (t *tailnetACL) service() PolicyService {
	if t == nil || t.pol == nil {
		return nil
	}
	return t.pol()
}

// aclInputs reads what the generated entries need. ok=false means a response was written.
func (a *Admin) aclInputs(
	w http.ResponseWriter,
	r *http.Request,
) ([]Ingress, TailnetDoc, map[string][]string, bool) {
	cur, doc, _, resolved, ok := a.loadGrantState(w, r)
	if !ok {
		return nil, doc, nil, false
	}
	return cur.Cfg.Ingresses, doc, WantedGroupRules(resolved), true
}

// getTailnetACL answers GET /enroll/tailnet-acl: the state and the preview of the generated
// entries.
func (a *Admin) getTailnetACL(w http.ResponseWriter, r *http.Request) {
	ings, doc, groups, ok := a.aclInputs(w, r)
	if ok {
		writeJSON(w, http.StatusOK, a.tacl.state(r.Context(), ings, doc, groups))
	}
}

// applyTailnetACL answers POST /enroll/tailnet-acl: it submits every generated entry as one policy
// change. The policy console then asks to confirm it, or reverts it.
func (a *Admin) applyTailnetACL(w http.ResponseWriter, r *http.Request) {
	svc := a.tacl.service()
	if svc == nil {
		detail(
			w,
			http.StatusConflict,
			"the policy console is not active (POLICY_DB); add the rule to the policy by hand",
		)
		return
	}
	ings, doc, groups, ok := a.aclInputs(w, r)
	if !ok {
		return
	}
	want := a.tacl.want(ings, doc)
	if anyGrant(ings, doc) && len(want) == 0 {
		detail(
			w,
			http.StatusUnprocessableEntity,
			"a bridge grants tailnet access but ENROLL_TAILNET_DST is empty",
		)
		return
	}
	reason := aclReason(a.tacl.Tag, want, groups)
	id, deadline, err := svc.Change(r.Context(), a.actor(r), func(cur string) (string, error) {
		return a.tacl.compose(cur, want, groups)
	}, reason)
	if !a.aclOutcome(w, err) {
		return
	}
	a.audit(r, "tailnet_acl", reason)
	out := map[string]any{
		"change_id": id,
		"deadline":  deadline.UTC().Format(time.RFC3339),
		"confirm":   "/policy",
	}
	if err != nil {
		out["uncertain"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}

func aclReason(tag string, want []string, groups map[string][]string) string {
	reason := "bridge tailnet access: " + tag + " -> " + strings.Join(want, ",")
	if want == nil {
		reason = "bridge tailnet access removed"
	}
	if len(groups) > 0 {
		reason += fmt.Sprintf("; %d grant group rules", len(groups))
	}
	return reason
}

// aclOutcome writes the answer for a failed change. It returns true when the change went ahead
// (also when the result is uncertain).
func (a *Admin) aclOutcome(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, policy.ErrNoChange):
		writeJSON(w, http.StatusOK, map[string]any{"status": "in_sync"})
	case errors.Is(err, policy.ErrBusy):
		detail(
			w,
			http.StatusConflict,
			"another policy change is pending: confirm or revert it in the policy console first",
		)
	case errors.Is(err, funnel.ErrNoTagOwner):
		detail(
			w,
			http.StatusUnprocessableEntity,
			"the policy has no tagOwners entry for "+a.tacl.Tag+": add it, the group tags copy it",
		)
	case errors.Is(err, funnel.ErrNotPlainJSON),
		errors.Is(err, funnel.ErrPolicyShape),
		errors.Is(err, policy.ErrNoVersion):
		detail(w, http.StatusUnprocessableEntity, err.Error())
	case err != nil && !errors.Is(err, policy.ErrUncertain):
		a.internal(w, err)
	default:
		return true
	}
	return false
}
