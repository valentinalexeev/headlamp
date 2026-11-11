// Package tailnetdns makes node-control the resolver of the tailnet names (#286). It answers, on the DNS listener
// it shares with serve, four kinds of names, in this order:
//
//  1. records managed here (API below): a name points at fixed tailnet addresses, one node, or the online nodes
//     with a tag;
//  2. the headscale dns.extra_records, read from node-control's own netmap;
//  3. the serve zone (the serve module fills it);
//  4. the MagicDNS names of the nodes (<given name>.<base_domain>), from the headscale API.
//
// Bridges get the server address and the list of these names with their credentials: their xray resolver then asks
// node-control for exactly these names and gets tailnet addresses, so the tailnet IP rule of a granted client
// matches. Tailnet devices ask node-control for the managed names through headscale split DNS.
//
// Answers hold only tailnet addresses (100.64.0.0/10, fd7a:115c:a1e0::/48). Subnet routes are out of scope.
package tailnetdns

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/statestore"
)

// Headscale is what the module needs from headscale: the nodes, for MagicDNS names and node or tag targets.
type Headscale interface {
	ListNodes(ctx context.Context) ([]headscale.Node, error)
}

const (
	maxRecords   = 256
	maxAddresses = 8
	maxNote      = 200
	// MaxBridgeNames is the most names a bridge gets; the agent refuses a longer list.
	MaxBridgeNames = 1024
)

