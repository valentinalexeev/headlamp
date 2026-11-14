package tailnetdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
)

// Zones (#285): a zone makes node-control authoritative for every name under it. headscale sends
// the zone to node-control with one nameservers.split line; a name in it without a record does not
// exist (NXDOMAIN). Records under a zone are added as records. A zone holds no records itself:
// deleting it leaves its records, which then need their own split lines.

type zoneReq struct {
	Name string `json:"name"`
	Note string `json:"note,omitempty"`
}

// checkZone normalises and validates a zone name.
func (m *Module) checkZone(z *Zone) error {
	z.Name = servedns.Normalize(z.Name)
	if err := validName(z.Name); err != nil {
		return errors.New("zone " + err.Error())
	}
	m.cur.Lock()
	suffix := m.cur.suffix
	m.cur.Unlock()
	if suffix == "" {
		suffix = m.baseEnv
	}
	if under(z.Name, suffix) || under(suffix, z.Name) {
		return errors.New("zone overlaps the MagicDNS base_domain: " +
			"tailnet devices answer those names themselves")
	}
	if under(z.Name, m.zone) || under(m.zone, z.Name) {
		return errors.New("zone overlaps the serve zone")
	}
	if len(z.Note) > maxNote {
		return fmt.Errorf("note is longer than %d characters", maxNote)
	}
	return nil
}

func (m *Module) createZone(w http.ResponseWriter, r *http.Request) {
	var req zoneReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	z := Zone{Name: req.Name, Note: req.Note}
	if err := m.checkZone(&z); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.st.Zones()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if len(all) >= maxZones {
		http.Error(w, fmt.Sprintf("at most %d zones", maxZones), http.StatusConflict)
		return
	}
	z.Created = m.now().UTC().Truncate(time.Second)
	switch err := m.st.CreateZone(z); {
	case errors.Is(err, ErrExists):
		http.Error(w, "this zone exists", http.StatusConflict)
		return
	case err != nil:
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	m.audit(r, "zone.create", z.Name)
	m.Refresh(context.WithoutCancel(r.Context()))
	writeJSON(w, http.StatusCreated, z)
}

func (m *Module) removeZone(w http.ResponseWriter, r *http.Request) {
	name := servedns.Normalize(r.PathValue("name"))
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.st.DeleteZone(name); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "no such zone", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	m.audit(r, "zone.delete", name)
	m.Refresh(context.WithoutCancel(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}
