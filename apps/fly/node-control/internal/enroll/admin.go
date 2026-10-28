package enroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The operator API and page for enrolment. It is tailnet-only with no auth beyond the network. Every
// POST must carry Content-Type: application/json (else 422), which a cross-site HTML form cannot
// send: this is the CSRF protection. Request bodies are limited to 1 MiB.
//
// Sync visibility: GET /enroll/devices returns {"devices": [...], "sync": {"pending", "state",
// "since"}, "ingresses": [{"name", "host", "sync", "online"}], "pending_ingresses": [...],
// "stats_partial"}; the create/revoke/extend responses carry sync_pending, sync (the aggregate:
// pending if any ingress is pending, the worst state) and ingresses (one entry each).
//
// A change returns once the ingresses answered, or after syncWait at the latest: an ingress that
// is slow or down never holds a revocation back; it stays pending on the page until it acknowledges.
//
// Traffic is keyed by an opaque id, the first 8 hex chars of SHA-256 of the device's "auto" UUID
// (of that ingress's own UUID for a derived ingress) and summed over the ingresses.
// The agent reports stats under that id so the UUID itself never leaves the agent and the store.

//go:embed static/enroll.html
var enrollHTML []byte

// Clients are the accepted DeviceIn.client values.
var Clients = []string{"sing-box", "karing", "clash"}

const (
	clientsRepr  = "('sing-box', 'karing', 'clash')" // Python tuple repr, kept for the 422 detail
	maxBodyBytes = 1 << 20
)

// syncWait bounds how long a change waits for the ingresses before answering.
var syncWait = 3 * time.Second

// Admin serves the operator API.
type Admin struct {
	store *Store
	reg   *Registry
}

// NewAdmin returns the operator API over store and one agent that serves every configured
// ingress. Its sync state takes time from the store's clock.
func NewAdmin(store *Store, cfg IngressConfig, agent Agent) *Admin {
	return &Admin{store: store, reg: newStaticRegistry(store, cfg, agent)}
}

// NewAdminFor returns the operator API over the ingress registry.
func NewAdminFor(store *Store, reg *Registry) *Admin { return &Admin{store: store, reg: reg} }

// State is the sync state of the first configured ingress.
func (a *Admin) State() *SyncState { return a.reg.First().State }

// Register adds the admin routes to mux.
func (a *Admin) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /enroll", a.page)
	mux.HandleFunc("GET /enroll/devices", a.listDevices)
	mux.HandleFunc("POST /enroll/devices", a.jsonOnly(a.createDevice))
	mux.HandleFunc("POST /enroll/devices/{id}/links", a.jsonOnly(a.newLink))
	mux.HandleFunc("POST /enroll/devices/{id}/revoke", a.jsonOnly(a.revoke))
	mux.HandleFunc("POST /enroll/devices/{id}/extend", a.jsonOnly(a.extend))
	mux.HandleFunc("POST /enroll/devices/{id}/rotate-secret", a.jsonOnly(a.rotate))
	mux.HandleFunc("GET /enroll/ingresses", a.listIngresses)
	mux.HandleFunc("POST /enroll/ingresses/{name}/approve", a.jsonOnly(a.approve))
	mux.HandleFunc("POST /enroll/ingresses/{name}/unapprove", a.jsonOnly(a.unapprove))
}

// opaqueID is the id the agent reports traffic under.
func opaqueID(d Device) string {
	sum := sha256.Sum256([]byte(d.UUIDs["auto"]))
	return hex.EncodeToString(sum[:])[:8]
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	b := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	w.Write(b)
}

func detail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"detail": msg})
}

// fieldError is one FastAPI-style validation error.
type fieldError struct {
	Type string `json:"type"`
	Loc  []any  `json:"loc"`
	Msg  string `json:"msg"`
}

func invalid(w http.ResponseWriter, errs ...fieldError) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"detail": errs})
}

func (a *Admin) internal(w http.ResponseWriter, err error) {
	slog.Warn("enrolment request failed", "error_type", errType(err))
	detail(w, http.StatusInternalServerError, "internal error")
}

