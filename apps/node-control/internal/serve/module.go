package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// Nodes lists the tailnet nodes, so the module can map a caller's address to its stable node id and keep serve
// names from shadowing node names.
type Nodes interface {
	Nodes(ctx context.Context) ([]headscale.Node, error)
}

// Module is the serve module (#117). It is on when SERVE_DB is set.
type Module struct {
	env     core.Env
	nodes   Nodes
	log     *slog.Logger
	st      Store
	ca      *CA
	base    string
	nodeTag string
	nsTag   string
	syncTag string
	now     func() time.Time
	sync    syncState
}

// New returns the serve module.
func New(env core.Env, nodes Nodes) core.Module {
	return &Module{env: env, nodes: nodes, now: time.Now}
}

func (m *Module) Name() string { return "serve" }

const (
	caCertKey = "ca-cert"
	caKeyKey  = "ca-key"
	sealScope = "serve-ca"
)

func (m *Module) Register(r *core.Routes) error {
	m.log = r.Log
	if m.log == nil {
		m.log = slog.Default()
	}
	path := m.env.Getenv("SERVE_DB")
	if path == "" {
		return nil
	}
	const on = "serve enabled (SERVE_DB set)"
	m.base = NormalizeName(m.env.Getenv("SERVE_BASE_DOMAIN"))
	if m.base == "" {
		return errors.New(on + " but SERVE_BASE_DOMAIN (the headscale MagicDNS base_domain) is not set")
	}
	m.nodeTag = m.env.Default("SERVE_NODE_TAG", "tag:serve")
	m.nsTag = m.env.Default("SERVE_NS_TAG_PREFIX", "tag:ns-")
	m.syncTag = m.env.Default("SERVE_SYNC_TAG", "tag:headscale")
	for k, v := range map[string]string{"SERVE_NODE_TAG": m.nodeTag, "SERVE_NS_TAG_PREFIX": m.nsTag, "SERVE_SYNC_TAG": m.syncTag} {
		if !strings.HasPrefix(v, "tag:") || strings.ContainsAny(v, " ,") {
			return fmt.Errorf("%s must be a single tailnet tag (tag:...)", k)
		}
	}
	if !authz.AnyoneCan(authz.P(authz.FeatureServe, authz.LevelAdmin), authz.Allowed{}) && authz.Default() != nil && !authz.Default().Empty() {
		return errors.New(on + " but ADMIN_CONFIG grants serve.admin to nobody")
	}
	if r.Tailnet == nil {
		return errors.New(on + " but the core has no tailnet node to identify callers")
	}
	st, err := OpenSQLite(path, m.now)
	if err != nil {
		return err
	}
	ca, err := m.loadCA(st)
	if err != nil {
		st.Close()
		return err
	}
	m.st, m.ca = st, ca

	view := authz.Gate(r.Tailnet, authz.P(authz.FeatureServe, authz.LevelView), authz.Allowed{}, m.log)
	admin := authz.Gate(r.Tailnet, authz.P(authz.FeatureServe, authz.LevelAdmin), authz.Allowed{}, m.log)
	json := func(h http.HandlerFunc) http.Handler { return authz.RequireJSON(h) }

	r.Admin.Handle("GET /serve/state", view(http.HandlerFunc(m.state)))
	r.Admin.Handle("PUT /serve/namespaces/{name}", admin(json(m.putNamespace)))
	r.Admin.Handle("DELETE /serve/namespaces/{name}", admin(http.HandlerFunc(m.deleteNamespace)))
	r.Admin.Handle("DELETE /serve/names/{name}", admin(http.HandlerFunc(m.dropName)))
	r.Admin.HandleFunc("GET /serve/ca.crt", m.caCert)

	node := m.nodeGate(r.Tailnet)
	r.Admin.Handle("POST /serve/v1/register", node(json(m.register)))
	r.Admin.Handle("DELETE /serve/v1/names/{name}", node(http.HandlerFunc(m.unregister)))

	r.Admin.Handle("GET /serve/v1/records", authz.TagGate(r.Tailnet, m.syncTag, m.log)(http.HandlerFunc(m.records)))
	r.Admin.Handle("POST /serve/v1/state", authz.TagGate(r.Tailnet, m.syncTag, m.log)(json(m.syncReport)))
	return nil
}

