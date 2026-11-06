package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ackNow is the clock of acknowledgements; a var so tests can move it.
var ackNow = time.Now

const (
	maxAckBody   = 4096
	maxAckExpiry = 90 * 24 * time.Hour
	// ackAuditRead is how many entries of the audit log are searched for acknowledging: the whole bounded log.
	ackAuditRead = 1000
)

// ackKeeper applies acknowledgements to what the sources report. A nil keeper means the feature is off: no module
// offers anywhere to keep them. Its methods are safe on nil.
type ackKeeper struct {
	store AckStore
}

// newAckKeeper finds the store. Modules decide at Register whether they have one, which has run by now.
func newAckKeeper(mods []Module) *ackKeeper {
	for _, s := range sourcesOf[AckSource](mods) {
		if st := s.src.AckStore(); st != nil {
			return &ackKeeper{store: st}
		}
	}
	return nil
}

type ackRef struct{ module, id string }

// heldAcks is what the store held at one moment. ok is false when there is no store or it could not be read: then
// nothing can be acknowledged and no acknowledgement ends, and every item is simply shown.
type heldAcks struct {
	byItem map[ackRef]Acknowledgement
	ok     bool
}

func (k *ackKeeper) load() heldAcks {
	if k == nil {
		return heldAcks{}
	}
	list, err := k.store.Acknowledgements()
	if err != nil {
		slog.Warn("acknowledgements could not be read", "err", err)
		return heldAcks{}
	}
	h := heldAcks{byItem: make(map[ackRef]Acknowledgement, len(list)), ok: true}
	for _, a := range list {
		h.byItem[ackRef{a.Module, a.ID}] = a
	}
	return h
}

// ackMeaning is what an acknowledgement of this item is about, hashed so that it is a fixed size and says nothing.
func ackMeaning(module string, it AttentionItem) string {
	key := it.AckKey
	if key == "" {
		key = it.ID + "|" + string(it.Level)
	}
	sum := sha256.Sum256([]byte(module + "\x00" + key))
	return hex.EncodeToString(sum[:12])
}

func ackCounted(l AttentionLevel) bool {
	return l == LevelTimed || l == LevelCritical || l == LevelWaiting
}

// apply handles the answer of one module that answered: it marks the acknowledged items, offers the others for
// acknowledging, lowers the system lines and ends what no longer holds. A module that errored or is hidden never
// gets here, so neither can end an acknowledgement.
func (k *ackKeeper) apply(module string, items []AttentionItem, lines []SystemLine, held heldAcks) {
	if k == nil || !held.ok {
		return
	}
	now := ackNow()
	seen := map[string]bool{}
	for i := range items {
		it := &items[i]
		seen[it.ID] = true
		meaning := ackMeaning(module, *it)
		a, has := held.byItem[ackRef{module, it.ID}]
		if it.Level != LevelTimed { // what counts down cannot wait for later
			it.AckToken, it.module = meaning, module
		}
		switch {
		case !has:
		case it.Level == LevelTimed || a.Meaning != meaning || (!a.Until.IsZero() && !now.Before(a.Until)):
			k.end(a)
		default:
			shown := a
			it.Acknowledged = &shown
		}
	}
	// An item missing from an answer has cleared, unless the module says it could not fully look.
	if !anyUnknown(lines) {
		for ref, a := range held.byItem {
			if ref.module == module && !seen[ref.id] {
				k.end(a)
			}
		}
	}
	lowerLines(items, lines)
}

func anyUnknown(lines []SystemLine) bool {
	for _, l := range lines {
		if l.State == "unknown" {
			return true
		}
	}
	return false
}

// lowerLines makes a module's waiting or critical lines follow its unacknowledged items only. It never raises a line,
// and with no acknowledged item that counts it changes nothing. With several lines all follow the module's worst item,
// which errs towards showing a problem.
func lowerLines(items []AttentionItem, lines []SystemLine) {
	acked := 0
	worst := "ok"
	for _, it := range items {
		switch {
		case !ackCounted(it.Level):
		case it.Acknowledged != nil:
			acked++
		case it.Level == LevelCritical:
			worst = "critical"
		case worst == "ok":
			worst = "waiting"
		}
	}
	if acked == 0 {
		return
	}
	for i := range lines {
		l := &lines[i]
		if l.State != "critical" && l.State != "waiting" {
			continue
		}
		if l.State == "critical" || worst != "critical" {
			l.State = worst
		}
		note := strconv.Itoa(acked) + " acknowledged"
		if l.Summary != "" {
			note = l.Summary + ". " + note
		}
		l.Summary = cleanText(note, maxSummaryRunes)
	}
}