// jsonOnly applies the CSRF rule and the body limit.
func (a *Admin) jsonOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ct, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";")
		if strings.ToLower(strings.TrimSpace(ct)) != "application/json" {
			detail(w, http.StatusUnprocessableEntity, "Content-Type must be application/json")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		h(w, r)
	}
}

// readObject reads the JSON request body as an object. ok=false means a response was written.
func readObject(w http.ResponseWriter, r *http.Request) (map[string]json.RawMessage, bool) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			detail(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			invalid(w, fieldError{"json_invalid", []any{"body"}, "could not read the request body"})
		}
		return nil, false
	}
	var raw json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		invalid(w, fieldError{"json_invalid", []any{"body", 0}, "JSON decode error"})
		return nil, false
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		invalid(w, fieldError{"model_attributes_type", []any{"body"}, "Input should be a valid dictionary or object to extract fields from"})
		return nil, false
	}
	return obj, true
}

type validator struct{ errs []fieldError }

func (v *validator) add(typ, field, msg string) {
	v.errs = append(v.errs, fieldError{typ, []any{"body", field}, msg})
}

func (v *validator) str(obj map[string]json.RawMessage, field string, min, max int) string {
	raw, ok := obj[field]
	if !ok {
		v.add("missing", field, "Field required")
		return ""
	}
	var s string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &s) != nil {
		v.add("string_type", field, "Input should be a valid string")
		return ""
	}
	n := utf8.RuneCountInString(s)
	if n < min {
		v.add("string_too_short", field, fmt.Sprintf("String should have at least %d character", min))
	} else if n > max {
		v.add("string_too_long", field, fmt.Sprintf("String should have at most %d characters", max))
	}
	return s
}

// integer accepts a JSON integer, an integral number such as 5.0, or a string of digits, like
// pydantic's lax int; bool and null are rejected.
func (v *validator) integer(obj map[string]json.RawMessage, field string, min, max int64) int64 {
	raw, ok := obj[field]
	if !ok {
		v.add("missing", field, "Field required")
		return 0
	}
	text := string(bytes.TrimSpace(raw))
	if strings.HasPrefix(text, `"`) {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			v.add("int_type", field, "Input should be a valid integer")
			return 0
		}
		text = strings.TrimSpace(s)
		if text == "" || strings.Trim(strings.TrimPrefix(text, "-"), "0123456789") != "" {
			v.add("int_parsing", field, "Input should be a valid integer, unable to parse string as an integer")
			return 0
		}
	} else if text == "true" || text == "false" || text == "null" || text == "" || text[0] == '{' || text[0] == '[' {
		v.add("int_type", field, "Input should be a valid integer")
		return 0
	}
	n, err := strconv.ParseInt(text, 10, 64)
	if err != nil {
		f, ferr := strconv.ParseFloat(text, 64)
		if ferr != nil || f != math.Trunc(f) || math.Abs(f) > 1<<53 {
			v.add("int_from_float", field, "Input should be a valid integer, got a number with a fractional part")
			return 0
		}
		n = int64(f)
	}
	if n < min {
		v.add("greater_than_equal", field, fmt.Sprintf("Input should be greater than or equal to %d", min))
	} else if n > max {
		v.add("less_than_equal", field, fmt.Sprintf("Input should be less than or equal to %d", max))
	}
	return n
}

func (a *Admin) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(enrollHTML)
}

type deviceRow struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Client    string `json:"client"`
	ExpiresAt int64  `json:"expires_at"`
	Revoked   bool   `json:"revoked"`
	Traffic   any    `json:"traffic"`
}

// ingressRow is one active ingress in the responses.
type ingressRow struct {
	Name   string       `json:"name"`
	Host   string       `json:"host"`
	Sync   SyncSnapshot `json:"sync"`
	Online *bool        `json:"online"`
}

// rowsOf pairs the current ingresses with sync snapshots (same order as cur.Targets).
func rowsOf(cur Current, targets []SyncTarget, snaps []SyncSnapshot) []ingressRow {
	online := map[string]*bool{}
	for _, v := range cur.Views {
		online[v.Name] = v.Online
	}
	host := map[string]string{}
	for _, in := range cur.Cfg.Ingresses {
		host[in.Name] = in.Host
	}
	rows := make([]ingressRow, 0, len(targets))
	for i, t := range targets {
		rows = append(rows, ingressRow{Name: t.Name, Host: host[t.Name], Sync: snaps[i], Online: online[t.Name]})
	}
	return rows
}

