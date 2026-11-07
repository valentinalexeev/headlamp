package enroll

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policy"
)

// PolicyService is the part of the policy console that the tailnet ACL entry needs (#132).
type PolicyService interface {
	Current(ctx context.Context) (text, updatedAt string, err error)
	Change(ctx context.Context, actor string, transform func(current string) (string, error), reason string) (int64, time.Time, error)
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

// want is the dst the rule should have now: none when no bridge grants access or nothing is configured.
func (t *tailnetACL) want(ings []Ingress) []string {
	for _, in := range ings {
		if in.GrantsTailnet() {
			return t.Dst
		}
	}
	return nil
}

// state reports the entry for the console: the wanted dst and whether the live policy has it.
func (t *tailnetACL) state(ctx context.Context, ings []Ingress) map[string]any {
	want := t.want(ings)
	out := map[string]any{"tag": t.Tag, "dst": nonNil(want), "granted": want != nil || anyGrant(ings)}
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
	next, err := funnel.ComposeRule(cur, t.Tag, want)
	switch {
	case err != nil:
		out["status"] = "not_plain_json"
	case next == cur:
		out["status"] = "in_sync"
	default:
		out["status"] = "out_of_sync"
	}
	return out
}

func anyGrant(ings []Ingress) bool {
	for _, in := range ings {
		if in.GrantsTailnet() {
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

// getTailnetACL answers GET /enroll/tailnet-acl.
func (a *Admin) getTailnetACL(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.tacl.state(r.Context(), a.reg.LinkConfig().Ingresses))
}

// applyTailnetACL answers POST /enroll/tailnet-acl: it submits the generated rule as one policy change. The
// policy console then asks to confirm it, or reverts it.
func (a *Admin) applyTailnetACL(w http.ResponseWriter, r *http.Request) {
	svc := a.tacl.service()
	if svc == nil {
		detail(w, http.StatusConflict, "the policy console is not active (POLICY_DB); add the rule to the policy by hand")
		return
	}
	want := a.tacl.want(a.reg.LinkConfig().Ingresses)
	if anyGrant(a.reg.LinkConfig().Ingresses) && len(want) == 0 {
		detail(w, http.StatusUnprocessableEntity, "a bridge grants tailnet access but ENROLL_TAILNET_DST is empty")
		return
	}
	reason := "bridge tailnet access: " + a.tacl.Tag + " -> " + strings.Join(want, ",")
	if want == nil {
		reason = "bridge tailnet access removed"
	}
	id, deadline, err := svc.Change(r.Context(), a.actor(r), func(cur string) (string, error) {
		return funnel.ComposeRule(cur, a.tacl.Tag, want)
	}, reason)
	switch {
	case errors.Is(err, policy.ErrNoChange):
		writeJSON(w, http.StatusOK, map[string]any{"status": "in_sync"})
		return
	case errors.Is(err, policy.ErrBusy):
		detail(w, http.StatusConflict, "another policy change is pending: confirm or revert it in the policy console first")
		return
	case errors.Is(err, funnel.ErrNotPlainJSON), errors.Is(err, funnel.ErrPolicyShape), errors.Is(err, policy.ErrNoVersion):
		detail(w, http.StatusUnprocessableEntity, err.Error())
		return
	case err != nil && !errors.Is(err, policy.ErrUncertain):
		a.internal(w, err)
		return
	}
	a.audit(r, "tailnet_acl", reason)
	out := map[string]any{"change_id": id, "deadline": deadline.UTC().Format(time.RFC3339), "confirm": "/policy"}
	if err != nil {
		out["uncertain"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}
