package serve

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// Headscale is what the module needs from headscale: the nodes (to map a caller and to build DNS answers) and the
// pre-auth keys of services.
type Headscale interface {
	ListNodes(ctx context.Context) ([]headscale.Node, error)
	ListUsers(ctx context.Context) ([]headscale.User, error)
	CreatePreAuthKey(ctx context.Context, userID uint64, tags []string, ttl time.Duration, ephemeral, reusable bool) (headscale.PreAuthKey, error)
	ExpirePreAuthKey(ctx context.Context, id uint64) error
}

// Module is the serve module (#117). It is on when SERVE_DB is set.
//
// A service is a name in the serve zone (exact, or a wildcard for an ingress controller) and one reusable ephemeral
// pre-auth key. Every proxy of the service joins the tailnet with that key. node-control knows the service of a node
// by the key it registered with, asks nothing of the node, and answers DNS for the zone itself: headscale sends the
// zone to node-control with dns.nameservers.split.
type Module struct {
	env core.Env
	hs  Headscale
	log *slog.Logger
	st  Store
	ca  *CA
	dns *servedns.Server
	tn  core.Tailnet
	now func() time.Time
	cfg config
	// selfName is this node's tailnet name, to find its address for the split line the page shows.
	selfName string
	mu       sync.Mutex // serialises service changes against headscale and the store
	last     struct {
		sync.Mutex
		at  time.Time
		err string
	}
}

type config struct {
	zone      string // normalised
	base      string // headscale base_domain, normalised
	user      string // headscale user that owns the service keys (id or name)
	nodeTag   string
	nsTag     string
	keyTTL    time.Duration
	certTTL   time.Duration
	refresh   time.Duration
	listenDNS bool
}

// Option configures the serve module.
type Option func(*Module)

// WithDNS shares a DNS server with the tailnet DNS module (#286): both answer on one listener.
func WithDNS(s *servedns.Server) Option { return func(m *Module) { m.dns = s } }

// New returns the serve module.
func New(env core.Env, hs Headscale, opts ...Option) core.Module {
	m := &Module{env: env, hs: hs, now: time.Now, dns: &servedns.Server{}}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (m *Module) Name() string { return "serve" }

const (
	caCertKey = "ca-cert"
	caKeyKey  = "ca-key"
	sealScope = "serve-ca"
)

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	nsRE    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
)

func duration(env core.Env, key string, def, lo, hi time.Duration) (time.Duration, error) {
	v := env.Getenv(key)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < lo || d > hi {
		return 0, fmt.Errorf("%s must be a duration from %s to %s", key, lo, hi)
	}
	return d, nil
}

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
	c := config{
		zone:      servedns.Normalize(m.env.Getenv("SERVE_ZONE")),
		base:      servedns.Normalize(m.env.Getenv("SERVE_BASE_DOMAIN")),
		user:      strings.TrimSpace(m.env.Getenv("SERVE_USER")),
		nodeTag:   m.env.Default("SERVE_NODE_TAG", "tag:serve"),
		nsTag:     m.env.Default("SERVE_NS_TAG_PREFIX", "tag:ns-"),
		listenDNS: m.env.Getenv("SERVE_DNS") != "off",
	}
	var err error
	if c.keyTTL, err = duration(m.env, "SERVE_KEY_TTL", 90*24*time.Hour, time.Hour, 365*24*time.Hour); err != nil {
		return err
	}
	if c.certTTL, err = duration(m.env, "SERVE_CERT_TTL", DefaultLeafTTL, time.Hour, MaxLeafTTL); err != nil {
		return err
	}
	if c.refresh, err = duration(m.env, "SERVE_DNS_REFRESH", 15*time.Second, 2*time.Second, 10*time.Minute); err != nil {
		return err
	}
	if err := checkZone(c.zone, c.base); err != nil {
		return fmt.Errorf("%s: %w", on, err)
	}
	if c.user == "" {
		return errors.New(on + " but SERVE_USER (the headscale user that owns the service keys) is not set")
	}
	for k, v := range map[string]string{"SERVE_NODE_TAG": c.nodeTag, "SERVE_NS_TAG_PREFIX": c.nsTag} {
		if !strings.HasPrefix(v, "tag:") || strings.ContainsAny(v, " ,") {
			return fmt.Errorf("%s must be a single tailnet tag (tag:...)", k)
		}
	}
	if p := authz.Default(); p != nil && !p.Empty() && !authz.AnyoneCan(authz.P(authz.FeatureServe, authz.LevelAdmin), authz.Allowed{}) {
		return errors.New(on + " but ADMIN_CONFIG grants serve.admin to nobody")
	}
	if r.Tailnet == nil {
		return errors.New(on + " but the core has no tailnet node to identify callers")
	}
	m.cfg, m.tn = c, r.Tailnet
	m.selfName = m.env.Default("TAILNET_HOSTNAME", "node-control")
	st, err := OpenSQLite(path)
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
	r.Admin.Handle("POST /serve/services", admin(json(m.createService)))
	r.Admin.Handle("DELETE /serve/services/{id}", admin(http.HandlerFunc(m.deleteService)))
	r.Admin.Handle("POST /serve/services/{id}/keys", admin(http.HandlerFunc(m.addKey)))
	r.Admin.Handle("DELETE /serve/services/{id}/keys/{key}", admin(http.HandlerFunc(m.retireKey)))
	r.Admin.HandleFunc("GET /serve/ca.crt", m.caCert)
	r.Admin.Handle("GET /serve", view(asset("text/html; charset=utf-8", pageHTML)))
	r.Admin.Handle("GET /serve/static/serve.js", view(asset("text/javascript; charset=utf-8", pageJS)))
	r.Admin.Handle("POST /serve/v1/cert", authz.TagGate(r.Tailnet, c.nodeTag, m.log)(json(m.cert)))
	return nil
}