func snapshotsOf(targets []SyncTarget) []SyncSnapshot {
	snaps := make([]SyncSnapshot, len(targets))
	for i, t := range targets {
		snaps[i] = t.State.Snapshot()
	}
	return snaps
}

func (a *Admin) listDevices(w http.ResponseWriter, r *http.Request) {
	cur := a.reg.Current(r.Context(), true)
	devs, err := a.store.ListDevices()
	if err != nil {
		a.internal(w, err)
		return
	}
	stats, partial := MergedStats(r.Context(), cur.SyncTargets, cur.Cfg, devs)
	rows := make([]deviceRow, 0, len(devs))
	for _, d := range devs {
		var traffic any
		if tr, ok := stats[d.ID]; ok {
			traffic = map[string]any{"up": tr.Up, "down": tr.Down}
		}
		rows = append(rows, deviceRow{d.ID, d.Name, d.Owner, d.Client, d.ExpiresAt, d.Revoked, traffic})
	}
	snaps := snapshotsOf(cur.SyncTargets)
	pending := cur.Pending
	if pending == nil {
		pending = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": rows, "sync": summarize(snaps),
		"ingresses": rowsOf(cur, cur.SyncTargets, snaps), "pending_ingresses": pending, "stats_partial": partial})
}

// syncResult is what a change reports about the ingresses.
type syncResult struct {
	Snap SyncSnapshot
	Rows []ingressRow
}

// body is the part of a change response that describes the sync.
func (s syncResult) body(m map[string]any) map[string]any {
	m["sync_pending"], m["sync"], m["ingresses"] = s.Snap.Pending, s.Snap, s.Rows
	return m
}

// syncNow pushes after a change. It outlives the request: a client that gives up must not leave
// the push half done. It answers when every ingress has answered or after syncWait; an ingress
// that has not answered by then reads as pending (state "unknown") until its push ends.
func (a *Admin) syncNow(r *http.Request) syncResult {
	ctx := context.WithoutCancel(r.Context())
	cur := a.reg.Current(ctx, false) // no /v1/info reading on the revoke path: it must not wait for one
	targets := cur.SyncTargets
	before := make([]uint64, len(targets))
	for i, t := range targets {
		before[i] = t.State.version()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("enrolment sync failed", "error_type", typeName(rec))
				for _, t := range targets {
					t.State.Failed("error")
				}
			}
		}()
		SyncAll(ctx, a.store, cur.Cfg, targets)
	}()
	timer := time.NewTimer(syncWait)
	defer timer.Stop()
	finished := false
	select {
	case <-done:
		finished = true
	case <-timer.C:
	}
	snaps := snapshotsOf(targets)
	if !finished {
		now := a.store.clock().Unix()
		for i, t := range targets {
			if t.State.version() == before[i] && !snaps[i].Pending {
				snaps[i] = SyncSnapshot{Pending: true, State: "unknown", Since: &now}
			}
		}
	}
	return syncResult{Snap: summarize(snaps), Rows: rowsOf(cur, targets, snaps)}
}

func (a *Admin) createDevice(w http.ResponseWriter, r *http.Request) {
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var v validator
	name := v.str(obj, "name", 1, 100)
	owner := v.str(obj, "owner", 1, 100)
	client := v.str(obj, "client", 0, math.MaxInt)
	expires := v.integer(obj, "expires_at", math.MinInt64, math.MaxInt64)
	if len(v.errs) > 0 {
		invalid(w, v.errs...)
		return
	}
	valid := false
	for _, c := range Clients {
		valid = valid || c == client
	}
	if !valid {
		detail(w, http.StatusUnprocessableEntity, "client must be one of "+clientsRepr)
		return
	}
	if !a.future(w, expires) {
		return
	}
	d, secret, err := a.store.CreateDevice(name, owner, client, expires)
	if err != nil {
		a.internal(w, err)
		return
	}
	res := a.syncNow(r)
	writeJSON(w, http.StatusOK, res.body(map[string]any{"id": d.ID, "subscription_url": SubURL(a.reg.LinkConfig(), secret)}))
}

