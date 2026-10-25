package enroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"example.com/node-control/internal/headscale"
)

// The ingress registry holds the CURRENT set of ingresses: the ones listed in enroll.json (approved
// by definition) and the ones discovered from headscale. A discovered ingress is a node tagged
// tag:ingress whose agent publishes its public parameters at GET /v1/info. It is never used (not in
// a profile, not synced) until the operator approves it, and the approval pins a fingerprint of the
// published parameters: when they change, the ingress needs approval again.

const (
	ingressTag  = "tag:ingress"
	maxInfoBody = 64 << 10
)

var (
	ingressAgentPort = "9000"
	infoTimeout      = 5 * time.Second
	infoTTL          = time.Minute // at most one /v1/info fetch per node per interval
)

// Approval states of an ingress row.
const (
	stConfigured  = "configured"
	stPending     = "pending"     // published parameters, never approved
	stApproved    = "approved"    // the stored fingerprint equals the current one
	stChanged     = "changed"     // approved earlier, the parameters differ now
	stUnreachable = "unreachable" // the parameters cannot be read (and none are known)
	stNoToken     = "no token"    // no token variable for it: it is not even asked
	stInvalid     = "invalid"     // the published parameters fail validation
)

// ApprovalError is a refused approval change; Status is the HTTP status to answer with.
type ApprovalError struct {
	Status int
	Msg    string
}

func (e *ApprovalError) Error() string { return e.Msg }

// RegistryConfig configures NewRegistry.
type RegistryConfig struct {
	Cfg         IngressConfig        // the configured ingresses, with the server key set
	Src         headscale.NodeSource // nil: no discovery, no online flags, no address lookup
	Getenv      func(string) string  // token variables
	HC          *http.Client         // agent and /v1/info requests; nil means a plain client
	Now         func() time.Time
	AgentURL    string // AGENT_URL, the agent of the first configured ingress without an agent_url
	LinkHostSet bool   // enroll.json names link_host: discovery needs the shared host
	// Agents are ready-made agents by ingress name (tests and the single-agent constructor); an
	// ingress listed here needs no URL and no token variable.
	Agents map[string]Agent
}

// IngressView is one row of the ingress list. Unapproved ingresses show only a short prefix of
// their Reality public key and short id.
type IngressView struct {
	Name          string        `json:"name"`
	Host          string        `json:"host"`
	Source        string        `json:"source"`   // configured | discovered
	Approval      string        `json:"approval"` // configured, or one of the discovered states
	Active        bool          `json:"active"`   // used for profiles and sync
	Approved      bool          `json:"approved"` // an approval is stored for it (it can be removed)
	Fingerprint   string        `json:"fingerprint,omitempty"`
	PublicKeyPref string        `json:"public_key_prefix,omitempty"`
	ShortIDPref   string        `json:"short_id_prefix,omitempty"`
	Online        *bool         `json:"online"`
	Sync          *SyncSnapshot `json:"sync"`
	Detail        string        `json:"detail,omitempty"`
}

// DiscoveryStatus says whether discovery runs and, when it does not, why.
type DiscoveryStatus struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// Current is one consistent view of the ingress set.
type Current struct {
	Cfg       IngressConfig // configured + approved discovered ingresses, for profiles and sync
	Targets   []SyncTarget  // the same ingresses as sync targets, in the same order
	Views     []IngressView // everything, configured first, then discovered by name
	Pending   []string      // discovered ingresses that are not approved
	Discovery DiscoveryStatus
}

// probe is what is known about one discovered node.
type probe struct {
	at     time.Time
	info   *infoResult // the last parameters read successfully
	state  string      // "" when the last attempt succeeded
	detail string
}

type infoResult struct {
	ing Ingress
	fp  string
}

// Registry is the dynamic ingress set. It is safe for concurrent use.
type Registry struct {
	mu           sync.Mutex
	store        *Store
	base         IngressConfig
	configured   []SyncTarget
	src          headscale.NodeSource
	getenv       func(string) string
	hc           *http.Client
	now          func() time.Time
	discoveryOff string

	nodes  []headscale.Node
	probes map[string]*probe
	dstate map[string]*SyncState
	dagent map[string]Agent
}