func (m *Module) Start(context.Context) error { return nil }

// Stop closes the store after the servers are down.
func (m *Module) Stop(context.Context) error {
	if m.st != nil {
		return m.st.Close()
	}
	return nil
}

// loadCA uses SERVE_CA_CERT and SERVE_CA_KEY (PEM, the operator's CA or intermediate with its chain) when set.
// Otherwise it loads the self-signed CA kept in the store, sealed with NODE_CONTROL_STATE_KEY, creating it on first
// start. Without that key there is nowhere safe to keep a generated CA key, so serve refuses to start.
func (m *Module) loadCA(st Store) (*CA, error) {
	certPEM, keyPEM := m.env.Getenv("SERVE_CA_CERT"), m.env.Getenv("SERVE_CA_KEY")
	switch {
	case certPEM != "" && keyPEM != "":
		return LoadCA([]byte(certPEM), []byte(keyPEM), "operator")
	case certPEM != "" || keyPEM != "":
		return nil, errors.New("serve: set both SERVE_CA_CERT and SERVE_CA_KEY, or neither for a self-signed CA")
	}
	ciph, err := statestore.NewCipher(m.env.Getenv("NODE_CONTROL_STATE_KEY"))
	if err == nil && ciph == nil {
		// An empty key means "no encryption" to the cipher. A CA key must never be kept in clear.
		err = errors.New("NODE_CONTROL_STATE_KEY is not set")
	}
	if err != nil {
		return nil, fmt.Errorf("serve: a self-signed CA needs NODE_CONTROL_STATE_KEY to keep its key encrypted: %w", err)
	}
	if cert, err := st.GetSecret(caCertKey); err == nil {
		sealed, err := st.GetSecret(caKeyKey)
		if err != nil {
			return nil, fmt.Errorf("serve: CA key: %w", err)
		}
		key, err := ciph.Open(sealScope, caKeyKey, sealed)
		if err != nil {
			return nil, fmt.Errorf("serve: CA key does not decrypt (NODE_CONTROL_STATE_KEY changed?): %w", err)
		}
		return LoadCA(cert, key, "self-signed")
	} else if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	ca, cert, key, err := NewSelfSignedCA("headlamp serve CA ("+m.base+")", m.now())
	if err != nil {
		return nil, err
	}
	sealed, err := ciph.Seal(sealScope, caKeyKey, key)
	if err != nil {
		return nil, err
	}
	if err := st.PutSecret(caKeyKey, sealed); err != nil {
		return nil, err
	}
	if err := st.PutSecret(caCertKey, cert); err != nil {
		return nil, err
	}
	m.log.Info("serve: created a self-signed CA; give users /serve/ca.crt to trust", "fingerprint", ca.Fingerprint())
	return ca, nil
}

// --- node side ---

type nodeKey struct{}

type caller struct {
	id, name   string
	ip         netip.Addr
	namespaces []string
}

// nodeGate lets through a tailnet node with the serve tag and at least one namespace tag. It resolves the node's
// stable id and address from headscale by the caller's address: WhoIs gives tags, headscale gives identity.
func (m *Module) nodeGate(tn core.Tailnet) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, tags, err := tn.WhoIs(r.Context(), r.RemoteAddr)
			if err != nil || !slices.Contains(tags, m.nodeTag) {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			var nss []string
			for _, t := range tags {
				if ns, ok := strings.CutPrefix(t, m.nsTag); ok && ns != "" {
					nss = append(nss, ns)
				}
			}
			if len(nss) == 0 {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			ip, err := netip.ParseAddr(host)
			if err != nil {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			c := caller{ip: ip.Unmap(), namespaces: nss}
			ns, err := m.nodes.Nodes(r.Context())
			if err != nil {
				http.Error(w, "headscale unavailable", http.StatusServiceUnavailable)
				return
			}
			for _, n := range ns {
				if slices.Contains(n.IPAddresses, c.ip.String()) {
					c.id, c.name = strconv.FormatUint(n.ID, 10), n.GivenName
					break
				}
			}
			if c.id == "" {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), nodeKey{}, c)))
		})
	}
}

