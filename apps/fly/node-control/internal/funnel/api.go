package funnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/policy"
)

const (
	maxBodyBytes = 64 << 10
	// staleAfter marks the ingress report stale: three of its default 30 s sync intervals.
	staleAfter = 90 * time.Second

	defaultReusableTTL  = 90 * 24 * time.Hour
	defaultEphemeralTTL = 24 * time.Hour
	minKeyTTL           = time.Hour
	maxKeyTTL           = 365 * 24 * time.Hour
)

type rangeJSON struct {
	Lo int `json:"lo"`
	Hi int `json:"hi"`
}

func toRanges(in []rangeJSON) []Range {
	out := make([]Range, len(in))
	for i, r := range in {
		out[i] = Range{r.Lo, r.Hi}
	}
	return out
}

func fromRanges(in []Range) []rangeJSON {
	out := make([]rangeJSON, len(in))
	for i, r := range in {
		out[i] = rangeJSON{r.Lo, r.Hi}
	}
	return out
}

func (m *Module) registerHuman(mux *http.ServeMux, gate func(http.Handler) http.Handler) {
	get := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(h)) }
	write := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(authz.RequireJSON(h))) }
	get("GET /funnel", asset("text/html; charset=utf-8", pageHTML))
	get("GET /funnel/static/funnel.js", asset("text/javascript; charset=utf-8", pageJS))
	get("GET /funnel/static/funnel.css", asset("text/css; charset=utf-8", pageCSS))
	get("GET /funnel/state", m.state)
	get("GET /funnel/audit", m.audit)
	write("POST /funnel/namespaces", m.createNamespace)
	write("PATCH /funnel/namespaces/{name}", m.patchNamespace)
	write("DELETE /funnel/namespaces/{name}", m.deleteNamespace)
	write("POST /funnel/namespaces/{name}/keys", m.mintKey)
	get("GET /funnel/namespaces/{name}/keys", m.listKeys)
	write("DELETE /funnel/keys/{id}", m.expireKey)
	write("POST /funnel/routes", m.putRoute)
	write("DELETE /funnel/routes/{ns}/{service}", m.deleteRoute)
	write("POST /funnel/blocks", m.putBlock)
	write("DELETE /funnel/blocks/{ns}/{service}", m.deleteBlock)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

func detail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"detail": msg})
}

func readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			detail(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			detail(w, http.StatusUnprocessableEntity, "the body must be a JSON object with the expected fields")
		}
		return false
	}
	return true
}

// storeFail maps a store error to an answer.
func (m *Module) storeFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrInvalid):
		detail(w, http.StatusUnprocessableEntity, trimErr(err))
	case errors.Is(err, ErrNotFound):
		detail(w, http.StatusNotFound, "not found")
	case errors.Is(err, ErrExists), errors.Is(err, ErrBusy), errors.Is(err, ErrInUse):
		detail(w, http.StatusConflict, trimErr(err))
	default:
		m.log.Error("funnel store failed", "err", err)
		detail(w, http.StatusInternalServerError, "the funnel database failed")
	}
}

func trimErr(err error) string { return strings.TrimSpace(err.Error()) }

// aclChange is the result of submitting an ACL change.
type aclChange struct {
	id        int64
	deadline  time.Time
	uncertain bool
	noChange  bool // the live policy already has exactly these managed entries
}

// aclNamespaces is the set of namespaces whose entries the ACL must carry: pending and active ones (a failed
// one never got its entries, or lost them in the revert), with proposed ports where a change is waiting.
func aclNamespaces(nss []Namespace, override map[string][]Range, drop string) map[string][]Range {
	out := map[string][]Range{}
	for _, n := range nss {
		if n.Name == drop || n.State == StateFailed {
			continue
		}
		ports := n.Ports
		if len(n.PendingPorts) > 0 {
			ports = n.PendingPorts
		}
		out[n.Name] = ports
	}
	for name, ports := range override {
		out[name] = ports
	}
	return out
}

// changeACL submits Compose(current, desired) as one policy change.
func (m *Module) changeACL(ctx context.Context, actor string, desired map[string][]Range, reason string) (aclChange, error) {
	id, deadline, err := m.svc.Change(ctx, actor, func(cur string) (string, error) {
		return Compose(cur, desired, m.s)
	}, reason)
	switch {
	case err == nil:
		return aclChange{id: id, deadline: deadline}, nil
	case errors.Is(err, policy.ErrNoChange):
		return aclChange{noChange: true}, nil
	case errors.Is(err, policy.ErrUncertain):
		return aclChange{id: id, deadline: deadline, uncertain: true}, nil
	}
	return aclChange{}, err
}

