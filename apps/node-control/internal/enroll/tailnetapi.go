package enroll

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/funnel"
)

// The tailnet grant API (#139). Reads need enroll view, changes need enroll admin
// (authz/routes.go). Every change goes to the audit log, and then to the bridges with one sync.

// grantsMu serialises the read-change-write of the grant document.
var grantsMu sync.Mutex

// grantView is the whole grant state for the console.
type grantView struct {
	Targets []TailnetTarget   `json:"targets"`
	Grants  []TailnetGrant    `json:"grants"`
	Bridges []grantBridgeView `json:"bridges"`
}

type grantBridgeView struct {
	Name       string           `json:"name"`
	Mode       string           `json:"mode"`
	ConfigMode string           `json:"config_mode"`
	Groups     []grantGroupView `json:"groups"`
	Unresolved []string         `json:"unresolved"`
	Error      string           `json:"error,omitempty"`
}

type grantGroupView struct {
	GrantGroup
	// Status is "ok", "needs_provisioning", "pending_acl" or "over_limit".
	Status string `json:"status"`
}

// grantState resolves the grant state of every current bridge. live is the generated rules of the
// live policy (nil: unknown, then no group reads "pending_acl").
func (a *Admin) grantState(
	cur Current,
	doc TailnetDoc,
	devs []Device,
	live map[string][]string,
) (grantView, []BridgeGrants) {
	now := a.store.Clock()().Unix()
	plan := newTailnetPlan(cur.Cfg, cur.Cfg.Ingresses, doc, devs, now)
	view := grantView{
		Targets: AllTargets(cur.Cfg.Targets, doc),
		Grants:  doc.Grants,
		Bridges: []grantBridgeView{},
	}
	if view.Grants == nil {
		view.Grants = []TailnetGrant{}
	}
	var resolved []BridgeGrants
	for _, in := range cur.Cfg.Ingresses {
		bv := grantBridgeView{
			Name:       in.Name,
			Mode:       doc.EffectiveMode(in),
			ConfigMode: Ingress{TailnetAccess: in.TailnetAccess}.modeOrOff(),
			Groups:     []grantGroupView{},
			Unresolved: []string{},
		}
		if bg, ok := plan.bridges[in.Name]; ok {
			resolved = append(resolved, bg)
			bv.Unresolved, bv.Error = bg.Unresolved, bg.Error
			have, known := stateGroups(cur, in.Name)
			for _, g := range bg.Groups {
				bv.Groups = append(
					bv.Groups,
					grantGroupView{GrantGroup: g, Status: groupStatus(g, have, known, live)},
				)
			}
		}
		view.Bridges = append(view.Bridges, bv)
	}
	return view, resolved
}

func (in Ingress) modeOrOff() string {
	if in.TailnetAccess == "" {
		return TailnetOff
	}
	return in.TailnetAccess
}

func stateGroups(cur Current, bridge string) ([]string, bool) {
	for _, t := range cur.SyncTargets {
		if t.Name == bridge {
			return t.State.Groups()
		}
	}
	return nil, false
}

func groupStatus(g GrantGroup, have []string, known bool, live map[string][]string) string {
	switch {
	case g.OverLimit:
		return "over_limit"
	case !known || !slices.Contains(have, g.ID):
		return "needs_provisioning"
	case live != nil && !slices.Equal(live[g.Tag], g.Dst):
		return "pending_acl"
	}
	return "ok"
}

// liveGroupRules reads the generated group rules of the live policy; nil when there is no console.
func (a *Admin) liveGroupRules(r *http.Request) map[string][]string {
	svc := a.tacl.service()
	if svc == nil {
		return nil
	}
	cur, _, err := svc.Current(r.Context())
	if err != nil {
		return nil
	}
	live, err := funnel.GroupRules(cur, GroupTagPrefix)
	if err != nil {
		return nil
	}
	return live
}

// loadGrantState reads the store and resolves the grants. ok=false means a response was written.
func (a *Admin) loadGrantState(
	w http.ResponseWriter,
	r *http.Request,
) (Current, TailnetDoc, grantView, []BridgeGrants, bool) {
	cur := a.reg.Current(r.Context(), false)
	doc, err := a.store.TailnetDoc()
	var devs []Device
	if err == nil {
		devs, err = a.store.ListDevices()
	}
	if err != nil {
		a.internal(w, err)
		return cur, doc, grantView{}, nil, false
	}
	view, resolved := a.grantState(cur, doc, devs, a.liveGroupRules(r))
	return cur, doc, view, resolved, true
}

// getGrants answers GET /enroll/tailnet-grants.
func (a *Admin) getGrants(w http.ResponseWriter, r *http.Request) {
	if _, _, view, _, ok := a.loadGrantState(w, r); ok {
		writeJSON(w, http.StatusOK, view)
	}
}