// NewRegistry validates the configured ingresses and builds their agents. Errors name an ingress
// and a variable, never a value.
func NewRegistry(rc RegistryConfig) (*Registry, error) {
	r := &Registry{base: rc.Cfg, src: rc.Src, getenv: rc.Getenv, hc: rc.HC, now: rc.Now,
		probes: map[string]*probe{}, dstate: map[string]*SyncState{}, dagent: map[string]Agent{}}
	if r.getenv == nil {
		r.getenv = func(string) string { return "" }
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.hc == nil {
		r.hc = newHTTPClient(nil)
	}
	for i, ing := range rc.Cfg.Ingresses {
		agent, ok := rc.Agents[ing.Name]
		if !ok {
			token, err := r.configuredToken(i, ing)
			if err != nil {
				return nil, err
			}
			url := ing.AgentURL
			if url == "" && i == 0 {
				url = rc.AgentURL
			}
			switch {
			case url != "":
				agent = NewAgent(url, token, r.hc)
			case r.src != nil:
				agent = NewAgentFunc(r.resolver(ing.Name), token, r.hc)
			default:
				return nil, fmt.Errorf("ingress %s: no agent_url and no headscale source to find its agent", ing.Name)
			}
		}
		r.configured = append(r.configured, SyncTarget{Name: ing.Name, Agent: agent, State: NewSyncState(r.now), Ingress: ing})
	}
	switch {
	case r.src == nil:
		r.discoveryOff = "no headscale node source"
	case !rc.LinkHostSet:
		r.discoveryOff = "link_host is not set in ENROLL_CONFIG"
	}
	return r, nil
}

// SetStore attaches the store that holds the approvals.
func (r *Registry) SetStore(s *Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.store = s
}

// tokenVar is the default token variable of an ingress: AGENT_TOKEN_<NAME>.
func tokenVar(name string) string {
	return "AGENT_TOKEN_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// configuredToken finds the token of a configured ingress: its token_env, else AGENT_TOKEN_<NAME>,
// else (first ingress only) AGENT_TOKEN. The error names the variable, never a value.
func (r *Registry) configuredToken(i int, ing Ingress) (string, error) {
	if ing.TokenEnv != "" {
		if v := r.getenv(ing.TokenEnv); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("ingress %s: token variable %s is not set", ing.Name, ing.TokenEnv)
	}
	if v := r.getenv(tokenVar(ing.Name)); v != "" {
		return v, nil
	}
	if i == 0 {
		if v := r.getenv("AGENT_TOKEN"); v != "" {
			return v, nil
		}
	}
	return "", fmt.Errorf("ingress %s: token variable %s is not set", ing.Name, tokenVar(ing.Name))
}

// resolver finds an agent's address at every call: the tailnet IPv4 of the node named like the
// ingress that carries tag:ingress.
func (r *Registry) resolver(name string) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		nodes, err := r.src.Nodes(ctx)
		if err != nil {
			return "", errors.New("node list unavailable")
		}
		for _, n := range nodes {
			if isIngressNode(n, name) {
				if ip := nodeIP(n); ip != "" {
					return "http://" + net.JoinHostPort(ip, ingressAgentPort), nil
				}
			}
		}
		return "", errors.New("no tagged node with an address")
	}
}

func hasIngressTag(n headscale.Node) bool {
	for _, t := range n.Tags {
		if t == ingressTag {
			return true
		}
	}
	return false
}

func isIngressNode(n headscale.Node, name string) bool {
	return hasIngressTag(n) && (n.Name == name || n.GivenName == name)
}

// nodeName is the ingress name a tagged node stands for: its name when that is a valid ingress name.
func nodeName(n headscale.Node) string {
	for _, c := range []string{n.Name, n.GivenName} {
		if ingressNameRe.MatchString(c) {
			return c
		}
	}
	return ""
}

func nodeIP(n headscale.Node) string {
	for _, s := range n.IPAddresses {
		if ip := net.ParseIP(s); ip != nil && ip.To4() != nil {
			return ip.String()
		}
	}
	return ""
}

