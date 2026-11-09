package authz

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// Kind says how a route is checked.
type Kind int

const (
	// KindPerm needs the caller to hold Rule.Perm.
	KindPerm Kind = iota
	// KindAnyUser needs a human caller with at least one permission. The handler filters what it shows by login
	// (attention, activity, search).
	KindAnyUser
	// KindOpen is not checked: health and static assets, which hold no data.
	KindOpen
	// KindMachine is checked by the module itself, by tailnet tag (funnel-ingress). No human may use it.
	KindMachine
)

// Rule is the check for one admin route.
type Rule struct {
	Kind Kind
	Perm Perm
}

func need(f Feature, l Level) Rule { return Rule{Kind: KindPerm, Perm: Perm{f, l}} }

var (
	open    = Rule{Kind: KindOpen}
	anyUser = Rule{Kind: KindAnyUser}
	machine = Rule{Kind: KindMachine}
)

// Routes is the permission of every admin route, by its exact ServeMux pattern. A route that is not here makes
// the server refuse to start (core.Mux.Seal). Keep it in the order of the features.
var Routes = map[string]Rule{
	// core
	"GET /healthz":                   open,
	"GET /_attention.json":           anyUser,
	"GET /_activity.json":            anyUser,
	"GET /_search.json":              anyUser,
	"POST /_attention/acknowledge":   anyUser,
	"POST /_attention/unacknowledge": anyUser,

	// overview
	"GET /{$}":                  need(FeatureOverview, LevelView),
	"GET /overview/overview.js": open,
	"GET /activity":             need(FeatureAudit, LevelView),
	"GET /activity/activity.js": open,
	"GET /tailnet/status":       need(FeatureOverview, LevelView),

	// nodes
	"GET /nodes":             need(FeatureNodes, LevelView),
	"GET /nodes/nodes.js":    open,
	"GET /nodes.json":        need(FeatureNodes, LevelView),
	"GET /status":            need(FeatureNodes, LevelView),
	"GET /nodes/{id}/status": need(FeatureNodes, LevelView),
	"POST /nodes/{id}/start": need(FeatureNodes, LevelOperate),
	"POST /nodes/{id}/stop":  need(FeatureNodes, LevelOperate),

	// enroll
	"GET /enroll":                             need(FeatureEnroll, LevelView),
	"GET /enroll/static/enroll.js":            open,
	"GET /enroll/static/people.js":            open,
	"GET /enroll/qrcode.js":                   open,
	"GET /enroll/approve":                     need(FeatureEnroll, LevelOperate),
	"GET /enroll/static/approve.js":           open,
	"GET /enroll/static/tailnet.js":           open,
	"GET /enroll/devices":                     need(FeatureEnroll, LevelView),
	"POST /enroll/devices":                    need(FeatureEnroll, LevelOperate),
	"DELETE /enroll/devices/{id}":             need(FeatureEnroll, LevelOperate),
	"POST /enroll/devices/{id}/links":         need(FeatureEnroll, LevelOperate),
	"POST /enroll/devices/{id}/revoke":        need(FeatureEnroll, LevelOperate),
	"POST /enroll/devices/{id}/extend":        need(FeatureEnroll, LevelOperate),
	"POST /enroll/devices/{id}/rotate-secret": need(FeatureEnroll, LevelOperate),
	"GET /enroll/ingresses":                   need(FeatureEnroll, LevelView),
	"POST /enroll/ingresses/{name}/approve":   need(FeatureEnroll, LevelAdmin),
	"POST /enroll/ingresses/{name}/unapprove": need(FeatureEnroll, LevelAdmin),
	"GET /enroll/tailnet-acl":                 need(FeatureEnroll, LevelView),
	"GET /enroll/people":                      need(FeatureEnroll, LevelView),
	"POST /enroll/people/{sub}/lock":          need(FeatureEnroll, LevelAdmin),
	"POST /enroll/people/{sub}/unlock":        need(FeatureEnroll, LevelAdmin),
	"GET /enroll/static/invites.js":           open,
	"GET /enroll/invites":                     need(FeatureEnroll, LevelView),
	// An invitation lets a new person in: a relay device and a tailnet key.
	"POST /enroll/invites":             need(FeatureEnroll, LevelAdmin),
	"POST /enroll/invites/{id}/revoke": need(FeatureEnroll, LevelOperate),
	"POST /enroll/tailnet-acl":         need(FeatureEnroll, LevelAdmin),
	// tailnet grants (#139): view to read, admin to change
	"GET /enroll/tailnet-grants":                   need(FeatureEnroll, LevelView),
	"POST /enroll/tailnet-grants/mode":             need(FeatureEnroll, LevelAdmin),
	"POST /enroll/tailnet-grants/targets":          need(FeatureEnroll, LevelAdmin),
	"DELETE /enroll/tailnet-grants/targets/{name}": need(FeatureEnroll, LevelAdmin),
	"POST /enroll/tailnet-grants/subject":          need(FeatureEnroll, LevelAdmin),
	"POST /enroll/tailnet-grants/convert":          need(FeatureEnroll, LevelAdmin),

	// tasks
	"GET /tasks":                       need(FeatureTasks, LevelView),
	"GET /tasks/static/tasks.js":       open,
	"GET /tasks.json":                  need(FeatureTasks, LevelView),
	"GET /tasks/{id}":                  need(FeatureTasks, LevelView),
	"GET /tasks/{id}/runs":             need(FeatureTasks, LevelView),
	"GET /runs/{id}":                   need(FeatureTasks, LevelView),
	"POST /tasks/{id}/run":             need(FeatureTasks, LevelOperate),
	"DELETE /runs/{id}":                need(FeatureTasks, LevelOperate),
	"GET /tasks/config":                need(FeatureTasks, LevelView),
	"PUT /tasks/config":                need(FeatureTasks, LevelAdmin),
	"GET /tasks/secrets":               need(FeatureTasks, LevelAdmin),
	"PUT /tasks/secrets":               need(FeatureTasks, LevelAdmin),
	"GET /sessions":                    need(FeatureTasks, LevelView),
	"GET /sessions/static/sessions.js": open,
	"GET /sessions.json":               need(FeatureTasks, LevelView),
	"POST /sessions/{node}/start":      need(FeatureTasks, LevelOperate),
	"POST /sessions/{node}/stop":       need(FeatureTasks, LevelOperate),

	// policy
	"GET /policy":                  need(FeaturePolicy, LevelView),
	"GET /policy/static/policy.js": open,
	"GET /policy/state":            need(FeaturePolicy, LevelView),
	"POST /policy/preview":         need(FeaturePolicy, LevelView),
	"POST /policy/apply":           need(FeaturePolicy, LevelAdmin),
	"POST /policy/confirm":         need(FeaturePolicy, LevelAdmin),
	"POST /policy/revert/{id}":     need(FeaturePolicy, LevelAdmin),
	"POST /policy/revert-now":      need(FeaturePolicy, LevelAdmin),

	// funnel
	"GET /funnel":                          need(FeatureFunnel, LevelView),
	"GET /funnel/static/funnel.js":         open,
	"GET /funnel/static/config.js":         open,
	"GET /funnel/state":                    need(FeatureFunnel, LevelView),
	"GET /funnel/config":                   need(FeatureFunnel, LevelView),
	"GET /funnel/audit":                    need(FeatureFunnel, LevelView),
	"GET /funnel/namespaces/{name}/keys":   need(FeatureFunnel, LevelAdmin),
	"POST /funnel/namespaces":              need(FeatureFunnel, LevelAdmin),
	"PATCH /funnel/namespaces/{name}":      need(FeatureFunnel, LevelAdmin),
	"DELETE /funnel/namespaces/{name}":     need(FeatureFunnel, LevelAdmin),
	"POST /funnel/namespaces/{name}/keys":  need(FeatureFunnel, LevelAdmin),
	"DELETE /funnel/keys/{id}":             need(FeatureFunnel, LevelAdmin),
	"POST /funnel/routes":                  need(FeatureFunnel, LevelAdmin),
	"DELETE /funnel/routes/{ns}/{service}": need(FeatureFunnel, LevelAdmin),
	"POST /funnel/blocks":                  need(FeatureFunnel, LevelAdmin),
	"DELETE /funnel/blocks/{ns}/{service}": need(FeatureFunnel, LevelAdmin),
	"GET /ingress/v1/config":               machine,
	"POST /ingress/v1/state":               machine,
	// serve (#117): a service is a name plus the pre-auth key its proxies join with
	"GET /serve/state":                       need(FeatureServe, LevelView),
	"POST /serve/services":                   need(FeatureServe, LevelAdmin),
	"DELETE /serve/services/{id}":            need(FeatureServe, LevelAdmin),
	"POST /serve/services/{id}/keys":         need(FeatureServe, LevelAdmin),
	"DELETE /serve/services/{id}/keys/{key}": need(FeatureServe, LevelAdmin),
	"GET /serve/ca.crt":                      open,
	"GET /serve":                             need(FeatureServe, LevelView),
	"GET /serve/static/serve.js":             need(FeatureServe, LevelView),
	"POST /serve/v1/cert":                    machine,

	// admin: roles and grants (#123)
	"GET /admin":                  need(FeatureAdmin, LevelView),
	"GET /admin/static/access.js": need(FeatureAdmin, LevelView),
	"GET /admin/state":            need(FeatureAdmin, LevelView),
	"GET /admin/export":           need(FeatureAdmin, LevelView),
	"GET /admin/audit":            need(FeatureAudit, LevelView),
	"POST /admin/grants":          need(FeatureAdmin, LevelAdmin),
	"PUT /admin/roles/{name}":     need(FeatureAdmin, LevelAdmin),
	"DELETE /admin/roles/{name}":  need(FeatureAdmin, LevelAdmin),
	"POST /admin/import":          need(FeatureAdmin, LevelAdmin),
}