// checkZone refuses a zone that clients would never ask node-control about: the zone must not be the MagicDNS
// base_domain or under it (clients answer those names themselves, as the spike on #117 showed).
func checkZone(zone, base string) error {
	switch {
	case zone == "":
		return errors.New("SERVE_ZONE (the DNS zone node-control answers, outside the MagicDNS base_domain) is not set")
	case base == "":
		return errors.New("SERVE_BASE_DOMAIN (the headscale MagicDNS base_domain) is not set")
	case zone == base || strings.HasSuffix(zone, "."+base):
		return fmt.Errorf("SERVE_ZONE %q is under the MagicDNS base_domain %q: tailnet clients answer those names themselves and never ask node-control", zone, base)
	case strings.HasSuffix(base, "."+zone):
		return fmt.Errorf("SERVE_ZONE %q contains the MagicDNS base_domain %q", zone, base)
	}
	for _, l := range strings.Split(zone, ".") {
		if !labelRE.MatchString(l) {
			return fmt.Errorf("SERVE_ZONE %q is not a valid DNS name", zone)
		}
	}
	if !strings.Contains(zone, ".") {
		return fmt.Errorf("SERVE_ZONE %q must have at least two labels", zone)
	}
	return nil
}

// Start opens the DNS listeners and keeps the answers in step with the online nodes of each service.
func (m *Module) Start(ctx context.Context) error {
	if m.st == nil {
		return nil
	}
	m.refreshDNS(ctx)
	go func() {
		t := time.NewTicker(m.cfg.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.refreshDNS(ctx)
			}
		}
	}()
	if !m.cfg.listenDNS {
		return nil
	}
	dl, ok := m.tn.(core.DNSListener)
	if !ok {
		m.log.Warn("serve: this tailnet cannot listen for DNS; names will not resolve")
		return nil
	}
	if err := m.dns.ListenOnce(ctx, dl, m.log); err != nil {
		return fmt.Errorf("serve: DNS listener: %w", err)
	}
	m.log.Info("serve: answering DNS", "zone", m.cfg.zone)
	return nil
}

// Stop closes the store after the servers are down.
func (m *Module) Stop(context.Context) error {
	if m.st != nil {
		return m.st.Close()
	}
	return nil
}

// --- DNS ---

// refreshDNS rebuilds the answers: each service name answers the tailnet addresses of the online nodes that joined
// with one of its keys. On a headscale error the old answers stay.
func (m *Module) refreshDNS(ctx context.Context) {
	svcs, err := m.st.Services()
	if err == nil {
		var nodes []headscale.Node
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		nodes, err = m.hs.ListNodes(ctx)
		cancel()
		if err == nil {
			m.dns.SetTable(servedns.NewTable(m.cfg.zone, records(svcs, nodes)))
		}
	}
	m.last.Lock()
	m.last.at = m.now()
	m.last.err = ""
	if err != nil {
		m.last.err = err.Error()
	}
	m.last.Unlock()
	if err != nil {
		m.log.Warn("serve: DNS answers not refreshed", "error", err)
	}
}

