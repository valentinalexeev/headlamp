package funnel

import (
	"encoding/json"
	"time"
)

// IngressStatus is what the fleet view (#195) shows of the funnel ingress: its build version,
// when it last reported and whether that report is fresh and has no errors.
type IngressStatus struct {
	Version    string    `json:"version,omitempty"` // "" for an ingress before 0.16.0
	API        int       `json:"api,omitempty"`     // the funnel API version (#300); 0: before #300
	ReportedAt time.Time `json:"reported_at"`
	Stale      bool      `json:"stale"`
	Errors     int       `json:"errors"`
}

// Ingress is the status of the funnel ingress from its last report, or false when Funnel is off
// or the ingress has not reported.
func (m *Module) Ingress() (IngressStatus, bool) {
	if m.st == nil {
		return IngressStatus{}, false
	}
	body, at, ok, err := m.st.LatestState()
	if err != nil || !ok {
		return IngressStatus{}, false
	}
	var rep struct {
		API     int               `json:"api"`
		Version string            `json:"build_version"`
		Errors  []json.RawMessage `json:"errors"`
	}
	_ = json.Unmarshal(body, &rep)
	if rep.API < 0 || rep.API > maxAPI {
		rep.API = 0
	}
	return IngressStatus{Version: plainVersion(rep.Version), API: rep.API, ReportedAt: at,
		Stale: m.now().Sub(at) > staleAfter, Errors: len(rep.Errors)}, true
}

// plainVersion keeps a short version string of safe characters only; anything else gives "".
func plainVersion(v string) string {
	const maxLen = 64
	if len(v) > maxLen {
		return ""
	}
	for _, c := range v {
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c == '.' || c == '-' || c == '+' || c == '_'
		if !ok {
			return ""
		}
	}
	return v
}

// maxAPI bounds a reported interface version (#300); a bigger number counts as none.
const maxAPI = 1000