func (k *ackKeeper) end(a Acknowledgement) {
	if _, err := k.store.DeleteAcknowledgement(a); err != nil {
		slog.Warn("an acknowledgement could not be removed", "err", err)
	}
}

// record writes an audit entry; a failure never fails the action. The detail starts with the module and the item's
// source, which activity reads back: the title is free text, so they come first and are cut off at the first ": ".
func (k *ackKeeper) record(login, action string, it AttentionItem) {
	if err := k.store.Audit(login, action, it.module+"/"+it.Source+": "+it.Title); err != nil {
		slog.Warn("an acknowledgement could not be recorded", "action", action, "err", err)
	}
}

func parseAckDetail(detail string) (module, source, title string, ok bool) {
	head, title, ok := strings.Cut(detail, ": ")
	if !ok {
		return "", "", "", false
	}
	module, source, ok = strings.Cut(head, "/")
	return module, source, title, ok && module != ""
}

// activity lists acknowledging, newest first, for this caller. An entry names an item, so it is shown only to someone
// who can see the module that reported the item, which is asked the way the attention list asks it: a module that hides
// itself hides the entries, and one that cannot say does too, as its visibility is then unknown. The modules are asked
// only for entries that would be listed.
func (k *ackKeeper) activity(ctx context.Context, att []modOf[AttentionSource], nav []NavItem, login string, limit int, only string) (out []ActivityEntry, partial, hidden bool) {
	log, err := k.store.AuditLog(ackAuditRead)
	if err != nil {
		slog.Warn("the acknowledgement log could not be read", "err", err)
		return nil, true, false
	}
	type entry struct {
		e      ActivityEntry
		module string
	}
	var found []entry
	wanted := map[string]bool{}
	for _, e := range log {
		if e.Action != ActionAcknowledge && e.Action != ActionUnacknowledge {
			continue
		}
		module, source, title, ok := parseAckDetail(e.Detail)
		if !ok {
			continue
		}
		label := source
		for _, it := range nav {
			if it.Source == source {
				label = it.Label
				break
			}
		}
		if only != "" && label != only {
			continue
		}
		found = append(found, entry{ActivityEntry{At: e.At, Actor: cleanText(e.Actor, maxShortRunes), Source: cleanText(label, maxShortRunes),
			Action: e.Action, Detail: cleanText(title, maxDetailRunes)}, module})
		wanted[module] = true
		if len(found) == limit {
			break
		}
	}
	if len(found) == 0 {
		return nil, false, false
	}
	var ask []modOf[AttentionSource]
	for _, s := range att {
		if wanted[s.name] {
			ask = append(ask, s)
		}
	}
	_, errs := fanOut(ctx, ask, func(c context.Context, s modOf[AttentionSource]) (struct{}, error) {
		_, _, err := s.src.Attention(c, login)
		return struct{}{}, err
	})
	visible := map[string]bool{}
	for i, s := range ask {
		switch err := errs[i]; {
		case errors.Is(err, ErrNotVisible):
			hidden = true
		case err != nil:
			partial = true
		default:
			visible[s.name] = true
		}
	}
	for _, f := range found {
		if visible[f.module] {
			out = append(out, f.e)
		}
	}
	return out, partial, hidden
}

// ackRequest is the body of both routes. Token is the item's ack_token as the caller was shown it.
type ackRequest struct {
	ID               string `json:"id"`
	Token            string `json:"ack_token"`
	ExpiresInSeconds int64  `json:"expires_in_seconds"`
}

func ackError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"detail": detail})
}