// DiscoveryEnabled reports whether discovery runs.
func (r *Registry) DiscoveryEnabled() bool { return r.discoveryOff == "" }

// LinkConfig is the stable part of the configuration: the link host and the first configured
// ingress, which is all that links and subscription URLs use.
func (r *Registry) LinkConfig() IngressConfig { return r.base }

// First is the sync target of the first configured ingress.
func (r *Registry) First() SyncTarget { return r.configured[0] }

// Current returns the ingress set. With fresh it first refreshes the node list and reads the
// parameters of discovered nodes whose last reading is older than a minute.
func (r *Registry) Current(ctx context.Context, fresh bool) Current {
	r.mu.Lock()
	defer r.mu.Unlock()
	if fresh {
		r.refreshLocked(ctx, "")
	}
	return r.buildLocked()
}

// refreshLocked reloads the node list and re-reads the stale probes; force names a node whose
// parameters are read again whatever their age.
func (r *Registry) refreshLocked(ctx context.Context, force string) {
	if r.src != nil {
		if nodes, err := r.src.Nodes(ctx); err == nil {
			r.nodes = nodes
		}
	}
	if r.discoveryOff != "" {
		return
	}
	configured := map[string]bool{}
	for _, t := range r.configured {
		configured[t.Name] = true
	}
	cands := map[string]headscale.Node{}
	for _, n := range r.nodes {
		if !hasIngressTag(n) {
			continue
		}
		name := nodeName(n)
		if name == "" || configured[name] {
			continue
		}
		if _, dup := cands[name]; !dup {
			cands[name] = n
		}
	}
	for name := range r.probes {
		if _, ok := cands[name]; !ok {
			delete(r.probes, name) // the node is gone
		}
	}
	type result struct {
		name string
		p    *probe
	}
	var wg sync.WaitGroup
	out := make(chan result, len(cands))
	now := r.now()
	for name, n := range cands {
		token := r.getenv(tokenVar(name))
		if token == "" {
			r.probes[name] = &probe{at: now, state: stNoToken, detail: "no token variable for this ingress"}
			continue
		}
		if p := r.probes[name]; p != nil && p.state != stNoToken && now.Sub(p.at) < infoTTL && force != name {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, state, detail := r.fetchInfo(ctx, n, name, token)
			out <- result{name, &probe{info: info, state: state, detail: detail}}
		}()
	}
	wg.Wait()
	close(out)
	for res := range out {
		res.p.at = r.now()
		if old := r.probes[res.name]; old != nil && res.p.state == stUnreachable {
			res.p.info = old.info // a failed reading keeps the last good parameters
		}
		r.probes[res.name] = res.p
	}
}