// Authorizer is the core.Authorizer of the admin listener. Open is the transition mode of #122 decision 3: when
// true and the policy grants nothing, KindPerm and KindAnyUser routes pass for every human caller, as they did
// before; a policy with any grant always applies.
type Authorizer struct {
	Policy  *Policy
	Tailnet core.Tailnet
	Log     *slog.Logger
	Open    bool
	Rules   map[string]Rule // nil means Routes
}

type subjectKey struct{}

// SubjectFrom returns the caller set by the Authorizer, if any.
func SubjectFrom(ctx context.Context) (Subject, bool) {
	s, ok := ctx.Value(subjectKey{}).(Subject)
	return s, ok
}

// Wrap implements core.Authorizer.
func (a *Authorizer) Wrap(pattern string, h http.Handler) (http.Handler, error) {
	rules := a.Rules
	if rules == nil {
		rules = Routes
	}
	r, ok := rules[pattern]
	if !ok {
		return nil, fmt.Errorf("no permission for %q", pattern)
	}
	if r.Kind == KindOpen || r.Kind == KindMachine {
		return h, nil
	}
	log := a.Log
	if log == nil {
		log = slog.Default()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		login, tags, err := a.Tailnet.WhoIs(req.Context(), req.RemoteAddr)
		l := NormalizeLogin(login)
		switch {
		case err != nil:
			deny(w, log, "whois_error")
			return
		case len(tags) > 0 || l == "tagged-devices" || l == "":
			deny(w, log, "tagged")
			return
		}
		s := Subject{Login: l}
		if !(a.Open && a.Policy.Empty()) {
			switch r.Kind {
			case KindAnyUser:
				if len(a.Policy.Role(s)) == 0 {
					deny(w, log, "no_role")
					return
				}
			case KindPerm:
				if !a.Policy.Can(s, r.Perm) {
					deny(w, log, "missing_"+strings.ReplaceAll(r.Perm.String(), ".", "_"))
					return
				}
			}
		}
		ctx := context.WithValue(req.Context(), subjectKey{}, s)
		ctx = context.WithValue(ctx, loginKey, l)
		h.ServeHTTP(w, req.WithContext(ctx))
	}), nil
}