// aclFail answers a failed policy change.
func (m *Module) aclFail(w http.ResponseWriter, err error) {
	var ae *headscale.APIError
	switch {
	case errors.Is(err, ErrNotPlainJSON), errors.Is(err, ErrPolicyShape):
		detail(w, http.StatusUnprocessableEntity, trimErr(err))
	case errors.Is(err, policy.ErrBusy):
		detail(w, http.StatusConflict, "another policy change is pending: confirm or revert it in the policy console first")
	case errors.Is(err, policy.ErrNoVersion):
		detail(w, http.StatusUnprocessableEntity, "headscale reports no policy version; load one first with the bootstrap command")
	case errors.Is(err, policy.ErrTooLarge):
		detail(w, http.StatusRequestEntityTooLarge, "the resulting policy is too large")
	case errors.As(err, &ae):
		detail(w, http.StatusUnprocessableEntity, ae.Message)
	default:
		m.log.Error("funnel policy change failed", "err", err)
		detail(w, http.StatusBadGateway, "headscale is unreachable or failed")
	}
}

func (m *Module) accepted(w http.ResponseWriter, c aclChange) {
	out := map[string]any{"change_id": c.id, "deadline": c.deadline.UTC().Format(time.RFC3339)}
	if c.uncertain {
		out["uncertain"] = true
		out["detail"] = "outcome unknown; an automatic revert is armed"
	}
	writeJSON(w, http.StatusAccepted, out)
}

// checkPorts rejects ranges that contain a port the ingress reserves for itself: the ingress would refuse the
// whole configuration, and every later change would stall behind it.
func (m *Module) checkPorts(rs []Range) error {
	for _, r := range rs {
		for _, p := range m.s.ReservedPorts {
			if p >= r.Lo && p <= r.Hi {
				return invalid("ports %d-%d contain %d, which the ingress uses itself", r.Lo, r.Hi, p)
			}
		}
	}
	return nil
}

func (m *Module) validPorts(in []rangeJSON) ([]Range, error) {
	rs, err := normalizeRanges(toRanges(in))
	if err != nil {
		return nil, err
	}
	if err := m.checkPorts(rs); err != nil {
		return nil, err
	}
	return rs, nil
}

func (m *Module) createNamespace(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name  string      `json:"name"`
		Ports []rangeJSON `json:"ports"`
		Note  string      `json:"note"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	if err := checkName("namespace name", in.Name); err != nil {
		m.storeFail(w, err)
		return
	}
	ports, err := m.validPorts(in.Ports)
	if err == nil {
		err = checkText("note", in.Note, maxNote)
	}
	if err != nil {
		m.storeFail(w, err)
		return
	}
	actor := authz.LoginFrom(r.Context())

	m.mu.Lock()
	defer m.mu.Unlock()
	nss, err := m.namespaces()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	for _, n := range nss {
		if n.Name == in.Name && n.State != StateFailed {
			detail(w, http.StatusConflict, "that namespace already exists")
			return
		}
	}
	ch, err := m.changeACL(r.Context(), actor, aclNamespaces(nss, map[string][]Range{in.Name: ports}, ""), "funnel: add namespace "+in.Name)
	if err != nil {
		m.aclFail(w, err)
		return
	}
	if err := m.st.CreateNamespace(in.Name, ports, in.Note, ch.id); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(actor, "namespace.create", in.Name+" ports "+FormatPorts(ports))
	if ch.noChange { // the policy already carries these entries: nothing to confirm
		if err := m.st.MarkActive(in.Name); err != nil {
			m.storeFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": StateActive})
		return
	}
	m.accepted(w, ch)
}

func (m *Module) patchNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	var in struct {
		Ports []rangeJSON `json:"ports"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ports, err := m.validPorts(in.Ports)
	if err != nil {
		m.storeFail(w, err)
		return
	}
	actor := authz.LoginFrom(r.Context())

	m.mu.Lock()
	defer m.mu.Unlock()
	nss, err := m.namespaces()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	i := slices.IndexFunc(nss, func(n Namespace) bool { return n.Name == name })
	switch {
	case i < 0:
		detail(w, http.StatusNotFound, "unknown namespace")
		return
	case nss[i].State != StateActive:
		detail(w, http.StatusConflict, "only an active namespace can change its ports")
		return
	case len(nss[i].PendingPorts) > 0:
		detail(w, http.StatusConflict, "a port change is already waiting for confirmation")
		return
	}
	ch, err := m.changeACL(r.Context(), actor, aclNamespaces(nss, map[string][]Range{name: ports}, ""), "funnel: change ports of "+name)
	if err != nil {
		m.aclFail(w, err)
		return
	}
	if err := m.st.SetPendingPorts(name, ports, ch.id); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(actor, "namespace.ports", name+" ports "+FormatPorts(ports))
	if ch.noChange {
		if err := m.st.MarkActive(name); err != nil {
			m.storeFail(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"state": StateActive})
		return
	}
	m.accepted(w, ch)
}