func (a *Admin) future(w http.ResponseWriter, ts int64) bool {
	if float64(ts) <= a.store.nowSec() {
		detail(w, http.StatusUnprocessableEntity, "expires_at must be in the future")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		invalid(w, fieldError{"int_parsing", []any{"path", "device_id"}, "Input should be a valid integer, unable to parse string as an integer"})
		return 0, false
	}
	return id, true
}

// known answers 404 for an unknown device.
func (a *Admin) known(w http.ResponseWriter, id int64) bool {
	if _, err := a.store.GetDevice(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			detail(w, http.StatusNotFound, "unknown device")
		} else {
			a.internal(w, err)
		}
		return false
	}
	return true
}

func (a *Admin) newLink(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var v validator
	ttl := v.integer(obj, "ttl", 60, 604800)
	uses := v.integer(obj, "max_uses", 1, 100)
	if len(v.errs) > 0 {
		invalid(w, v.errs...)
		return
	}
	if !a.known(w, id) {
		return
	}
	tok, err := a.store.NewLink(id, time.Duration(ttl)*time.Second, int(uses))
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"url": "https://" + a.reg.LinkConfig().LinkHost + "/e/" + tok})
}

func (a *Admin) revoke(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !a.known(w, id) {
		return
	}
	if err := a.store.Revoke(id); err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.syncNow(r).body(map[string]any{"ok": true}))
}

func (a *Admin) extend(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var v validator
	expires := v.integer(obj, "expires_at", math.MinInt64, math.MaxInt64)
	if len(v.errs) > 0 {
		invalid(w, v.errs...)
		return
	}
	if !a.known(w, id) || !a.future(w, expires) {
		return
	}
	if err := a.store.Extend(id, expires); err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.syncNow(r).body(map[string]any{"ok": true}))
}

func (a *Admin) rotate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok || !a.known(w, id) {
		return
	}
	if err := a.store.RotateSecret(id); err != nil {
		a.internal(w, err)
		return
	}
	secret, err := a.store.SubscriptionSecret(id)
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"subscription_url": SubURL(a.reg.LinkConfig(), secret)})
}

// listIngresses shows every ingress with its approval state. Unapproved ones show only a short
// prefix of their key and short id.
func (a *Admin) listIngresses(w http.ResponseWriter, r *http.Request) {
	cur := a.reg.Current(r.Context(), true)
	pending := cur.Pending
	if pending == nil {
		pending = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ingresses": cur.Views, "pending_ingresses": pending, "discovery": cur.Discovery})
}

// ingressName answers 404 for a name that cannot be an ingress.
func ingressName(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("name")
	if !ingressNameRe.MatchString(name) {
		detail(w, http.StatusNotFound, "unknown bridge")
		return "", false
	}
	return name, true
}

func (a *Admin) approvalFailed(w http.ResponseWriter, err error) {
	var ae *ApprovalError
	if errors.As(err, &ae) {
		detail(w, ae.Status, ae.Msg)
		return
	}
	a.internal(w, err)
}

func (a *Admin) approve(w http.ResponseWriter, r *http.Request) {
	name, ok := ingressName(w, r)
	if !ok {
		return
	}
	obj, ok := readObject(w, r)
	if !ok {
		return
	}
	var v validator
	fp := v.str(obj, "fingerprint", 1, 128)
	nodeID := v.integer(obj, "node_id", 1, math.MaxInt64)
	if len(v.errs) > 0 {
		invalid(w, v.errs...)
		return
	}
	if err := a.reg.Approve(r.Context(), name, fp, uint64(nodeID)); err != nil {
		a.approvalFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.syncNow(r).body(map[string]any{"ok": true}))
}

func (a *Admin) unapprove(w http.ResponseWriter, r *http.Request) {
	name, ok := ingressName(w, r)
	if !ok {
		return
	}
	if _, ok := readObject(w, r); !ok {
		return
	}
	if err := a.reg.Unapprove(r.Context(), name); err != nil {
		a.approvalFailed(w, err)
		return
	}
	writeJSON(w, http.StatusOK, a.syncNow(r).body(map[string]any{"ok": true}))
}