func (m *Module) policy(ctx context.Context) (Policy, error) {
	nss, err := m.st.Namespaces()
	if err != nil {
		return Policy{}, err
	}
	p := Policy{BaseDomain: m.base, Suffixes: map[string][]string{}}
	for _, ns := range nss {
		p.Suffixes[ns.Name] = ns.Suffixes
	}
	nodes, err := m.nodes.Nodes(ctx)
	if err != nil {
		return Policy{}, err
	}
	taken := map[string]bool{}
	for _, n := range nodes {
		for _, nm := range []string{n.GivenName, n.Name} {
			if nm != "" {
				taken[NormalizeName(nm)+"."+m.base] = true
			}
		}
	}
	p.NodeNames = func(n string) bool { return taken[n] }
	return p, nil
}

type registerReq struct {
	Namespace string   `json:"namespace"`
	Names     []string `json:"names"`
	CSR       string   `json:"csr"`
	TTLHours  int      `json:"ttl_hours,omitempty"`
}

type registerResp struct {
	Names   []string  `json:"names"`
	Cert    string    `json:"cert"`
	Chain   string    `json:"chain"`
	Expires time.Time `json:"expires"`
}

const maxNamesPerRequest = 16

// register claims names for the calling node, at its own tailnet address, and signs its CSR for exactly those
// names. Every name must pass the namespace policy and be free or already held by this node; one refusal refuses
// the whole request, so a node never gets a certificate for a name it does not hold.
func (m *Module) register(w http.ResponseWriter, r *http.Request) {
	c := r.Context().Value(nodeKey{}).(caller)
	var req registerReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	ns := req.Namespace
	if ns == "" && len(c.namespaces) == 1 {
		ns = c.namespaces[0]
	}
	if !slices.Contains(c.namespaces, ns) {
		http.Error(w, "the node is not in that namespace", http.StatusForbidden)
		return
	}
	if len(req.Names) == 0 || len(req.Names) > maxNamesPerRequest {
		http.Error(w, fmt.Sprintf("give 1 to %d names", maxNamesPerRequest), http.StatusUnprocessableEntity)
		return
	}
	if _, err := RecordFor("x", c.ip); err != nil {
		http.Error(w, "caller address is not a tailnet address", http.StatusForbidden)
		return
	}
	pol, err := m.policy(r.Context())
	if err != nil {
		http.Error(w, "headscale unavailable", http.StatusServiceUnavailable)
		return
	}
	var names []string
	for _, n := range req.Names {
		full, err := pol.Check(ns, n)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		if !slices.Contains(names, full) {
			names = append(names, full)
		}
	}
	// Check every name is free or ours before claiming any, so a refusal leaves nothing half done.
	regs, err := m.st.Registrations()
	if err != nil {
		http.Error(w, "store", http.StatusInternalServerError)
		return
	}
	for _, rg := range regs {
		if slices.Contains(names, rg.Name) && rg.NodeID != c.id {
			http.Error(w, rg.Name+": "+ErrTaken.Error(), http.StatusConflict)
			return
		}
	}
	cert, exp, err := m.ca.SignCSR([]byte(req.CSR), names, time.Duration(req.TTLHours)*time.Hour)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	for _, n := range names {
		if err := m.st.Register(Registration{Name: n, Namespace: ns, NodeID: c.id, NodeName: c.name, IP: c.ip}); err != nil {
			code := http.StatusInternalServerError
			if errors.Is(err, ErrTaken) {
				code = http.StatusConflict
			}
			http.Error(w, n+": "+err.Error(), code)
			return
		}
		_ = m.st.SetCertExpiry(n, exp)
	}
	m.log.Info("serve: names registered", "node", c.name, "namespace", ns, "names", strings.Join(names, ","))
	writeJSON(w, registerResp{Names: names, Cert: string(cert), Chain: string(m.ca.ChainPEM()), Expires: exp})
}