func (m *Module) deleteNamespace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	actor := authz.LoginFrom(r.Context())

	m.mu.Lock()
	defer m.mu.Unlock()
	nss, err := m.namespaces()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	if !slices.ContainsFunc(nss, func(n Namespace) bool { return n.Name == name }) {
		detail(w, http.StatusNotFound, "unknown namespace")
		return
	}
	routes, err := m.st.ManualRoutes()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	if slices.ContainsFunc(routes, func(rt ManualRoute) bool { return rt.NS == name }) {
		detail(w, http.StatusConflict, "the namespace still has manual routes; delete them first")
		return
	}
	if n := m.reportedDynamic(name); n > 0 {
		detail(w, http.StatusConflict, fmt.Sprintf("the ingress still reports %d registered route(s) in this namespace; stop them or block them first", n))
		return
	}
	ch, err := m.changeACL(r.Context(), actor, aclNamespaces(nss, nil, name), "funnel: remove namespace "+name)
	if err != nil {
		m.aclFail(w, err)
		return
	}
	// The row goes with the request. If the policy change is later reverted, the entries stay until the
	// next namespace change regenerates the managed set without them.
	if err := m.st.DeleteNamespace(name); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(actor, "namespace.delete", name)
	keys := m.unexpiredKeys(r.Context(), name)
	if ch.noChange {
		writeJSON(w, http.StatusOK, map[string]any{"state": "deleted", "unexpired_keys": keys})
		return
	}
	out := map[string]any{"change_id": ch.id, "deadline": ch.deadline.UTC().Format(time.RFC3339), "unexpired_keys": keys}
	if ch.uncertain {
		out["uncertain"] = true
	}
	writeJSON(w, http.StatusAccepted, out)
}

// reportedDynamic counts the registered (owner-bound) routes of ns in the latest ingress report.
func (m *Module) reportedDynamic(ns string) int {
	body, _, ok, err := m.st.LatestState()
	if err != nil || !ok {
		return 0
	}
	var rep struct {
		Routes []struct {
			NS    string `json:"ns"`
			Owner string `json:"owner_node_id"`
		} `json:"routes"`
	}
	if json.Unmarshal(body, &rep) != nil {
		return 0
	}
	n := 0
	for _, rt := range rep.Routes {
		if rt.NS == ns && rt.Owner != "" {
			n++
		}
	}
	return n
}

// namespaces lists the namespaces after settling those whose policy change has ended, so an operation made
// right after a confirmation sees the namespace as active without waiting for the ingress to pull.
func (m *Module) namespaces() ([]Namespace, error) { return settle(m.st, m.outcome) }

func (m *Module) audit1(actor, action, detail string) {
	if err := m.st.Audit(actor, action, detail); err != nil {
		m.log.Error("funnel audit failed", "err", err)
	}
}

// --- keys ---

// keyTags are the tags a publisher key for ns carries: fixed by the server, never by the request.
func (m *Module) keyTags(ns string) []string { return []string{m.s.FunnelTag, m.s.NSPrefix + ns} }

func (m *Module) activeNamespace(w http.ResponseWriter, name string) (Namespace, bool) {
	nss, err := m.namespaces()
	if err != nil {
		m.storeFail(w, err)
		return Namespace{}, false
	}
	for _, n := range nss {
		if n.Name == name {
			if n.State != StateActive {
				detail(w, http.StatusConflict, "the namespace is not active yet")
				return Namespace{}, false
			}
			return n, true
		}
	}
	detail(w, http.StatusNotFound, "unknown namespace")
	return Namespace{}, false
}

