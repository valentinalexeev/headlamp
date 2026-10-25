package policy

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/headscale"
)

//go:embed static/policy.html
var pageHTML []byte

//go:embed static/policy.js
var pageJS []byte

//go:embed static/policy.css
var pageCSS []byte

const (
	historyLimit = 50
	bodySlack    = 8192 // room for the JSON wrapper around a policy
	// style-src allows inline styles only because the core's top bar is an inline style block inserted into admin pages;
	// scripts stay limited to same-origin files, and this page puts no server text into HTML or styles.
	cspValue = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"
)

// console holds the HTTP handlers of the policy module.
type console struct {
	svc      *Service
	store    *Store
	maxBytes int
}

func newConsole(svc *Service, store *Store, maxBytes int) *console {
	return &console{svc: svc, store: store, maxBytes: maxBytes}
}

// register mounts every route behind the user gate; mutating ones also need a JSON content type.
func (c *console) register(mux *http.ServeMux, gate func(http.Handler) http.Handler) {
	get := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, gate(h))
	}
	post := func(pattern string, h http.HandlerFunc) {
		mux.Handle(pattern, gate(authz.RequireJSON(h)))
	}
	get("GET /policy", asset("text/html; charset=utf-8", pageHTML))
	get("GET /policy/static/policy.js", asset("text/javascript; charset=utf-8", pageJS))
	get("GET /policy/static/policy.css", asset("text/css; charset=utf-8", pageCSS))
	get("GET /policy/state", c.state)
	post("POST /policy/preview", c.preview)
	post("POST /policy/apply", c.apply)
	post("POST /policy/confirm", c.confirm)
	post("POST /policy/revert/{id}", c.revert)
}

// asset serves an embedded file with headers that forbid framing, sniffing, caching and any
// script or style that is not from this origin.
func asset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", contentType)
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", cspValue)
		w.Write(body)
	}
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

// fail maps a service error to an HTTP answer. It never includes policy text, and logs only the
// class of an unexpected error.
func fail(w http.ResponseWriter, err error, id int64, deadline time.Time) {
	var ae *headscale.APIError
	switch {
	case errors.Is(err, ErrUncertain):
		writeJSON(w, http.StatusAccepted, map[string]any{"change_id": id, "deadline": deadline.UTC().Format(time.RFC3339),
			"uncertain": true, "detail": "outcome unknown; an automatic revert is armed"})
	case errors.Is(err, ErrConflict):
		detail(w, http.StatusConflict, "the policy changed since you loaded it")
	case errors.Is(err, ErrBusy):
		detail(w, http.StatusConflict, "another change is pending")
	case errors.Is(err, ErrNothingPending):
		detail(w, http.StatusConflict, "no change is pending")
	case errors.Is(err, ErrNotPending):
		detail(w, http.StatusConflict, "that change is not the pending one")
	case errors.Is(err, ErrNotLive):
		detail(w, http.StatusConflict, "the live policy is not the pending change; reload and check it")
	case errors.Is(err, ErrNoVersion):
		detail(w, http.StatusUnprocessableEntity, "headscale reports no policy version; load one first with the bootstrap command")
	case errors.Is(err, ErrTooLarge):
		detail(w, http.StatusRequestEntityTooLarge, "the policy is too large")
	case errors.Is(err, ErrNoChange):
		detail(w, http.StatusUnprocessableEntity, "the policy is unchanged")
	case errors.Is(err, ErrEmpty):
		detail(w, http.StatusUnprocessableEntity, "the policy is empty")
	case errors.Is(err, ErrNotFound):
		detail(w, http.StatusNotFound, "unknown change")
	case errors.As(err, &ae):
		detail(w, http.StatusUnprocessableEntity, ae.Message)
	default:
		slog.Error("policy console request failed", "cause", errCause(err))
		detail(w, http.StatusBadGateway, "headscale is unreachable or failed")
	}
}