// fetchInfo reads GET /v1/info of one node: 5 s, 64 KiB, no redirects. The returned detail never
// carries the token or any published value.
func (r *Registry) fetchInfo(ctx context.Context, n headscale.Node, name, token string) (*infoResult, string, string) {
	if !n.Online {
		return nil, stUnreachable, "the node is offline"
	}
	ip := nodeIP(n)
	if ip == "" {
		return nil, stUnreachable, "the node has no tailnet address"
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), infoTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(ip, ingressAgentPort)+"/v1/info", nil)
	if err != nil {
		return nil, stUnreachable, "invalid request"
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, stUnreachable, "the agent did not answer"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, stUnreachable, fmt.Sprintf("the agent answered %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxInfoBody+1))
	if err != nil {
		return nil, stUnreachable, "the answer could not be read"
	}
	if len(body) > maxInfoBody {
		return nil, stInvalid, "the answer is too large"
	}
	info, err := parseInfo(name, body)
	if err != nil {
		return nil, stInvalid, err.Error()
	}
	return info, "", ""
}

var infoKeys = []string{"name", "host", "port", "sni", "public_key", "short_id"}

// parseInfo validates the published parameters with the rules of the configuration (only the six
// published keys are read; an agent cannot choose its own URL, token variable or credential mode).
func parseInfo(node string, body []byte) (*infoResult, error) {
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil || raw == nil {
		return nil, errors.New("the answer is not a JSON object")
	}
	if name, _ := rawString(raw["name"]); name != node {
		return nil, errors.New("the published name does not match the node name")
	}
	pick := map[string]json.RawMessage{"uuids": json.RawMessage(`"` + uuidsDerived + `"`)}
	for _, k := range infoKeys {
		if v, ok := raw[k]; ok {
			pick[k] = v
		}
	}
	b, _ := json.Marshal(pick)
	ing, err := parseIngress(0, b)
	if err != nil {
		return nil, errors.New(strings.TrimPrefix(err.Error(), "ENROLL_CONFIG: "))
	}
	return &infoResult{ing: ing, fp: fingerprint(ing)}, nil
}

// fingerprint is the hex SHA-256 of the canonical JSON of the published parameters: the keys name,
// host, port, sni, public_key, short_id in this order, no whitespace.
func fingerprint(in Ingress) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(struct {
		Name      string `json:"name"`
		Host      string `json:"host"`
		Port      int    `json:"port"`
		SNI       string `json:"sni"`
		PublicKey string `json:"public_key"`
		ShortID   string `json:"short_id"`
	}{in.Name, in.Host, in.Port, in.SNI, in.PublicKey, in.ShortID})
	sum := sha256.Sum256(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
	return hex.EncodeToString(sum[:])
}

func prefix(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

// buildLocked assembles the current view from the cached readings and the stored approvals. The
// approvals are read every time, so an approval change takes effect at once.
func (r *Registry) buildLocked() Current {
	cur := Current{Cfg: r.base, Discovery: DiscoveryStatus{Enabled: r.discoveryOff == "", Reason: r.discoveryOff}}
	cur.Cfg.Ingresses = nil
	hosts := map[string]bool{}
	for _, t := range r.configured {
		cur.Cfg.Ingresses = append(cur.Cfg.Ingresses, t.Ingress)
		cur.Targets = append(cur.Targets, t)
		hosts[t.Ingress.Host] = true
		snap := t.State.Snapshot()
		cur.Views = append(cur.Views, IngressView{Name: t.Name, Host: t.Ingress.Host, Source: "configured",
			Approval: stConfigured, Active: true, Online: r.onlineLocked(t.Name), Sync: &snap})
	}
	if r.discoveryOff != "" {
		return cur
	}
	approvals := map[string]string{}
	var aerr error
	if r.store != nil {
		approvals, aerr = r.store.Approvals()
		if aerr != nil {
			slog.Warn("enrolment approvals unreadable", "error_type", errType(aerr))
			approvals = map[string]string{} // fail closed: nothing discovered is approved
		}
	}
	configured := map[string]bool{}
	for _, t := range r.configured {
		configured[t.Name] = true
	}
	seen := map[string]bool{}
	var names []string
	for n := range r.probes {
		seen[n] = true
		names = append(names, n)
	}
	for n := range approvals {
		if !seen[n] && !configured[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		v := IngressView{Name: name, Source: "discovered", Online: r.onlineLocked(name)}
		p := r.probes[name]
		storedFP, approved := approvals[name]
		v.Approved = approved
		switch {
		case p == nil:
			v.Approval, v.Detail = stUnreachable, "the node is not in the node list"
		case p.info == nil:
			v.Approval, v.Detail = p.state, p.detail
		default:
			ing := p.info.ing
			v.Host, v.Fingerprint = ing.Host, p.info.fp
			v.PublicKeyPref, v.ShortIDPref = prefix(ing.PublicKey), prefix(ing.ShortID)
			switch {
			case hosts[ing.Host]:
				v.Approval, v.Detail = stInvalid, "the host is already used by another ingress"
			case !approved:
				v.Approval = stPending
			case storedFP == p.info.fp:
				v.Approval = stApproved
			default:
				v.Approval = stChanged
			}
			hosts[ing.Host] = true
			if p.state != "" {
				v.Detail = "the last check failed: " + p.detail
			}
		}
		if aerr != nil && v.Approval == stPending {
			v.Detail = "the stored approvals could not be read"
		}
		if v.Approval == stApproved {
			v.Active = true
			t := r.discoveredTarget(name, p.info.ing)
			cur.Cfg.Ingresses = append(cur.Cfg.Ingresses, t.Ingress)
			cur.Targets = append(cur.Targets, t)
			snap := t.State.Snapshot()
			v.Sync = &snap
		} else {
			cur.Pending = append(cur.Pending, name)
		}
		cur.Views = append(cur.Views, v)
	}
	return cur
}

// discoveredTarget is the sync target of an approved discovered ingress; its state and agent live
// as long as the registry.
func (r *Registry) discoveredTarget(name string, ing Ingress) SyncTarget {
	st := r.dstate[name]
	if st == nil {
		st = NewSyncState(r.now)
		r.dstate[name] = st
	}
	ag := r.dagent[name]
	if ag == nil {
		ag = NewAgentFunc(r.resolver(name), r.getenv(tokenVar(name)), r.hc)
		r.dagent[name] = ag
	}
	return SyncTarget{Name: name, Agent: ag, State: st, Ingress: ing}
}

// onlineLocked is the headscale online flag of the node named like the ingress, or nil.
func (r *Registry) onlineLocked(name string) *bool {
	for _, n := range r.nodes {
		if isIngressNode(n, name) {
			v := n.Online
			return &v
		}
	}
	return nil
}

func (r *Registry) isConfigured(name string) bool {
	for _, t := range r.configured {
		if t.Name == name {
			return true
		}
	}
	return false
}

// Approve approves a discovered ingress, but only when the parameters it publishes RIGHT NOW (read
// again, not from the cache) have the posted fingerprint.
func (r *Registry) Approve(ctx context.Context, name, fingerprint string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isConfigured(name) {
		return &ApprovalError{http.StatusConflict, "already configured"}
	}
	if r.discoveryOff != "" || r.store == nil {
		return &ApprovalError{http.StatusNotFound, "unknown ingress"}
	}
	r.refreshLocked(ctx, name)
	p := r.probes[name]
	if p == nil {
		return &ApprovalError{http.StatusNotFound, "unknown ingress"}
	}
	if p.info == nil || p.state != "" {
		return &ApprovalError{http.StatusConflict, "the ingress parameters cannot be read now"}
	}
	for _, v := range r.buildLocked().Views {
		if v.Name == name && v.Approval == stInvalid {
			return &ApprovalError{http.StatusConflict, "the ingress parameters are not valid: " + v.Detail}
		}
	}
	if p.info.fp != fingerprint {
		return &ApprovalError{http.StatusConflict, "the fingerprint does not match the parameters the ingress publishes now; reload and review them"}
	}
	if err := r.store.Approve(name, fingerprint); err != nil {
		return err
	}
	return nil
}

// Unapprove removes the approval: the ingress leaves profiles and stops being synced (its agent
// keeps the last set it received).
func (r *Registry) Unapprove(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isConfigured(name) {
		return &ApprovalError{http.StatusConflict, "a configured ingress is not approved here; remove it from ENROLL_CONFIG"}
	}
	if r.discoveryOff != "" || r.store == nil {
		return &ApprovalError{http.StatusNotFound, "unknown ingress"}
	}
	approvals, err := r.store.Approvals()
	if err != nil {
		return err
	}
	_, has := approvals[name]
	if _, known := r.probes[name]; !has && !known {
		return &ApprovalError{http.StatusNotFound, "unknown ingress"}
	}
	return r.store.Unapprove(name)
}

// newStaticRegistry is the registry of the single-agent constructors: the configured ingresses,
// all served by one given agent, and no discovery.
func newStaticRegistry(store *Store, cfg IngressConfig, agent Agent) *Registry {
	agents := map[string]Agent{}
	for _, ing := range cfg.Ingresses {
		agents[ing.Name] = agent
	}
	if len(cfg.Ingresses) == 0 {
		cfg.Ingresses = []Ingress{{UUIDs: uuidsBase}}
		agents[""] = agent
	}
	r, _ := NewRegistry(RegistryConfig{Cfg: cfg, Agents: agents, Now: store.clock})
	r.store = store
	return r
}