func (m *Module) mintKey(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TTLHours  *int `json:"ttl_hours"`
		Ephemeral bool `json:"ephemeral"`
		Reusable  bool `json:"reusable"`
	}
	if !readJSON(w, r, &in) { // other fields (tags, user, ...) are ignored on purpose
		return
	}
	name := r.PathValue("name")
	ttl := defaultReusableTTL
	if in.Ephemeral {
		ttl = defaultEphemeralTTL
	}
	if in.TTLHours != nil {
		ttl = time.Duration(*in.TTLHours) * time.Hour
	}
	if ttl < minKeyTTL || ttl > maxKeyTTL {
		detail(w, http.StatusUnprocessableEntity, "ttl_hours must be between 1 and 8760")
		return
	}
	if _, ok := m.activeNamespace(w, name); !ok {
		return
	}
	users, err := m.hs.ListUsers(r.Context())
	if err != nil {
		m.hsFail(w, err)
		return
	}
	idx := slices.IndexFunc(users, func(u headscale.User) bool { return u.Name == m.s.KeyUser })
	if idx < 0 {
		detail(w, http.StatusUnprocessableEntity, "the configured key user does not exist in headscale")
		return
	}
	k, err := m.hs.CreatePreAuthKey(r.Context(), users[idx].ID, m.keyTags(name), ttl, in.Ephemeral, in.Reusable)
	if err != nil {
		m.hsFail(w, err)
		return
	}
	// The secret is returned once. It is never logged, audited or stored.
	m.audit1(authz.LoginFrom(r.Context()), "key.mint", fmt.Sprintf("%s id %d ephemeral=%t reusable=%t ttl=%dh", name, k.ID, in.Ephemeral, in.Reusable, int(ttl/time.Hour)))
	writeJSON(w, http.StatusOK, map[string]any{
		"id": k.ID, "key": k.Key, "expiration": k.Expiration.UTC().Format(time.RFC3339),
		"ephemeral": k.Ephemeral, "reusable": k.Reusable, "tags": k.ACLTags,
	})
}

type keyView struct {
	ID         uint64   `json:"id"`
	Expiration string   `json:"expiration"`
	Expired    bool     `json:"expired"`
	Ephemeral  bool     `json:"ephemeral"`
	Reusable   bool     `json:"reusable"`
	Used       bool     `json:"used"`
	Tags       []string `json:"tags"`
}