func (m *Module) unregister(w http.ResponseWriter, r *http.Request) {
	c := r.Context().Value(nodeKey{}).(caller)
	switch err := m.st.Unregister(NormalizeName(r.PathValue("name")), c.id); {
	case errors.Is(err, ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

// --- records-sync agent ---

type recordsResp struct {
	Version int64    `json:"version"`
	Records []Record `json:"records"`
}

// records is the list headscale serves, for the records-sync agent. It never contains anything but names under the
// base domain at tailnet addresses: the store accepted them only that way.
func (m *Module) records(w http.ResponseWriter, _ *http.Request) {
	v, err := m.st.Version()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	regs, err := m.st.Registrations()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	out := recordsResp{Version: v, Records: []Record{}}
	for _, rg := range regs {
		if rec, err := RecordFor(rg.Name, rg.IP); err == nil {
			out.Records = append(out.Records, rec)
		}
	}
	writeJSON(w, out)
}

// syncState is what the agent last reported.
type syncState struct {
	Version  int64     `json:"version"`
	Applied  time.Time `json:"applied"`
	Error    string    `json:"error,omitempty"`
	Reported time.Time `json:"reported"`
}

func (m *Module) syncReport(w http.ResponseWriter, r *http.Request) {
	var s syncState
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&s); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(s.Error) > 300 {
		s.Error = s.Error[:300]
	}
	s.Reported = m.now()
	m.sync = s
	w.WriteHeader(http.StatusNoContent)
}

// --- admin ---

type stateResp struct {
	BaseDomain    string         `json:"base_domain"`
	CA            string         `json:"ca"`
	Fingerprint   string         `json:"ca_fingerprint"`
	Version       int64          `json:"version"`
	Sync          syncState      `json:"sync"`
	Namespaces    []Namespace    `json:"namespaces"`
	Registrations []Registration `json:"registrations"`
}

func (m *Module) state(w http.ResponseWriter, _ *http.Request) {
	nss, err1 := m.st.Namespaces()
	regs, err2 := m.st.Registrations()
	v, err3 := m.st.Version()
	if err := errors.Join(err1, err2, err3); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	if nss == nil {
		nss = []Namespace{}
	}
	if regs == nil {
		regs = []Registration{}
	}
	writeJSON(w, stateResp{BaseDomain: m.base, CA: m.ca.Issued, Fingerprint: m.ca.Fingerprint(), Version: v, Sync: m.sync,
		Namespaces: nss, Registrations: regs})
}

type namespaceReq struct {
	Suffixes []string `json:"suffixes"`
	Note     string   `json:"note"`
}

func (m *Module) putNamespace(w http.ResponseWriter, r *http.Request) {
	var req namespaceReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	if len(req.Suffixes) == 0 {
		http.Error(w, "give at least one suffix", http.StatusUnprocessableEntity)
		return
	}
	var zones []string
	for _, s := range req.Suffixes {
		z, err := CheckSuffix(s, m.base)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		zones = append(zones, z)
	}
	if len(req.Note) > 200 {
		http.Error(w, "note too long", http.StatusUnprocessableEntity)
		return
	}
	if err := m.st.PutNamespace(Namespace{Name: r.PathValue("name"), Suffixes: zones, Note: req.Note}); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	m.log.Info("serve: namespace set", "by", authz.LoginFrom(r.Context()), "namespace", r.PathValue("name"), "zones", strings.Join(zones, ","))
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) deleteNamespace(w http.ResponseWriter, r *http.Request) {
	switch err := m.st.DeleteNamespace(r.PathValue("name")); {
	case errors.Is(err, ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		m.log.Info("serve: namespace deleted", "by", authz.LoginFrom(r.Context()), "namespace", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}
}

func (m *Module) dropName(w http.ResponseWriter, r *http.Request) {
	switch err := m.st.DropName(NormalizeName(r.PathValue("name"))); {
	case errors.Is(err, ErrNotFound):
		w.WriteHeader(http.StatusNotFound)
	case err != nil:
		w.WriteHeader(http.StatusInternalServerError)
	default:
		m.log.Info("serve: name dropped", "by", authz.LoginFrom(r.Context()), "name", r.PathValue("name"))
		w.WriteHeader(http.StatusNoContent)
	}
}

// caCert serves the CA certificate. It is public material: users install it to trust serve names.
func (m *Module) caCert(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("Content-Disposition", `attachment; filename="headlamp-serve-ca.crt"`)
	_, _ = w.Write(m.ca.CertPEM())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(v)
}