// ackGuard is what guards every console write: only a person on the tailnet, named, may write (a tagged node may not),
// and only with Content-Type application/json, which a cross-site HTML form cannot send. It is authz.RequireJSON's rule
// written out, as authz depends on this package. It decodes a small strict body and passes on the caller's login.
func ackGuard(tn Tailnet, next func(w http.ResponseWriter, r *http.Request, login string, req ackRequest)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var login string
		var tags []string
		var err error
		if tn != nil {
			login, tags, err = tn.WhoIs(r.Context(), r.RemoteAddr)
		}
		if tn == nil || err != nil || len(tags) > 0 || login == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if ct, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || ct != "application/json" {
			ackError(w, http.StatusUnprocessableEntity, "Content-Type must be application/json")
			return
		}
		var req ackRequest
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAckBody))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil || dec.Decode(&struct{}{}) != io.EOF || req.ID == "" || req.Token == "" {
			ackError(w, http.StatusUnprocessableEntity, "id and ack_token are required")
			return
		}
		next(w, r, login, req)
	}
}

// findAckItem looks the item up in what this caller sees right now. Not seeing it reads the same whether it has cleared
// or is hidden from them.
func findAckItem(w http.ResponseWriter, v attentionView, req ackRequest) (AttentionItem, bool) {
	var same *AttentionItem
	for i := range v.items {
		it := &v.items[i]
		if it.ID != req.ID {
			continue
		}
		if it.AckToken == req.Token {
			return *it, true
		}
		same = it
	}
	switch {
	case same == nil:
		ackError(w, http.StatusNotFound, "This item is not in your attention list.")
	case same.AckToken == "":
		ackError(w, http.StatusConflict, "This item cannot be acknowledged.")
	default:
		ackError(w, http.StatusConflict, "This item has changed since you saw it. Look at it again.")
	}
	return AttentionItem{}, false
}

// registerAckRoutes serves the two writes. Each reads the caller's own attention list first: only someone who sees an
// item right now may acknowledge it or undo that.
func registerAckRoutes(mux *http.ServeMux, tn Tailnet, k *ackKeeper, read func(context.Context, string) attentionView) {
	ok := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	}
	mux.HandleFunc("POST /_attention/acknowledge", ackGuard(tn, func(w http.ResponseWriter, r *http.Request, login string, req ackRequest) {
		var expiry time.Duration
		if req.ExpiresInSeconds != 0 {
			expiry = time.Duration(req.ExpiresInSeconds) * time.Second
			if req.ExpiresInSeconds < 0 || req.ExpiresInSeconds > int64(maxAckExpiry/time.Second) {
				ackError(w, http.StatusUnprocessableEntity, "expires_in_seconds must be between 1 second and 90 days")
				return
			}
		}
		it, found := findAckItem(w, read(r.Context(), login), req)
		if !found {
			return
		}
		if it.Acknowledged != nil {
			ackError(w, http.StatusConflict, "This item is already acknowledged.")
			return
		}
		now := ackNow().Truncate(time.Second)
		a := Acknowledgement{Module: it.module, ID: it.ID, Meaning: it.AckToken, By: login, At: now}
		if expiry > 0 {
			a.Until = now.Add(expiry)
		}
		if err := k.store.PutAcknowledgement(a); err != nil {
			slog.Error("an acknowledgement could not be stored", "err", err)
			ackError(w, http.StatusInternalServerError, "The acknowledgement could not be stored.")
			return
		}
		k.record(login, ActionAcknowledge, it)
		ok(w)
	}))
	mux.HandleFunc("POST /_attention/unacknowledge", ackGuard(tn, func(w http.ResponseWriter, r *http.Request, login string, req ackRequest) {
		it, found := findAckItem(w, read(r.Context(), login), req)
		if !found {
			return
		}
		if it.Acknowledged == nil {
			ackError(w, http.StatusNotFound, "This item is not acknowledged.")
			return
		}
		removed, err := k.store.DeleteAcknowledgement(*it.Acknowledged)
		if err != nil {
			slog.Error("an acknowledgement could not be removed", "err", err)
			ackError(w, http.StatusInternalServerError, "The acknowledgement could not be removed.")
			return
		}
		if !removed {
			ackError(w, http.StatusNotFound, "This item is not acknowledged.")
			return
		}
		k.record(login, ActionUnacknowledge, it)
		ok(w)
	}))
}