func (m *Module) keysFor(ctx context.Context, ns string) ([]keyView, error) {
	all, err := m.hs.ListPreAuthKeys(ctx)
	if err != nil {
		return nil, err
	}
	out := []keyView{}
	for _, k := range all {
		if !slices.Contains(k.ACLTags, m.s.NSPrefix+ns) || !slices.Contains(k.ACLTags, m.s.FunnelTag) {
			continue
		}
		out = append(out, keyView{ID: k.ID, Expiration: k.Expiration.UTC().Format(time.RFC3339), Expired: !k.Expiration.IsZero() && k.Expiration.Before(m.now()),
			Ephemeral: k.Ephemeral, Reusable: k.Reusable, Used: k.Used, Tags: k.ACLTags})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (m *Module) unexpiredKeys(ctx context.Context, ns string) int {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	keys, err := m.keysFor(ctx, ns)
	if err != nil {
		return 0
	}
	n := 0
	for _, k := range keys {
		if !k.Expired {
			n++
		}
	}
	return n
}

func (m *Module) listKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := m.keysFor(r.Context(), r.PathValue("name"))
	if err != nil {
		m.hsFail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

func (m *Module) expireKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseUint(r.PathValue("id"), 10, 64)
	if err != nil {
		detail(w, http.StatusUnprocessableEntity, "the key id must be a number")
		return
	}
	all, err := m.hs.ListPreAuthKeys(r.Context())
	if err != nil {
		m.hsFail(w, err)
		return
	}
	// Only publisher keys (funnel tag plus a namespace tag) can be expired here; this page must not become a
	// way to kill an unrelated key such as the one another app runs on.
	idx := slices.IndexFunc(all, func(k headscale.PreAuthKey) bool {
		return k.ID == id && slices.Contains(k.ACLTags, m.s.FunnelTag) &&
			slices.ContainsFunc(k.ACLTags, func(t string) bool { return strings.HasPrefix(t, m.s.NSPrefix) })
	})
	if idx < 0 {
		detail(w, http.StatusNotFound, "no such publisher key")
		return
	}
	if err := m.hs.ExpirePreAuthKey(r.Context(), id); err != nil {
		m.hsFail(w, err)
		return
	}
	m.audit1(authz.LoginFrom(r.Context()), "key.expire", "id "+strconv.FormatUint(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) hsFail(w http.ResponseWriter, err error) {
	var ae *headscale.APIError
	if errors.As(err, &ae) && ae.Status < 500 {
		detail(w, http.StatusUnprocessableEntity, ae.Message)
		return
	}
	m.log.Error("funnel headscale call failed", "err", err)
	detail(w, http.StatusBadGateway, "headscale is unreachable or failed")
}

// --- manual routes and blocks ---

func (m *Module) putRoute(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NS                 string `json:"ns"`
		Service            string `json:"service"`
		Target             string `json:"target"`
		Port               int    `json:"port"`
		Scheme             string `json:"scheme"`
		InsecureSkipVerify bool   `json:"insecure_skip_verify"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	ns, ok := m.activeNamespace(w, in.NS)
	if !ok {
		return
	}
	if !portIn(ns.Ports, in.Port) {
		detail(w, http.StatusUnprocessableEntity, "the port is outside the namespace's allowed ports")
		return
	}
	nodes, err := m.hs.ListNodes(r.Context())
	if err != nil {
		m.hsFail(w, err)
		return
	}
	want := m.s.NSPrefix + in.NS
	if !slices.ContainsFunc(nodes, func(n headscale.Node) bool {
		return slices.Contains(n.Tags, want) && (n.Name == in.Target || n.GivenName == in.Target || slices.Contains(n.IPAddresses, in.Target))
	}) {
		detail(w, http.StatusUnprocessableEntity, "the target must be a headscale node (name or tailnet address) carrying "+want)
		return
	}
	if err := m.st.PutManual(ManualRoute{NS: in.NS, Service: in.Service, Target: in.Target, Port: in.Port, Scheme: in.Scheme, InsecureSkipVerify: in.InsecureSkipVerify}); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(authz.LoginFrom(r.Context()), "route.put", fmt.Sprintf("%s/%s port %d", in.NS, in.Service, in.Port))
	w.WriteHeader(http.StatusNoContent)
}

func portIn(rs []Range, p int) bool {
	for _, r := range rs {
		if p >= r.Lo && p <= r.Hi {
			return true
		}
	}
	return false
}

func (m *Module) deleteRoute(w http.ResponseWriter, r *http.Request) {
	ns, svc := r.PathValue("ns"), r.PathValue("service")
	if err := m.st.DeleteManual(ns, svc); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(authz.LoginFrom(r.Context()), "route.delete", ns+"/"+svc)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) putBlock(w http.ResponseWriter, r *http.Request) {
	var in struct {
		NS      string `json:"ns"`
		Service string `json:"service"`
		Reason  string `json:"reason"`
	}
	if !readJSON(w, r, &in) {
		return
	}
	actor := authz.LoginFrom(r.Context())
	if err := m.st.Block(in.NS, in.Service, in.Reason, actor); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(actor, "block.put", in.NS+"/"+in.Service)
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) deleteBlock(w http.ResponseWriter, r *http.Request) {
	ns, svc := r.PathValue("ns"), r.PathValue("service")
	if err := m.st.Unblock(ns, svc); err != nil {
		m.storeFail(w, err)
		return
	}
	m.audit1(authz.LoginFrom(r.Context()), "block.delete", ns+"/"+svc)
	w.WriteHeader(http.StatusNoContent)
}

// --- views ---

type checklist struct {
	Records  []map[string]string `json:"records"`
	Commands []string            `json:"commands"`
	Notes    []string            `json:"notes"`
}

func (m *Module) checklistFor(ns string) checklist {
	// FUNNEL_INGRESS_HOSTNAME is the name the wildcard record points at, for example <app>.fly.dev.
	target, app := m.s.IngressHostname, "<your-funnel-ingress-app>"
	if target == "" {
		target = app + ".fly.dev"
	} else if a, ok := strings.CutSuffix(target, ".fly.dev"); ok && !strings.Contains(a, ".") {
		app = a
	}
	host := ns + "." + m.s.PublicDomain
	return checklist{
		Records: []map[string]string{
			{"type": "CNAME", "name": "*." + host + ".", "value": target + "."},
			{"type": "CNAME", "name": "_acme-challenge." + host + ".", "value": "(the value `fly certs add` prints)"},
		},
		Commands: []string{fmt.Sprintf(`fly certs add "*.%s" -a %s`, host, app)},
		Notes: []string{
			"A wildcard does not cover " + host + " itself; add it separately if you want it.",
			"The public domain must not be under the headscale base domain.",
			"Check Fly's current certificate price and limits before adding many namespaces.",
		},
	}
}

type nsView struct {
	Name         string      `json:"name"`
	Ports        []rangeJSON `json:"ports"`
	PendingPorts []rangeJSON `json:"pending_ports"`
	State        string      `json:"state"`
	Note         string      `json:"note"`
	ChangeID     int64       `json:"change_id"`
	Outcome      string      `json:"change_outcome,omitempty"`
	Checklist    checklist   `json:"checklist"`
}

func (m *Module) state(w http.ResponseWriter, r *http.Request) {
	// Settling here makes the page agree with what the ingress will be served on its next pull.
	nss, err := settle(m.st, m.outcome)
	if err != nil {
		m.storeFail(w, err)
		return
	}
	out := map[string]any{
		"public_domain": m.s.PublicDomain, "funnel_tag": m.s.FunnelTag, "ns_prefix": m.s.NSPrefix, "ingress_tag": m.s.IngressTag,
		"reserved_ports": m.s.ReservedPorts,
	}
	views := make([]nsView, 0, len(nss))
	for _, n := range nss {
		v := nsView{Name: n.Name, Ports: fromRanges(n.Ports), PendingPorts: fromRanges(n.PendingPorts), State: n.State, Note: n.Note,
			ChangeID: n.ChangeID, Checklist: m.checklistFor(n.Name)}
		if n.State == StatePending || len(n.PendingPorts) > 0 {
			v.Outcome = string(m.svc.Outcome(n.ChangeID))
		}
		views = append(views, v)
	}
	out["namespaces"] = views
	routes, err := m.st.ManualRoutes()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	rv := make([]map[string]any, 0, len(routes))
	for _, rt := range routes {
		rv = append(rv, map[string]any{"ns": rt.NS, "service": rt.Service, "target": rt.Target, "port": rt.Port, "scheme": rt.Scheme, "insecure_skip_verify": rt.InsecureSkipVerify})
	}
	out["routes"] = rv
	blocks, err := m.st.Blocks()
	if err != nil {
		m.storeFail(w, err)
		return
	}
	bv := make([]map[string]any, 0, len(blocks))
	for _, b := range blocks {
		bv = append(bv, map[string]any{"ns": b.NS, "service": b.Service, "reason": b.Reason, "actor": b.Actor, "at": b.At.UTC().Format(time.RFC3339)})
	}
	out["blocks"] = bv
	out["ingress"] = m.ingressView()
	writeJSON(w, http.StatusOK, out)
}

// ingressView is the latest report with its age. The report body came from a node and is shown as data only.
func (m *Module) ingressView() any {
	body, at, ok, err := m.st.LatestState()
	if err != nil || !ok {
		return nil
	}
	var report any
	if json.Unmarshal(body, &report) != nil {
		return nil
	}
	age := m.now().Sub(at)
	return map[string]any{"received_at": at.UTC().Format(time.RFC3339), "age_seconds": int(age.Seconds()), "stale": age > staleAfter, "report": report}
}

func (m *Module) audit(w http.ResponseWriter, r *http.Request) {
	es, err := m.st.AuditLog(200)
	if err != nil {
		m.storeFail(w, err)
		return
	}
	out := make([]map[string]any, 0, len(es))
	for _, e := range es {
		out = append(out, map[string]any{"id": e.ID, "at": e.At.UTC().Format(time.RFC3339), "actor": e.Actor, "action": e.Action, "detail": e.Detail})
	}
	writeJSON(w, http.StatusOK, map[string]any{"audit": out})
}