func records(svcs []Service, nodes []headscale.Node) []servedns.Record {
	byKey := map[uint64]int{}
	for i, s := range svcs {
		for _, k := range s.Keys {
			byKey[k.ID] = i
		}
	}
	addrs := make([][]netip.Addr, len(svcs))
	for _, n := range nodes {
		i, ok := byKey[n.PreAuthKeyID]
		if !ok || n.PreAuthKeyID == 0 || !n.Online {
			continue
		}
		for _, a := range n.IPAddresses {
			if ip, err := netip.ParseAddr(a); err == nil {
				addrs[i] = append(addrs[i], ip)
			}
		}
	}
	out := make([]servedns.Record, 0, len(svcs))
	for i, s := range svcs {
		slices.SortFunc(addrs[i], func(a, b netip.Addr) int { return a.Compare(b) })
		out = append(out, servedns.Record{Name: s.Name, Addrs: addrs[i]})
	}
	return out
}

// --- admin ---

type createReq struct {
	// Name is the full name, or a label under the zone. A wildcard starts with "*.".
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Note      string `json:"note,omitempty"`
}

type keyResp struct {
	Service Service `json:"service"`
	// AuthKey is shown once. Put it in the secret of the service's proxies.
	AuthKey string `json:"auth_key"`
}

// fullName checks a requested name and returns it in full: one or more labels under the zone, or "*." and at least
// one label under the zone.
func (m *Module) fullName(raw string) (string, error) {
	n := servedns.Normalize(raw)
	if !strings.HasSuffix(n, "."+m.cfg.zone) {
		n += "." + m.cfg.zone
	}
	if len(n) > 253 {
		return "", errors.New("name too long")
	}
	rest := strings.TrimSuffix(n, "."+m.cfg.zone)
	labels := strings.Split(rest, ".")
	for i, l := range labels {
		if l == "*" && i == 0 && len(labels) > 1 {
			continue
		}
		if !labelRE.MatchString(l) {
			return "", fmt.Errorf("%q is not a valid name in %s (a wildcard needs at least one label: *.apps)", raw, m.cfg.zone)
		}
	}
	return n, nil
}

func (m *Module) userID(ctx context.Context) (uint64, error) {
	if id, err := strconv.ParseUint(m.cfg.user, 10, 64); err == nil {
		return id, nil
	}
	users, err := m.hs.ListUsers(ctx)
	if err != nil {
		return 0, err
	}
	for _, u := range users {
		if u.Name == m.cfg.user {
			return u.ID, nil
		}
	}
	return 0, fmt.Errorf("headscale has no user %q (SERVE_USER)", m.cfg.user)
}

func (m *Module) mintKey(ctx context.Context, ns string) (headscale.PreAuthKey, error) {
	uid, err := m.userID(ctx)
	if err != nil {
		return headscale.PreAuthKey{}, err
	}
	return m.hs.CreatePreAuthKey(ctx, uid, []string{m.cfg.nodeTag, m.cfg.nsTag + ns}, m.cfg.keyTTL, true, true)
}

