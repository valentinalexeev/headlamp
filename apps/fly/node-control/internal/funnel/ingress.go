package funnel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"example.com/node-control/internal/authz"
)

// maxIngressBody caps what the ingress may post as its state report.
const maxIngressBody = 1 << 20

// OutcomeFunc reports how a policy change ended: "pending", "confirmed", "reverted" or "unknown".
type OutcomeFunc func(changeID int64) string

// ConfigDoc is the body of GET /ingress/v1/config.
type ConfigDoc struct {
	Version      string                  `json:"version"`
	PublicDomain string                  `json:"public_domain"`
	Namespaces   map[string]docNamespace `json:"namespaces"`
	Routes       []docRoute              `json:"routes"`
	Blocked      []docBlock              `json:"blocked"`
}

type docNamespace struct {
	Ports []docRange `json:"ports"`
}

type docRange struct {
	Lo int `json:"lo"`
	Hi int `json:"hi"`
}

type docRoute struct {
	NS                 string `json:"ns"`
	Service            string `json:"service"`
	Target             string `json:"target"`
	Port               int    `json:"port"`
	Scheme             string `json:"scheme"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`
}

type docBlock struct {
	NS      string `json:"ns"`
	Service string `json:"service"`
}

// settle moves namespaces whose policy change has ended: confirmed makes them (or their new ports) active,
// reverted marks a new namespace failed (a port change simply drops its pending ports). A change that is
// still pending, or whose outcome is unknown, changes nothing, so a namespace is never served before its ACL
// change is confirmed and never after it was reverted.
func settle(st Store, outcome OutcomeFunc) ([]Namespace, error) {
	nss, err := st.Namespaces()
	if err != nil {
		return nil, err
	}
	changed := false
	for _, n := range nss {
		if n.State != StatePending && !(n.State == StateActive && len(n.PendingPorts) > 0) {
			continue
		}
		switch outcome(n.ChangeID) {
		case "confirmed":
			err = st.MarkActive(n.Name)
		case "reverted":
			err = st.MarkFailed(n.Name)
		default:
			continue
		}
		if err != nil {
			return nil, err
		}
		changed = true
	}
	if changed {
		return st.Namespaces()
	}
	return nss, nil
}

// BuildConfig settles pending namespaces and builds the document the ingress pulls: active namespaces with
// their confirmed ports, the manual routes of active namespaces, and the blocks. The version is the hex
// SHA-256 of the canonical JSON of everything except the version itself.
func BuildConfig(st Store, s *Settings, outcome OutcomeFunc) (ConfigDoc, error) {
	nss, err := settle(st, outcome)
	if err != nil {
		return ConfigDoc{}, err
	}
	doc := ConfigDoc{PublicDomain: s.PublicDomain, Namespaces: map[string]docNamespace{}, Routes: []docRoute{}, Blocked: []docBlock{}}
	active := map[string]bool{}
	for _, n := range nss {
		if n.State != StateActive {
			continue
		}
		active[n.Name] = true
		ports := make([]docRange, 0, len(n.Ports))
		for _, r := range n.Ports {
			ports = append(ports, docRange{r.Lo, r.Hi})
		}
		doc.Namespaces[n.Name] = docNamespace{Ports: ports}
	}
	routes, err := st.ManualRoutes()
	if err != nil {
		return ConfigDoc{}, err
	}
	for _, r := range routes {
		if !active[r.NS] {
			continue
		}
		scheme := r.Scheme
		if scheme == "" {
			scheme = "http"
		}
		doc.Routes = append(doc.Routes, docRoute{r.NS, r.Service, r.Target, r.Port, scheme, r.InsecureSkipVerify})
	}
	sort.Slice(doc.Routes, func(i, j int) bool {
		a, b := doc.Routes[i], doc.Routes[j]
		return a.NS+"/"+a.Service < b.NS+"/"+b.Service
	})
	blocks, err := st.Blocks()
	if err != nil {
		return ConfigDoc{}, err
	}
	for _, b := range blocks {
		doc.Blocked = append(doc.Blocked, docBlock{b.NS, b.Service})
	}
	sort.Slice(doc.Blocked, func(i, j int) bool {
		a, b := doc.Blocked[i], doc.Blocked[j]
		return a.NS+"/"+a.Service < b.NS+"/"+b.Service
	})
	canonical, err := json.Marshal(doc) // struct field order is fixed and map keys are sorted
	if err != nil {
		return ConfigDoc{}, err
	}
	sum := sha256.Sum256(canonical)
	doc.Version = hex.EncodeToString(sum[:])
	return doc, nil
}

// ingress serves the two machine endpoints the funnel-ingress app uses. Only a node carrying the ingress tag
// gets through; everything else, including every human, is refused with 403 and an empty body.
type ingress struct {
	st      Store
	s       *Settings
	outcome OutcomeFunc
	now     func() time.Time
	log     *slog.Logger
}

func (g *ingress) register(mux *http.ServeMux, gate func(http.Handler) http.Handler) {
	mux.Handle("GET /ingress/v1/config", gate(http.HandlerFunc(g.config)))
	mux.Handle("POST /ingress/v1/state", gate(authz.RequireJSON(http.HandlerFunc(g.state))))
}

func (g *ingress) config(w http.ResponseWriter, r *http.Request) {
	doc, err := BuildConfig(g.st, g.s, g.outcome)
	if err != nil {
		g.log.Error("building the ingress configuration failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	etag := `"` + doc.Version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-store")
	for _, cand := range strings.Split(r.Header.Get("If-None-Match"), ",") {
		cand = strings.TrimPrefix(strings.TrimSpace(cand), "W/")
		if cand == etag || cand == doc.Version {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

// stateReport is the shape the ingress posts. Extra fields are ignored; a wrong shape is refused.
type stateReport struct {
	AppliedVersion *string          `json:"applied_version"`
	SentAt         *time.Time       `json:"sent_at"`
	SnapshotLoaded *bool            `json:"snapshot_loaded"`
	PeerCount      *int             `json:"peer_count"`
	Routes         []map[string]any `json:"routes"`
	Errors         []string         `json:"errors"`
}

func (g *ingress) state(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxIngressBody+1))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(body) > maxIngressBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	var rep stateReport
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&rep); err != nil || rep.AppliedVersion == nil || rep.SentAt == nil || rep.SnapshotLoaded == nil ||
		rep.PeerCount == nil || *rep.PeerCount < 0 || rep.Routes == nil || rep.Errors == nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if err := g.st.SaveState(host, body, g.now()); err != nil {
		if errors.Is(err, ErrInvalid) {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		g.log.Error("saving the ingress report failed", "err", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