// readJSON decodes a size-limited JSON object body into dst; unknown fields are ignored. It
// answers the error itself and reports whether decoding succeeded.
func (c *console) readJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, int64(2*c.maxBytes+bodySlack)) // JSON escaping can double the text
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

type entryJSON struct {
	ID       int64  `json:"id"`
	At       string `json:"at"`
	Actor    string `json:"actor"`
	Reason   string `json:"reason"`
	State    string `json:"state"`
	Deadline string `json:"deadline"`
}

func toEntry(e Entry) entryJSON {
	return entryJSON{ID: e.ID, At: e.At.UTC().Format(time.RFC3339), Actor: e.Actor, Reason: e.Reason,
		State: string(e.State), Deadline: e.Deadline.UTC().Format(time.RFC3339)}
}

func (c *console) state(w http.ResponseWriter, r *http.Request) {
	text, updated, err := c.svc.Current(r.Context())
	if err != nil {
		fail(w, err, 0, time.Time{})
		return
	}
	hist, err := c.store.History(historyLimit)
	if err != nil {
		fail(w, err, 0, time.Time{})
		return
	}
	p, pending, err := c.store.Pending()
	if err != nil {
		fail(w, err, 0, time.Time{})
		return
	}
	out := map[string]any{"policy": text, "updated_at": updated, "pending": nil, "last_edit": nil,
		"history": make([]entryJSON, 0, len(hist))}
	for _, e := range hist {
		out["history"] = append(out["history"].([]entryJSON), toEntry(e))
	}
	if len(hist) > 0 {
		out["last_edit"] = toEntry(hist[0])
	}
	if pending {
		left := max(int(time.Until(p.Deadline).Seconds()), 0)
		out["pending"] = map[string]any{"id": p.ID, "actor": p.Actor, "reason": p.Reason,
			"deadline": p.Deadline.UTC().Format(time.RFC3339), "seconds_left": left,
			"attempts": p.Attempts, "overdue": time.Now().After(p.Deadline)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (c *console) preview(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Policy *string `json:"policy"`
	}
	if !c.readJSON(w, r, &in) {
		return
	}
	if in.Policy == nil {
		detail(w, http.StatusUnprocessableEntity, "policy is required")
		return
	}
	p, err := c.svc.Preview(r.Context(), *in.Policy)
	if err != nil {
		fail(w, err, 0, time.Time{})
		return
	}
	if p.Warnings == nil {
		p.Warnings = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"valid": p.Valid, "error": p.Error, "diff": p.Diff, "warnings": p.Warnings})
}

func changeResult(w http.ResponseWriter, id int64, deadline time.Time, err error) {
	if err != nil {
		fail(w, err, id, deadline)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"change_id": id, "deadline": deadline.UTC().Format(time.RFC3339)})
}

func (c *console) apply(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Policy *string `json:"policy"`
		Base   string  `json:"base_updated_at"`
	}
	if !c.readJSON(w, r, &in) {
		return
	}
	if in.Policy == nil {
		detail(w, http.StatusUnprocessableEntity, "policy is required")
		return
	}
	id, dl, err := c.svc.Apply(r.Context(), authz.LoginFrom(r.Context()), *in.Policy, in.Base, "edit")
	changeResult(w, id, dl, err)
}

func (c *console) confirm(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ChangeID int64 `json:"change_id"`
	}
	if !c.readJSON(w, r, &in) {
		return
	}
	if in.ChangeID < 1 {
		detail(w, http.StatusUnprocessableEntity, "change_id must be a positive whole number")
		return
	}
	if err := c.svc.Confirm(r.Context(), authz.LoginFrom(r.Context()), in.ChangeID); err != nil {
		fail(w, err, 0, time.Time{})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (c *console) revert(w http.ResponseWriter, r *http.Request) {
	hid, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || hid < 1 {
		detail(w, http.StatusUnprocessableEntity, "id must be a positive whole number")
		return
	}
	id, dl, err := c.svc.RevertTo(r.Context(), authz.LoginFrom(r.Context()), hid)
	changeResult(w, id, dl, err)
}