var (
	labelRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tagRE   = regexp.MustCompile(`^tag:[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	// tailnetRanges are the headscale address ranges: the only addresses an answer may hold.
	tailnetRanges = []netip.Prefix{netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("fd7a:115c:a1e0::/48")}
)

// InTailnet says whether a is a tailnet address.
func InTailnet(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range tailnetRanges {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Module is the tailnet DNS module. It is on when TAILNET_DNS_DB (SQLite) is set, or TAILNET_DNS_REDIS=1 (the shared
// Redis of REDIS_URL, no volume needed).
type Module struct {
	env core.Env
	hs  Headscale
	dns *servedns.Server
	log *slog.Logger
	st  Store
	tn  core.Tailnet
	now func() time.Time

	zone, baseEnv string // SERVE_ZONE and SERVE_BASE_DOMAIN, normalised
	refresh       time.Duration
	listen        bool
	// extra publishes the managed records to headscale's extra_records_path file; nil: off.
	extra *extraFile

	mu  sync.Mutex // serialises record changes
	cur struct {
		sync.Mutex
		suffix    string
		self4     netip.Addr
		listening bool
		extra     []string // the extra_records names answered
		at        time.Time
		err       string
	}
}

// New returns the module. dns is the server shared with serve.
func New(env core.Env, hs Headscale, dns *servedns.Server) *Module {
	return &Module{env: env, hs: hs, dns: dns, now: time.Now}
}

func (m *Module) Name() string { return "tailnetdns" }

// Enabled says whether the module is active (after Register).
func (m *Module) Enabled() bool { return m.st != nil }

const on = "tailnet DNS enabled (TAILNET_DNS_DB or TAILNET_DNS_REDIS=1 set)"

// redisPrefix is the key prefix in the shared Redis: TAILNET_DNS_REDIS_KEY, else "<app>:dns".
func redisPrefix(env core.Env) string {
	if k := env.Getenv("TAILNET_DNS_REDIS_KEY"); k != "" {
		return k
	}
	app := env.Getenv("FLY_APP_NAME")
	if app == "" {
		app = env.Default("TAILNET_HOSTNAME", "node-control")
	}
	return app + ":dns"
}

// openStore opens the configured backend; nil, nil: the module is off. A new feature is off until the operator turns
// it on, so REDIS_URL alone does not enable it.
func openStore(env core.Env) (Store, error) {
	path, useRedis := env.Getenv("TAILNET_DNS_DB"), env.Getenv("TAILNET_DNS_REDIS")
	switch {
	case useRedis != "" && useRedis != "1" && useRedis != "0":
		return nil, errors.New("TAILNET_DNS_REDIS must be 1 or 0")
	case path != "" && useRedis == "1":
		return nil, errors.New("set either TAILNET_DNS_DB or TAILNET_DNS_REDIS=1, not both")
	case path != "":
		return OpenSQLite(path)
	case useRedis == "1":
		url := env.Getenv("REDIS_URL")
		if url == "" {
			return nil, errors.New("TAILNET_DNS_REDIS=1 needs REDIS_URL")
		}
		rdb, _, err := statestore.OpenClient(url)
		if err != nil {
			return nil, fmt.Errorf("tailnet DNS store: %w", err)
		}
		s, err := NewRedisStore(rdb, redisPrefix(env))
		if err != nil {
			rdb.Close()
		}
		return s, err
	}
	return nil, nil
}

func (m *Module) Register(r *core.Routes) error {
	m.log = r.Log
	if m.log == nil {
		m.log = slog.Default()
	}
	if v := m.env.Getenv("TAILNET_DNS_REDIS"); m.env.Getenv("TAILNET_DNS_DB") == "" && (v == "" || v == "0") {
		return nil
	}
	m.zone = servedns.Normalize(m.env.Getenv("SERVE_ZONE"))
	m.baseEnv = servedns.Normalize(m.env.Getenv("SERVE_BASE_DOMAIN"))
	m.listen = m.env.Getenv("TAILNET_DNS_LISTEN") != "off"
	if p := m.env.Getenv("TAILNET_DNS_EXTRA_RECORDS_FILE"); p != "" {
		if err := checkExtraPath(p); err != nil {
			return err
		}
		m.extra = &extraFile{path: p}
	}
	m.refresh = 15 * time.Second
	if v := m.env.Getenv("TAILNET_DNS_REFRESH"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < 2*time.Second || d > 10*time.Minute {
			return errors.New("TAILNET_DNS_REFRESH must be a duration from 2s to 10m")
		}
		m.refresh = d
	}
	if p := authz.Default(); p != nil && !p.Empty() &&
		!authz.AnyoneCan(authz.P(authz.FeatureDNS, authz.LevelAdmin), authz.Allowed{}) {
		return errors.New(on + " but ADMIN_CONFIG grants dns.admin to nobody")
	}
	if r.Tailnet == nil {
		return errors.New(on + " but the core has no tailnet node to identify callers")
	}
	if m.dns == nil {
		m.dns = &servedns.Server{}
	}
	m.tn = r.Tailnet
	st, err := openStore(m.env)
	if err != nil || st == nil {
		return err
	}
	m.st = st
	view := authz.Gate(r.Tailnet, authz.P(authz.FeatureDNS, authz.LevelView), authz.Allowed{}, m.log)
	admin := authz.Gate(r.Tailnet, authz.P(authz.FeatureDNS, authz.LevelAdmin), authz.Allowed{}, m.log)
	js := func(h http.HandlerFunc) http.Handler { return authz.RequireJSON(h) }
	r.Admin.Handle("GET /dns/state", view(http.HandlerFunc(m.state)))
	r.Admin.Handle("POST /dns/records", admin(js(m.create)))
	r.Admin.Handle("PUT /dns/records/{name}", admin(js(m.replace)))
	r.Admin.Handle("DELETE /dns/records/{name}", admin(http.HandlerFunc(m.remove)))
	return nil
}

// Start fills the tables, keeps them in step, and opens the shared listener.
func (m *Module) Start(ctx context.Context) error {
	if m.st == nil {
		return nil
	}
	m.Refresh(ctx)
	go func() {
		t := time.NewTicker(m.refresh)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				m.Refresh(ctx)
			}
		}
	}()
	if !m.listen {
		return nil
	}
	dl, ok := m.tn.(core.DNSListener)
	if !ok {
		m.log.Warn("tailnet DNS: this tailnet cannot listen for DNS; names will not resolve")
		return nil
	}
	if err := m.dns.ListenOnce(ctx, dl, m.log); err != nil {
		return fmt.Errorf("tailnet DNS: listener: %w", err)
	}
	m.cur.Lock()
	m.cur.listening = true
	m.cur.Unlock()
	m.log.Info("tailnet DNS: answering")
	return nil
}

// Stop closes the store.
func (m *Module) Stop(context.Context) error {
	if m.st != nil {
		return m.st.Close()
	}
	return nil
}

// --- tables ---

// Refresh rebuilds the managed, extra_records and MagicDNS tables. A source that fails keeps its old table.
func (m *Module) Refresh(ctx context.Context) {
	var errs []string
	var info core.TailnetDNS
	if dr, ok := m.tn.(core.DNSReader); ok {
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		i, err := dr.TailnetDNS(c)
		cancel()
		if err != nil {
			errs = append(errs, "netmap: "+err.Error())
		} else {
			info = i
			var recs []servedns.Record
			var names []string
			for _, e := range i.Extra {
				if !InTailnet(e.Addr) || validName(e.Name) != nil {
					continue
				}
				recs = append(recs, servedns.Record{Name: e.Name, Addrs: []netip.Addr{e.Addr.Unmap()}})
				names = append(names, e.Name)
			}
			slices.Sort(names)
			m.dns.SetSlot(servedns.SlotExtra, servedns.NewNames(recs))
			m.cur.Lock()
			m.cur.extra = slices.Compact(names)
			m.cur.Unlock()
		}
	}
	suffix := info.Suffix
	if suffix == "" {
		suffix = m.baseEnv
	}
	c, cancel := context.WithTimeout(ctx, 10*time.Second)
	nodes, err := m.hs.ListNodes(c)
	cancel()
	if err != nil {
		errs = append(errs, "headscale: "+err.Error())
	} else {
		if suffix != "" {
			m.dns.SetSlot(servedns.SlotMagic, servedns.NewTable(suffix, magicRecords(nodes, suffix)))
		}
		recs, rerr := m.st.Records()
		if rerr != nil {
			errs = append(errs, "store: "+rerr.Error())
		} else {
			t := servedns.NewNames(answers(recs, nodes))
			m.dns.SetSlot(servedns.SlotRecords, t)
			// Only after a full rebuild: a failed source never empties the file.
			if m.extra != nil {
				if werr := m.extra.write(extraContent(t), m.now()); werr != nil {
					errs = append(errs, werr.Error())
				}
			}
		}
	}
	m.cur.Lock()
	if suffix != "" {
		m.cur.suffix = suffix
	}
	if info.Self4.IsValid() {
		m.cur.self4 = info.Self4
	}
	m.cur.at = m.now()
	m.cur.err = strings.Join(errs, "; ")
	m.cur.Unlock()
	if len(errs) > 0 {
		m.log.Warn("tailnet DNS: answers not fully refreshed", "error", m.cur.err)
	}
}

func nodeAddrs(n headscale.Node) []netip.Addr {
	var out []netip.Addr
	for _, a := range n.IPAddresses {
		if ip, err := netip.ParseAddr(a); err == nil && InTailnet(ip) {
			out = append(out, ip.Unmap())
		}
	}
	return out
}

// magicRecords gives <given name>.<suffix> for every node.
func magicRecords(nodes []headscale.Node, suffix string) []servedns.Record {
	out := make([]servedns.Record, 0, len(nodes))
	for _, n := range nodes {
		name := strings.ToLower(n.GivenName)
		if name == "" || !labelRE.MatchString(name) {
			continue
		}
		out = append(out, servedns.Record{Name: name + "." + suffix, Addrs: nodeAddrs(n)})
	}
	return out
}

// answers resolves the targets of the managed records. A node target gives the node's addresses; a tag target the
// addresses of the online nodes with the tag. A target with no match is a name without addresses (NODATA).
func answers(recs []Record, nodes []headscale.Node) []servedns.Record {
	out := make([]servedns.Record, 0, len(recs))
	for _, r := range recs {
		var addrs []netip.Addr
		switch {
		case r.Node != "":
			for _, n := range nodes {
				if strings.EqualFold(n.GivenName, r.Node) {
					addrs = append(addrs, nodeAddrs(n)...)
				}
			}
		case r.Tag != "":
			for _, n := range nodes {
				if n.Online && slices.Contains(n.Tags, r.Tag) {
					addrs = append(addrs, nodeAddrs(n)...)
				}
			}
		default:
			for _, a := range r.Addresses {
				if ip, err := netip.ParseAddr(a); err == nil && InTailnet(ip) {
					addrs = append(addrs, ip.Unmap())
				}
			}
		}
		slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
		out = append(out, servedns.Record{Name: r.Name, Addrs: slices.Compact(addrs)})
	}
	return out
}

// --- bridges ---

// Bridge is what a bridge needs to resolve the tailnet names (#286): the server and the names, as xray domain
// matchers. node-control sends it with the credentials.
type Bridge struct {
	Server  string   `json:"server"`
	Domains []string `json:"domains"`
}

// BridgeDNS gives the current Bridge. ok is false while the module is off, not listening, or does not know its own
// address yet: the bridge then gets no tailnet DNS (as before #286).
func (m *Module) BridgeDNS() (Bridge, bool) {
	if m == nil || m.st == nil {
		return Bridge{}, false
	}
	m.cur.Lock()
	self, listening, suffix, extra := m.cur.self4, m.cur.listening, m.cur.suffix, slices.Clone(m.cur.extra)
	m.cur.Unlock()
	if !listening || !self.IsValid() {
		return Bridge{}, false
	}
	var d []string
	if t := m.dns.Slot(servedns.SlotRecords); t != nil {
		for _, n := range t.Names() {
			d = append(d, "full:"+n)
		}
	}
	for _, n := range extra {
		d = append(d, "full:"+n)
	}
	for _, z := range []string{m.zone, suffix} {
		if z != "" {
			d = append(d, "domain:"+z)
		}
	}
	slices.Sort(d)
	d = slices.Compact(d)
	if len(d) == 0 {
		return Bridge{}, false
	}
	if len(d) > MaxBridgeNames {
		m.log.Warn("tailnet DNS: too many names for a bridge; the list is cut", "names", len(d))
		d = d[:MaxBridgeNames]
	}
	return Bridge{Server: "tcp://" + self.String() + ":53", Domains: d}, true
}

// --- validation ---

func validName(n string) error {
	if len(n) > 253 || !strings.Contains(n, ".") {
		return errors.New("name must be a DNS name with at least two labels")
	}
	for _, l := range strings.Split(n, ".") {
		if !labelRE.MatchString(l) {
			return errors.New("name must be lower-case letters, digits and dashes, in labels separated by dots")
		}
	}
	return nil
}

func under(n, zone string) bool { return zone != "" && (n == zone || strings.HasSuffix(n, "."+zone)) }

// check normalises and validates a record. The error is for the caller and quotes nothing secret.
func (m *Module) check(r *Record) error {
	r.Name = servedns.Normalize(r.Name)
	if err := validName(r.Name); err != nil {
		return err
	}
	m.cur.Lock()
	suffix := m.cur.suffix
	m.cur.Unlock()
	if suffix == "" {
		suffix = m.baseEnv
	}
	if under(r.Name, suffix) {
		return errors.New("name is under the MagicDNS base_domain: tailnet devices answer those names themselves")
	}
	if under(r.Name, m.zone) {
		return errors.New("name is in the serve zone: use a serve service")
	}
	set := 0
	if len(r.Addresses) > 0 {
		set++
	}
	if r.Node != "" {
		set++
	}
	if r.Tag != "" {
		set++
	}
	if set != 1 {
		return errors.New("give exactly one of addresses, node or tag")
	}
	if len(r.Addresses) > maxAddresses {
		return fmt.Errorf("at most %d addresses", maxAddresses)
	}
	for i, a := range r.Addresses {
		ip, err := netip.ParseAddr(strings.TrimSpace(a))
		if err != nil || !InTailnet(ip) {
			return errors.New("an address is not a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48)")
		}
		r.Addresses[i] = ip.Unmap().String()
	}
	slices.Sort(r.Addresses)
	r.Addresses = slices.Compact(r.Addresses)
	r.Node = strings.ToLower(strings.TrimSpace(r.Node))
	if r.Node != "" && !labelRE.MatchString(r.Node) {
		return errors.New("node must be the given name of a headscale node")
	}
	r.Tag = strings.TrimSpace(r.Tag)
	if r.Tag != "" && !tagRE.MatchString(r.Tag) {
		return errors.New("tag must be tag:<name>")
	}
	if len(r.Note) > maxNote {
		return fmt.Errorf("note is longer than %d characters", maxNote)
	}
	return nil
}

// --- HTTP ---

type recordReq struct {
	Name      string   `json:"name"`
	Addresses []string `json:"addresses,omitempty"`
	Node      string   `json:"node,omitempty"`
	Tag       string   `json:"tag,omitempty"`
	Note      string   `json:"note,omitempty"`
}

func decode(w http.ResponseWriter, r *http.Request) (recordReq, bool) {
	var req recordReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (m *Module) audit(r *http.Request, action, detail string) {
	if err := m.st.Audit(m.now().UTC(), authz.LoginFrom(r.Context()), action, detail); err != nil {
		m.log.Warn("tailnet DNS: audit not written", "action", action, "error", err)
	}
}

func describe(rec Record) string {
	switch {
	case rec.Node != "":
		return rec.Name + " -> node " + rec.Node
	case rec.Tag != "":
		return rec.Name + " -> " + rec.Tag
	}
	return rec.Name + " -> " + strings.Join(rec.Addresses, ",")
}

func (m *Module) create(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	rec := Record{Name: req.Name, Addresses: req.Addresses, Node: req.Node, Tag: req.Tag, Note: req.Note}
	m.write(w, r, rec, true)
}

func (m *Module) replace(w http.ResponseWriter, r *http.Request) {
	req, ok := decode(w, r)
	if !ok {
		return
	}
	name := r.PathValue("name")
	if req.Name != "" && servedns.Normalize(req.Name) != servedns.Normalize(name) {
		http.Error(w, "the name in the body differs from the path; a record has no rename", http.StatusUnprocessableEntity)
		return
	}
	rec := Record{Name: name, Addresses: req.Addresses, Node: req.Node, Tag: req.Tag, Note: req.Note}
	m.write(w, r, rec, false)
}

func (m *Module) write(w http.ResponseWriter, r *http.Request, rec Record, create bool) {
	if err := m.check(&rec); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	all, err := m.st.Records()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	now := m.now().UTC().Truncate(time.Second)
	rec.Updated = now
	var action string
	if create {
		if len(all) >= maxRecords {
			http.Error(w, fmt.Sprintf("at most %d records", maxRecords), http.StatusConflict)
			return
		}
		rec.Created = now
		err, action = m.st.Create(rec), "record.create"
	} else {
		for _, o := range all {
			if o.Name == rec.Name {
				rec.Created = o.Created
			}
		}
		err, action = m.st.Replace(rec), "record.replace"
	}
	switch {
	case errors.Is(err, ErrExists):
		http.Error(w, "a record with this name exists", http.StatusConflict)
		return
	case errors.Is(err, ErrNotFound):
		http.Error(w, "no such record", http.StatusNotFound)
		return
	case err != nil:
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	m.audit(r, action, describe(rec))
	m.Refresh(context.WithoutCancel(r.Context()))
	code := http.StatusOK
	if create {
		code = http.StatusCreated
	}
	writeJSON(w, code, rec)
}

func (m *Module) remove(w http.ResponseWriter, r *http.Request) {
	name := servedns.Normalize(r.PathValue("name"))
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.st.Delete(name); err != nil {
		if errors.Is(err, ErrNotFound) {
			http.Error(w, "no such record", http.StatusNotFound)
			return
		}
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	m.audit(r, "record.delete", name)
	m.Refresh(context.WithoutCancel(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

type recordView struct {
	Record
	// Answer is what node-control answers now.
	Answer []string `json:"answer"`
}

type stateResp struct {
	Server     string       `json:"server"`      // tcp://<node-control>:53, "" while unknown
	Listening  bool         `json:"listening"`   // the DNS listener is open
	BaseDomain string       `json:"base_domain"` // the MagicDNS suffix
	ServeZone  string       `json:"serve_zone,omitempty"`
	Records    []recordView `json:"records"`
	Extra      []string     `json:"extra_records"` // the extra_records names answered (from the netmap)
	// Split is the dns.nameservers.split block for headscale: tailnet devices then ask node-control for the managed
	// names.
	Split  map[string][]string `json:"split"`
	Bridge *Bridge             `json:"bridge,omitempty"`
	// ExtraFile is the headscale extra_records_path file that node-control writes; empty: not configured.
	ExtraFile        string     `json:"extra_records_file,omitempty"`
	ExtraFileWritten *time.Time `json:"extra_records_file_written,omitempty"`
	ExtraFileError   string     `json:"extra_records_file_error,omitempty"`
	CanAdmin         bool       `json:"can_admin"`
	Refreshed        time.Time  `json:"refreshed"`
	Error            string     `json:"error,omitempty"`
}

func (m *Module) state(w http.ResponseWriter, r *http.Request) {
	recs, err := m.st.Records()
	if err != nil {
		http.Error(w, "store error", http.StatusInternalServerError)
		return
	}
	m.cur.Lock()
	out := stateResp{Listening: m.cur.listening, BaseDomain: m.cur.suffix, ServeZone: m.zone,
		Extra: append([]string{}, m.cur.extra...), Refreshed: m.cur.at, Error: m.cur.err,
		Records: []recordView{}, Split: map[string][]string{}}
	self := m.cur.self4
	m.cur.Unlock()
	if self.IsValid() {
		out.Server = "tcp://" + self.String() + ":53"
	}
	out.CanAdmin = authz.Can(authz.LoginFrom(r.Context()), authz.P(authz.FeatureDNS, authz.LevelAdmin), authz.Allowed{})
	if m.extra != nil {
		out.ExtraFile = m.extra.path
		at, ferr := m.extra.status()
		if !at.IsZero() {
			out.ExtraFileWritten = &at
		}
		out.ExtraFileError = ferr
	}
	t := m.dns.Slot(servedns.SlotRecords)
	// The managed records come back from headscale as extra records when node-control writes the file: list them
	// once, under records.
	managed := map[string]bool{}
	for _, rec := range recs {
		managed[rec.Name] = true
	}
	out.Extra = slices.DeleteFunc(out.Extra, func(n string) bool { return managed[n] })
	for _, rec := range recs {
		v := recordView{Record: rec, Answer: []string{}}
		if t != nil {
			if addrs, found, _ := t.Lookup(rec.Name); found {
				for _, a := range addrs {
					v.Answer = append(v.Answer, a.String())
				}
			}
		}
		out.Records = append(out.Records, v)
		// With the file, headscale already sends the records to devices: no split line is needed.
		if self.IsValid() && m.extra == nil {
			out.Split[rec.Name] = []string{self.String()}
		}
	}
	if b, ok := m.BridgeDNS(); ok {
		out.Bridge = &b
	}
	writeJSON(w, http.StatusOK, out)
}

// --- console integration ---

// HasActivity reports whether the module keeps an activity log.
func (m *Module) HasActivity() bool { return m.st != nil }

// Activity lists record changes for the console's activity page.
func (m *Module) Activity(_ context.Context, login string, limit int) ([]core.ActivityEntry, error) {
	if m.st == nil {
		return nil, nil
	}
	if !authz.Can(login, authz.P(authz.FeatureDNS, authz.LevelView), authz.Allowed{}) {
		return nil, core.ErrNotVisible
	}
	es, err := m.st.AuditLog(limit)
	if err != nil {
		return nil, fmt.Errorf("tailnet DNS store: %w", err)
	}
	out := make([]core.ActivityEntry, 0, len(es))
	for _, e := range es {
		out = append(out, core.ActivityEntry{At: e.At, Actor: e.Actor, Source: "DNS", Action: e.Action, Detail: e.Detail})
	}
	return out, nil
}