// changeDoc runs fn on the stored document under the lock, saves it, writes the audit entry and
// syncs.
func (a *Admin) changeDoc(
	w http.ResponseWriter,
	r *http.Request,
	action string,
	fn func(cur Current, d *TailnetDoc) (string, int, string),
) {
	grantsMu.Lock()
	cur := a.reg.Current(r.Context(), false)
	doc, err := a.store.TailnetDoc()
	if err != nil {
		grantsMu.Unlock()
		a.internal(w, err)
		return
	}
	auditText, status, msg := fn(cur, &doc)
	if status != 0 {
		grantsMu.Unlock()
		detail(w, status, msg)
		return
	}
	err = a.store.PutTailnetDoc(doc)
	grantsMu.Unlock()
	if err != nil {
		detail(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	a.audit(r, action, auditText)
	writeJSON(w, http.StatusOK, a.syncNow(r).body(map[string]any{"ok": true}))
}

func bridgeKnown(cur Current, name string) bool {
	return slices.ContainsFunc(cur.Cfg.Ingresses, func(in Ingress) bool { return in.Name == name })
}

// putMode answers POST /enroll/tailnet-grants/mode {"bridge", "mode"}: the console override of
// tailnet_access.
func (a *Admin) putMode(w http.ResponseWriter, r *http.Request) {
	var body struct{ Bridge, Mode string }
	if !readInto(w, r, &body) {
		return
	}
	a.changeDoc(w, r, "tailnet.mode", func(cur Current, d *TailnetDoc) (string, int, string) {
		if !bridgeKnown(cur, body.Bridge) {
			return "", http.StatusNotFound, "unknown bridge"
		}
		if !ValidTailnetMode(body.Mode) {
			return "", http.StatusUnprocessableEntity, `mode must be "off", "all" or "grants"`
		}
		if d.Modes == nil {
			d.Modes = map[string]string{}
		}
		d.Modes[body.Bridge] = body.Mode
		return body.Bridge + ": " + body.Mode, 0, ""
	})
}

// putTarget answers POST /enroll/tailnet-grants/targets {"name", "dst"}: add or change a console
// target.
func (a *Admin) putTarget(w http.ResponseWriter, r *http.Request) {
	var t TailnetTarget
	if !readInto(w, r, &t) {
		return
	}
	a.changeDoc(w, r, "tailnet.target", func(cur Current, d *TailnetDoc) (string, int, string) {
		t.Source = ""
		if err := ValidateTarget(t); err != nil {
			return "", http.StatusUnprocessableEntity, err.Error()
		}
		if slices.ContainsFunc(
			cur.Cfg.Targets,
			func(c TailnetTarget) bool { return c.Name == t.Name },
		) {
			return "", http.StatusConflict, "this target is set in enroll.json; change it there"
		}
		i := slices.IndexFunc(d.Targets, func(c TailnetTarget) bool { return c.Name == t.Name })
		if i < 0 && len(AllTargets(cur.Cfg.Targets, *d)) >= maxTargets {
			return "", http.StatusUnprocessableEntity, fmt.Sprintf(
				"at most %d tailnet targets",
				maxTargets,
			)
		}
		if i < 0 {
			d.Targets = append(d.Targets, t)
		} else {
			d.Targets[i] = t
		}
		return t.Name + " -> " + strings.Join(t.Dst, ","), 0, ""
	})
}

// deleteTarget answers DELETE /enroll/tailnet-grants/targets/{name}: it removes the target and its
// grants.
func (a *Admin) deleteTarget(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	a.changeDoc(
		w,
		r,
		"tailnet.target_remove",
		func(cur Current, d *TailnetDoc) (string, int, string) {
			i := slices.IndexFunc(d.Targets, func(c TailnetTarget) bool { return c.Name == name })
			if i < 0 {
				return "", http.StatusNotFound, "unknown console target"
			}
			d.Targets = slices.Delete(d.Targets, i, i+1)
			d.Grants = slices.DeleteFunc(
				d.Grants,
				func(g TailnetGrant) bool { return g.Target == name },
			)
			return name, 0, ""
		},
	)
}

// setGrants answers POST /enroll/tailnet-grants/subject {"bridge", "kind", "subject", "targets":
// [...]}: it replaces the targets that one subject holds on one bridge.
func (a *Admin) setGrants(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bridge, Kind, Subject string
		Targets               []string
	}
	if !readInto(w, r, &body) {
		return
	}
	a.changeDoc(w, r, "tailnet.grant", func(cur Current, d *TailnetDoc) (string, int, string) {
		code, msg := a.checkSubject(cur, *d, body.Bridge, body.Kind, body.Subject, body.Targets)
		if code != 0 {
			return "", code, msg
		}
		d.Grants = slices.DeleteFunc(d.Grants, func(g TailnetGrant) bool {
			return g.Bridge == body.Bridge && g.Kind == body.Kind && g.Subject == body.Subject
		})
		for _, t := range body.Targets {
			d.Grants = append(
				d.Grants,
				TailnetGrant{
					Bridge:  body.Bridge,
					Kind:    body.Kind,
					Subject: body.Subject,
					Target:  t,
				},
			)
		}
		if len(d.Grants) > maxGrants {
			return "", http.StatusUnprocessableEntity, fmt.Sprintf("at most %d grants", maxGrants)
		}
		return fmt.Sprintf(
			"%s %s %s on %s: [%s]",
			body.Kind,
			body.Subject,
			a.subjectName(body.Kind, body.Subject),
			body.Bridge,
			strings.Join(body.Targets, ","),
		), 0, ""
	})
}

func (a *Admin) checkSubject(
	cur Current,
	d TailnetDoc,
	bridge, kind, subject string,
	targets []string,
) (int, string) {
	if !bridgeKnown(cur, bridge) {
		return http.StatusNotFound, "unknown bridge"
	}
	switch kind {
	case SubjectDevice:
		id, err := strconv.ParseInt(subject, 10, 64)
		if err != nil {
			return http.StatusUnprocessableEntity, "subject must be a device id"
		}
		if _, err := a.store.GetDevice(id); err != nil {
			return http.StatusNotFound, "unknown device"
		}
	case SubjectOwner, SubjectGroup:
		if subject == "" || len(subject) > 200 || strings.ContainsAny(subject, "\x00\r\n") {
			return http.StatusUnprocessableEntity, "subject must be 1-200 characters"
		}
	default:
		return http.StatusUnprocessableEntity, `kind must be "device", "owner" or "group"`
	}
	all := AllTargets(cur.Cfg.Targets, d)
	for _, t := range targets {
		if !slices.ContainsFunc(all, func(c TailnetTarget) bool { return c.Name == t }) {
			return http.StatusUnprocessableEntity, "unknown target " + strconv.Quote(t)
		}
	}
	return 0, ""
}

func (a *Admin) subjectName(kind, subject string) string {
	if kind != SubjectDevice {
		return ""
	}
	id, _ := strconv.ParseInt(subject, 10, 64)
	return "(" + a.deviceName(id) + ")"
}

// convertToGrants answers POST /enroll/tailnet-grants/convert {"bridge"}: the one-time migration
// from "all". It makes the target "all" from ENROLL_TAILNET_DST, grants it to every active device,
// and sets the bridge to grants mode, so access does not change. The operator then narrows the
// grants.
func (a *Admin) convertToGrants(w http.ResponseWriter, r *http.Request) {
	var body struct{ Bridge string }
	if !readInto(w, r, &body) {
		return
	}
	devs, err := a.store.ListDevices()
	if err != nil {
		a.internal(w, err)
		return
	}
	now := a.store.Clock()().Unix()
	a.changeDoc(w, r, "tailnet.convert", func(cur Current, d *TailnetDoc) (string, int, string) {
		return convertDoc(cur, d, a.tacl, devs, now, body.Bridge)
	})
}

func convertDoc(
	cur Current,
	d *TailnetDoc,
	tacl *tailnetACL,
	devs []Device,
	now int64,
	bridge string,
) (string, int, string) {
	i := slices.IndexFunc(cur.Cfg.Ingresses, func(in Ingress) bool { return in.Name == bridge })
	if i < 0 {
		return "", http.StatusNotFound, "unknown bridge"
	}
	if d.EffectiveMode(cur.Cfg.Ingresses[i]) != TailnetAll {
		return "", http.StatusConflict, `the bridge is not in mode "all"`
	}
	if tacl == nil || len(tacl.Dst) == 0 {
		return "", http.StatusUnprocessableEntity,
			"ENROLL_TAILNET_DST is empty: there is nothing to convert"
	}
	t := TailnetTarget{Name: "all", Dst: tacl.Dst}
	if err := ValidateTarget(t); err != nil {
		return "", http.StatusUnprocessableEntity, "ENROLL_TAILNET_DST: " + err.Error()
	}
	if !slices.ContainsFunc(
		AllTargets(cur.Cfg.Targets, *d),
		func(c TailnetTarget) bool { return c.Name == "all" },
	) {
		d.Targets = append(d.Targets, t)
	}
	n := 0
	for _, dv := range devs {
		if dv.Revoked || dv.ExpiresAt <= now {
			continue
		}
		g := TailnetGrant{
			Bridge:  bridge,
			Kind:    SubjectDevice,
			Subject: strconv.FormatInt(dv.ID, 10),
			Target:  "all",
		}
		if !slices.Contains(d.Grants, g) {
			d.Grants = append(d.Grants, g)
		}
		n++
	}
	if len(d.Grants) > maxGrants {
		return "", http.StatusUnprocessableEntity, fmt.Sprintf("at most %d grants", maxGrants)
	}
	if d.Modes == nil {
		d.Modes = map[string]string{}
	}
	d.Modes[bridge] = TailnetGrants
	return fmt.Sprintf("%s: target all granted to %d devices, mode grants", bridge, n), 0, ""
}

// readInto decodes the JSON body into v. ok=false means a response was written.
func readInto(w http.ResponseWriter, r *http.Request, v any) bool {
	obj, ok := readObject(w, r)
	if !ok {
		return false
	}
	b, _ := json.Marshal(obj)
	if err := json.Unmarshal(b, v); err != nil {
		detail(w, http.StatusUnprocessableEntity, "the body has a field of the wrong type")
		return false
	}
	return true
}