func newID() string {
	var b [5]byte
	_, _ = rand.Read(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

func (m *Module) createService(w http.ResponseWriter, r *http.Request) {
	var req createReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}
	name, err := m.fullName(req.Name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	ns := strings.ToLower(strings.TrimSpace(req.Namespace))
	if !nsRE.MatchString(ns) {
		http.Error(w, "namespace must be 1 to 32 lower-case letters, digits or dashes", http.StatusUnprocessableEntity)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	// A name may be held once: refuse before a key exists, so a refused request leaves nothing in headscale.
	all, err := m.st.Services()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	for _, s := range all {
		if s.Name == name {
			http.Error(w, "the name is already a service", http.StatusConflict)
			return
		}
	}
	key, err := m.mintKey(r.Context(), ns)
	if err != nil {
		m.log.Error("serve: minting a key", "error", err)
		http.Error(w, "headscale refused the key", http.StatusBadGateway)
		return
	}
	now := m.now().UTC()
	svc := Service{ID: newID(), Name: name, Namespace: ns, Note: req.Note, Created: now,
		Keys: []Key{{ID: key.ID, Expires: key.Expiration, Created: now}}}
	if err := m.st.CreateService(svc); err != nil {
		_ = m.hs.ExpirePreAuthKey(context.WithoutCancel(r.Context()), key.ID)
		code := http.StatusInternalServerError
		if errors.Is(err, ErrExists) {
			code = http.StatusConflict
		}
		http.Error(w, err.Error(), code)
		return
	}
	m.log.Info("serve: service created", "id", svc.ID, "name", name, "namespace", ns, "by", authz.LoginFrom(r.Context()))
	m.audit(r, "service.create", name+" in "+ns)
	m.refreshDNS(r.Context())
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, keyResp{Service: svc, AuthKey: key.Key})
}

// addKey starts a rotation: a second key for the service. Both keys work until the old one is retired.
func (m *Module) addKey(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	svc, err := m.st.Service(r.PathValue("id"))
	if err != nil {
		notFound(w, err)
		return
	}
	if len(svc.Keys) >= 2 {
		http.Error(w, "retire the old key first: a service holds at most two keys", http.StatusConflict)
		return
	}
	key, err := m.mintKey(r.Context(), svc.Namespace)
	if err != nil {
		m.log.Error("serve: minting a key", "error", err)
		http.Error(w, "headscale refused the key", http.StatusBadGateway)
		return
	}
	if err := m.st.AddKey(svc.ID, Key{ID: key.ID, Expires: key.Expiration, Created: m.now().UTC()}); err != nil {
		_ = m.hs.ExpirePreAuthKey(context.WithoutCancel(r.Context()), key.ID)
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	svc, _ = m.st.Service(svc.ID)
	m.log.Info("serve: key added", "id", svc.ID, "key", key.ID, "by", authz.LoginFrom(r.Context()))
	m.audit(r, "key.add", fmt.Sprintf("%s: key %d", svc.Name, key.ID))
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, keyResp{Service: svc, AuthKey: key.Key})
}

// retireKey expires a key in headscale and removes it from the service. Nodes already on the tailnet keep their
// identity; the name stops answering their address at the next refresh.
func (m *Module) retireKey(w http.ResponseWriter, r *http.Request) {
	keyID, err := strconv.ParseUint(r.PathValue("key"), 10, 64)
	if err != nil {
		http.Error(w, "bad key id", http.StatusBadRequest)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.hs.ExpirePreAuthKey(r.Context(), keyID); err != nil {
		m.log.Warn("serve: expiring a key", "key", keyID, "error", err)
	}
	svc, _ := m.st.Service(r.PathValue("id"))
	if err := m.st.RetireKey(r.PathValue("id"), keyID); err != nil {
		notFound(w, err)
		return
	}
	m.audit(r, "key.retire", fmt.Sprintf("%s: key %d", svc.Name, keyID))
	m.log.Info("serve: key retired", "id", r.PathValue("id"), "key", keyID, "by", authz.LoginFrom(r.Context()))
	m.refreshDNS(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

func (m *Module) deleteService(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	svc, err := m.st.Service(r.PathValue("id"))
	if err != nil {
		notFound(w, err)
		return
	}
	for _, k := range svc.Keys {
		if err := m.hs.ExpirePreAuthKey(r.Context(), k.ID); err != nil {
			m.log.Warn("serve: expiring a key", "key", k.ID, "error", err)
		}
	}
	if err := m.st.DeleteService(svc.ID); err != nil {
		notFound(w, err)
		return
	}
	m.log.Info("serve: service deleted", "id", svc.ID, "name", svc.Name, "by", authz.LoginFrom(r.Context()))
	m.audit(r, "service.delete", svc.Name)
	m.refreshDNS(r.Context())
	w.WriteHeader(http.StatusNoContent)
}

type nodeView struct {
	Name   string   `json:"name"`
	Online bool     `json:"online"`
	IPs    []string `json:"ips"`
	KeyID  uint64   `json:"key_id"`
}

type serviceView struct {
	Service
	Nodes []nodeView `json:"nodes"`
}

type stateResp struct {
	Zone          string        `json:"zone"`
	CAFingerprint string        `json:"ca_fingerprint"`
	Services      []serviceView `json:"services"`
	DNSRefreshed  time.Time     `json:"dns_refreshed,omitzero"`
	DNSError      string        `json:"dns_error,omitempty"`
	// DNSServer is the tailnet address of node-control that headscale's split must name (#129), when headscale
	// reports it: the node with the node-control tag, else the given name of this node.
	DNSServer string `json:"dns_server,omitempty"`
	// CanAdmin says whether the caller may change services; the page shows the admin controls only then.
	CanAdmin bool `json:"can_admin"`
	// KeyTTL is how long a new service key lives, for the page to say so before an admin mints one.
	KeyTTLHours int `json:"key_ttl_hours"`
}

func (m *Module) state(w http.ResponseWriter, r *http.Request) {
	svcs, err := m.st.Services()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	nodes, nerr := m.hs.ListNodes(r.Context())
	out := stateResp{Zone: m.cfg.zone, CAFingerprint: m.ca.Fingerprint(), Services: []serviceView{},
		CanAdmin:    authz.Can(authz.LoginFrom(r.Context()), authz.P(authz.FeatureServe, authz.LevelAdmin), authz.Allowed{}),
		KeyTTLHours: int(m.cfg.keyTTL / time.Hour), DNSServer: selfAddr(nodes, m.selfName)}
	for _, s := range svcs {
		v := serviceView{Service: s, Nodes: []nodeView{}}
		for _, n := range nodes {
			if slices.ContainsFunc(s.Keys, func(k Key) bool { return k.ID == n.PreAuthKeyID }) && n.PreAuthKeyID != 0 {
				v.Nodes = append(v.Nodes, nodeView{Name: n.GivenName, Online: n.Online, IPs: n.IPAddresses, KeyID: n.PreAuthKeyID})
			}
		}
		out.Services = append(out.Services, v)
	}
	m.last.Lock()
	out.DNSRefreshed, out.DNSError = m.last.at, m.last.err
	m.last.Unlock()
	if nerr != nil && out.DNSError == "" {
		out.DNSError = nerr.Error()
	}
	writeJSON(w, out)
}

func (m *Module) caCert(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/x-pem-file")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(m.ca.CertPEM())
}

// --- node side ---

type certReq struct {
	CSR string `json:"csr"`
}

type certResp struct {
	Name     string    `json:"name"`
	CertPEM  string    `json:"cert_pem"`
	ChainPEM string    `json:"chain_pem"`
	NotAfter time.Time `json:"not_after"`
}

// cert signs the caller's CSR for the name of its service. The service comes from the pre-auth key the calling node
// joined with, which headscale reports; nothing the node says is trusted, and the names in the CSR are ignored.
func (m *Module) cert(w http.ResponseWriter, r *http.Request) {
	var req certReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil || req.CSR == "" {
		http.Error(w, "invalid JSON or no csr", http.StatusBadRequest)
		return
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	ip, perr := netip.ParseAddr(host)
	if err != nil || perr != nil {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	nodes, err := m.hs.ListNodes(r.Context())
	if err != nil {
		http.Error(w, "headscale unavailable", http.StatusServiceUnavailable)
		return
	}
	var keyID uint64
	for _, n := range nodes {
		if slices.Contains(n.IPAddresses, ip.Unmap().String()) {
			keyID = n.PreAuthKeyID
			break
		}
	}
	svc, ok, err := m.serviceForKey(keyID)
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "this node did not join with the key of a service", http.StatusForbidden)
		return
	}
	certPEM, notAfter, err := m.ca.SignCSR([]byte(req.CSR), []string{svc.Name}, m.cfg.certTTL)
	if err != nil {
		http.Error(w, "bad CSR: "+err.Error(), http.StatusUnprocessableEntity)
		return
	}
	writeJSON(w, certResp{Name: svc.Name, CertPEM: string(certPEM), ChainPEM: string(m.ca.ChainPEM()), NotAfter: notAfter})
}

func (m *Module) serviceForKey(keyID uint64) (Service, bool, error) {
	if keyID == 0 {
		return Service{}, false, nil
	}
	svcs, err := m.st.Services()
	if err != nil {
		return Service{}, false, err
	}
	for _, s := range svcs {
		if slices.ContainsFunc(s.Keys, func(k Key) bool { return k.ID == keyID }) {
			return s, true, nil
		}
	}
	return Service{}, false, nil
}

// --- CA ---

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
	ca, cert, key, err := NewSelfSignedCA("headlamp serve CA ("+m.cfg.zone+")", m.now())
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

func notFound(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	http.Error(w, "store error", http.StatusInternalServerError)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
