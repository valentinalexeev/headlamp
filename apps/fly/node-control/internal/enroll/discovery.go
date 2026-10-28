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
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"example.com/node-control/internal/headscale"
	"tailscale.com/util/dnsname"
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
	Legacy      bool   // the old single-host file: its token is AGENT_TOKEN and nothing else
	// Agents are ready-made agents by ingress name (tests and the single-agent constructor); an
	// ingress listed here needs no URL and no token variable.
	Agents map[string]Agent
}

// IngressView is one row of the ingress list. Unapproved ingresses show only a short prefix of
// their Reality public key and short id.
type IngressView struct {
	Name           string        `json:"name"`
	Host           string        `json:"host"`
	Source         string        `json:"source"`   // configured | discovered
	Approval       string        `json:"approval"` // configured, or one of the discovered states
	Active         bool          `json:"active"`   // used for profiles and sync
	Approved       bool          `json:"approved"` // an approval is stored for it (it can be removed)
	Fingerprint    string        `json:"fingerprint,omitempty"`
	PublicKeyPref  string        `json:"public_key_prefix,omitempty"`
	ShortIDPref    string        `json:"short_id_prefix,omitempty"`
	NodeID         uint64        `json:"node_id,omitempty"`            // the node the parameters were read from
	Address        string        `json:"address,omitempty"`            // its tailnet IPv4
	MachineKeyPref string        `json:"machine_key_prefix,omitempty"` // a short prefix of its machine key
	Hostname       string        `json:"hostname,omitempty"`           // the name the machine reports (plain text, shortened)
	GivenName      string        `json:"given_name,omitempty"`         // the name headscale gives it
	Online         *bool         `json:"online"`
	Sync           *SyncSnapshot `json:"sync"`
	Detail         string        `json:"detail,omitempty"`
}

// DiscoveryStatus says whether discovery runs and, when it does not, why.
type DiscoveryStatus struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason,omitempty"`
}

// Current is one consistent view of the ingress set.
type Current struct {
	Cfg     IngressConfig // configured + approved discovered ingresses, for profiles and sync
	Targets []SyncTarget  // the same ingresses as sync targets, in the same order
	// SyncTargets are Targets plus approved discovered ingresses whose parameters have not been
	// read (yet) but whose pinned node is in the node list. They only keep the credential set of an
	// ingress that was approved before, so a revocation reaches it right after a restart; they are
	// never offered in profiles.
	SyncTargets []SyncTarget
	Views       []IngressView // everything, configured first, then discovered by name
	Pending     []string      // discovered ingresses that are not approved
	Discovery   DiscoveryStatus
}

// probe is what is known about one discovered node.
type probe struct {
	at     time.Time
	nodeID uint64 // the headscale node that was asked
	node   headscale.Node
	info   *infoResult // the last parameters read successfully
	state  string      // "" when the last attempt succeeded
	detail string
	dup    bool // another tagged node carries the same name; this is the one the approval is pinned to
}

type infoResult struct {
	ing Ingress
	fp  string
}

// Registry is the dynamic ingress set. It is safe for concurrent use.
type Registry struct {
	fmu          sync.Mutex // serialises refreshes (network); never held while mu is wanted by a reader
	mu           sync.Mutex // guards everything below, held only for memory work
	legacy       bool
	store        *Store
	base         IngressConfig
	configured   []SyncTarget
	src          headscale.NodeSource
	getenv       func(string) string
	hc           *http.Client
	now          func() time.Time
	discoveryOff string

	nodes       []headscale.Node
	nodesLoaded bool
	rowsFailed  bool // the approvals were unreadable at the last refresh (logged once per change)
	probes      map[string]*probe
	dstate      map[string]*SyncState
	dagent      map[string]nodeAgent
}

// nodeAgent is the agent of a discovered ingress, bound to the node id it was approved on.
type nodeAgent struct {
	id    uint64
	key   string
	agent Agent
}

// NewRegistry validates the configured ingresses and builds their agents. Errors name an ingress
// and a variable, never a value.
func NewRegistry(rc RegistryConfig) (*Registry, error) {
	r := &Registry{base: rc.Cfg, src: rc.Src, getenv: rc.Getenv, hc: rc.HC, now: rc.Now,
		probes: map[string]*probe{}, dstate: map[string]*SyncState{}, dagent: map[string]nodeAgent{}, legacy: rc.Legacy}
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
				return nil, fmt.Errorf("bridge %s: no agent_url and no headscale source to find its agent", ing.Name)
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
// else (first ingress only) AGENT_TOKEN. The old single-host form uses AGENT_TOKEN only. The
// error names the variable(s), never a value.
func (r *Registry) configuredToken(i int, ing Ingress) (string, error) {
	if r.legacy {
		if v := r.getenv("AGENT_TOKEN"); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("bridge %s: token variable AGENT_TOKEN is not set", ing.Name)
	}
	if ing.TokenEnv != "" {
		if v := r.getenv(ing.TokenEnv); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("bridge %s: token variable %s is not set", ing.Name, ing.TokenEnv)
	}
	if v := r.getenv(tokenVar(ing.Name)); v != "" {
		return v, nil
	}
	if i == 0 {
		if v := r.getenv("AGENT_TOKEN"); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("bridge %s: token variable %s is not set (nor AGENT_TOKEN)", ing.Name, tokenVar(ing.Name))
	}
	return "", fmt.Errorf("bridge %s: token variable %s is not set", ing.Name, tokenVar(ing.Name))
}

// resolver finds the agent of a configured ingress at every call: the tailnet IPv4 of the tagged
// node whose given name is the ingress name. headscale derives a given name from the hostname the
// machine reports (at registration and again whenever the hostname changes, as long as the given
// name still follows it), so a given name counts only once the administrator has renamed the node
// to something the hostname does not produce. Without that, whoever controls a tagged machine could
// take a name by changing its hostname. An explicit `agent_url` avoids the lookup altogether.
func (r *Registry) resolver(name string) func(ctx context.Context) (string, error) {
	return r.resolve(func(nodes []headscale.Node) (headscale.Node, error) {
		var found []headscale.Node
		refused := false
		for _, n := range nodes {
			if !isIngressNode(n, name) {
				continue
			}
			if autoDerivedGivenName(n.GivenName, n.Name) {
				refused = true
				continue
			}
			found = append(found, n)
		}
		if len(found) == 0 && refused {
			slog.Warn("enrolment agent lookup refused: the given name of the tagged node follows its hostname; "+
				"run `headscale nodes rename <id> <name>` to a name the hostname does not produce, or set agent_url",
				"ingress", name)
		}
		// Two nodes answering to one name is ambiguous: the agent's token and credentials must not
		// go to a guess.
		if len(found) != 1 {
			return headscale.Node{}, errors.New("no unique administrator-named tagged node for this bridge")
		}
		return found[0], nil
	})
}

// pinnedTo reports whether a tagged node is the one an approval was given to: the id AND the
// machine key, both non-empty. A headscale database that was rebuilt can hand an old id to another
// machine; it cannot give that machine the old machine key.
func pinnedTo(n headscale.Node, id uint64, machineKey string) bool {
	return id != 0 && machineKey != "" && n.ID == id && n.MachineKey == machineKey && hasIngressTag(n)
}

// resolverFor finds the agent of a discovered, approved ingress: the tagged node with the pinned
// id and machine key. The name is not consulted (it may have collided and been suffixed since the
// approval).
func (r *Registry) resolverFor(name string, id uint64, machineKey string) func(ctx context.Context) (string, error) {
	return r.resolve(func(nodes []headscale.Node) (headscale.Node, error) {
		for _, n := range nodes {
			if pinnedTo(n, id, machineKey) {
				return n, nil
			}
		}
		return headscale.Node{}, errors.New("the approved node is not in the node list")
	})
}

func (r *Registry) resolve(pick func([]headscale.Node) (headscale.Node, error)) func(ctx context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		nodes, err := r.src.Nodes(ctx)
		if err != nil {
			return "", errors.New("node list unavailable")
		}
		n, err := pick(nodes)
		if err != nil {
			return "", err
		}
		ip := nodeIP(n)
		if ip == "" {
			return "", errors.New("the tagged node has no address")
		}
		return "http://" + net.JoinHostPort(ip, ingressAgentPort), nil
	}
}

// autoDerivedGivenName reports whether headscale treats a given name as derived from the node's own
// hostname, which it re-derives whenever the hostname changes. It is headscale's own rule
// (hscontrol/state isAutoDerivedGivenName, v0.29.3) over the same sanitiser (tailscale.com
// util/dnsname.SanitizeHostname; the file is identical in v1.98.10, which this module pins, and
// v1.101.0-pre, which headscale v0.29.3 pins): the given name equals the sanitised hostname, or that
// plus "-<integer>" (the suffix of a collision). When the sanitised hostname is empty headscale
// names the node "node" (and "node-1"...); such a node is refused too, because nothing the
// operator sees then ties its name to it.
func autoDerivedGivenName(given, hostname string) bool {
	if given == "" {
		return false
	}
	base := dnsname.SanitizeHostname(hostname)
	if base == "" || given == base {
		return true
	}
	suffix, ok := strings.CutPrefix(given, base+"-")
	if !ok {
		return false
	}
	_, err := strconv.Atoi(suffix)
	return err == nil
}

func hasIngressTag(n headscale.Node) bool {
	for _, t := range n.Tags {
		if t == ingressTag {
			return true
		}
	}
	return false
}

// isIngressNode matches a tagged node by its given name. The given name alone does not prove who
// chose it (see resolver); this match is for listing and for the online flag, never for sending
// a token or credentials.
func isIngressNode(n headscale.Node, name string) bool {
	return hasIngressTag(n) && n.GivenName == name
}

// nodeName is the ingress name a tagged node stands for: its given name when that is a valid
// ingress name.
func nodeName(n headscale.Node) string {
	if ingressNameRe.MatchString(n.GivenName) {
		return n.GivenName
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
// parameters of discovered nodes whose last reading is older than a minute; the network work
// happens outside the registry lock, so readers that do not ask for it never wait for it.
func (r *Registry) Current(ctx context.Context, fresh bool) Current {
	if fresh {
		r.refresh(ctx, "")
	} else {
		r.loadNodesOnce(ctx)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.buildLocked()
}

// loadNodesOnce reads the node list the first time a caller that does not refresh needs it (the
// node list is cached by its source; no /v1/info is read).
func (r *Registry) loadNodesOnce(ctx context.Context) {
	if r.src == nil {
		return
	}
	r.mu.Lock()
	done := r.nodesLoaded
	r.mu.Unlock()
	if done {
		return
	}
	nodes, err := r.src.Nodes(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.nodes, r.nodesLoaded = nodes, true
	}
}

// refresh reloads the node list and re-reads the stale probes; force names a node whose
// parameters are read again whatever their age. Refreshes run one at a time.
func (r *Registry) refresh(ctx context.Context, force string) {
	r.fmu.Lock()
	defer r.fmu.Unlock()
	if r.src != nil {
		if nodes, err := r.src.Nodes(ctx); err == nil {
			r.mu.Lock()
			r.nodes, r.nodesLoaded = nodes, true
			r.mu.Unlock()
		}
	}
	if r.discoveryOff != "" {
		return
	}
	type job struct {
		name  string
		node  headscale.Node
		token string
	}
	var jobs []job
	r.mu.Lock()
	configured := map[string]bool{}
	for _, t := range r.configured {
		configured[t.Name] = true
	}
	now := r.now()
	var rows map[string]ApprovalRow
	if r.store != nil {
		var err error
		if rows, err = r.store.ApprovalRows(); err != nil {
			if !r.rowsFailed {
				slog.Warn("enrolment approvals unreadable", "error_type", errType(err))
			}
			r.rowsFailed = true
		} else {
			r.rowsFailed = false
		}
	}
	// A node an approval is pinned to belongs to that approval's name only.
	pinnedBy := map[uint64]string{}
	for name, row := range rows {
		if !configured[name] && row.NodeID != 0 && row.MachineKey != "" {
			pinnedBy[row.NodeID] = name
		}
	}
	cands := map[string][]headscale.Node{}
	tagged := map[uint64]headscale.Node{}
	for _, n := range r.nodes {
		if !hasIngressTag(n) {
			continue
		}
		tagged[n.ID] = n
		name := nodeName(n)
		if name == "" || configured[name] {
			continue
		}
		if owner, ok := pinnedBy[n.ID]; ok && owner != name {
			continue // pinned to another name: never asked as a second candidate
		}
		cands[name] = append(cands[name], n)
	}
	// An approval pinned to a node is about THAT node (id and machine key): it is the only one asked,
	// whatever its name is now, and no other node is ever sent the token for this ingress.
	absent := map[string]*probe{}
	dups := map[string]bool{}
	for _, name := range pinnedBy {
		row := rows[name]
		x, ok := tagged[row.NodeID]
		others := 0
		for _, n := range cands[name] {
			if n.ID != row.NodeID {
				others++
			}
		}
		if ok && pinnedTo(x, row.NodeID, row.MachineKey) {
			cands[name] = []headscale.Node{x}
			dups[name] = others > 0
			continue
		}
		delete(cands, name)
		switch {
		case ok:
			absent[name] = &probe{at: now, state: stChanged, detail: "the node id is now held by a machine with another machine key"}
		default:
			d := "the approved node is not present"
			if others > 0 {
				d += "; another node carries the name"
			}
			absent[name] = &probe{at: now, state: stInvalid, detail: d}
		}
	}
	for name := range r.probes {
		if _, ok := cands[name]; !ok {
			delete(r.probes, name) // the node is gone
		}
	}
	for name, p := range absent {
		r.probes[name] = p
	}
	for name, ns := range cands {
		if len(ns) > 1 {
			// An ambiguous name is never an ingress (a pinned approval was settled above).
			r.probes[name] = &probe{at: now, state: stInvalid, detail: "several nodes carry this name"}
			continue
		}
		n := ns[0]
		if n.ID == 0 { // an id of 0 can never be pinned
			r.probes[name] = &probe{at: now, state: stInvalid, detail: "the node has no id"}
			continue
		}
		token := r.getenv(tokenVar(name))
		if token == "" {
			r.probes[name] = &probe{at: now, nodeID: n.ID, node: n, state: stNoToken, detail: "no token variable for this bridge"}
			continue
		}
		if p := r.probes[name]; p != nil && p.nodeID == n.ID && p.state != stNoToken &&
			now.Sub(p.at) < infoTTL && force != name {
			p.dup, p.node = dups[name], n
			continue
		}
		jobs = append(jobs, job{name, n, token})
	}
	r.mu.Unlock()

	type result struct {
		name string
		p    *probe
	}
	var wg sync.WaitGroup
	out := make(chan result, len(jobs))
	for _, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, state, detail := r.fetchInfo(ctx, j.node, j.name, j.token)
			out <- result{j.name, &probe{nodeID: j.node.ID, node: j.node, info: info, state: state, detail: detail, dup: dups[j.name]}}
		}()
	}
	wg.Wait()
	close(out)
	r.mu.Lock()
	defer r.mu.Unlock()
	for res := range out {
		res.p.at = r.now()
		// a failed reading keeps the last good parameters, but only of the same node
		if old := r.probes[res.name]; old != nil && res.p.state == stUnreachable && old.nodeID == res.p.nodeID {
			res.p.info = old.info
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

// plainText drops everything that is not a printable character (control characters, bidirectional
// overrides, line breaks) and cuts the rest to max characters: the name a machine reports about
// itself is shown to an operator who is about to approve it.
func plainText(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if !unicode.IsPrint(r) {
			continue
		}
		if n++; n > max {
			break
		}
		b.WriteRune(r)
	}
	return b.String()
}

// keyPrefix is the first 8 characters of a machine key (not secret, but never needed in full).
func keyPrefix(k string) string {
	k = strings.TrimPrefix(k, "mkey:")
	if len(k) > 8 {
		k = k[:8]
	}
	return k
}

func prefix(s string) string {
	if len(s) > 6 {
		return s[:6]
	}
	return s
}

// buildLocked assembles the current view from the cached readings and the stored approvals. The
// approvals are read every time, so an approval change takes effect at once. An approval pins the
// parameters AND the headscale node that published them: another node with the same name and the
// same parameters is "changed", and one that publishes nothing is not used at all.
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
	cur.SyncTargets = cur.Targets
	if r.discoveryOff != "" {
		return cur
	}
	approvals := map[string]ApprovalRow{}
	var aerr error
	if r.store != nil {
		approvals, aerr = r.store.ApprovalRows()
		if aerr != nil {
			slog.Warn("enrolment approvals unreadable", "error_type", errType(aerr))
			approvals = map[string]ApprovalRow{} // fail closed: nothing discovered is approved
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
	views := make([]IngressView, len(names))
	infos := make([]*infoResult, len(names))
	for k, name := range names {
		v := IngressView{Name: name, Source: "discovered"}
		p := r.probes[name]
		row, approved := approvals[name]
		v.Approved = approved
		switch {
		case approved && row.NodeID != 0:
			v.Online = r.onlineByIDLocked(row.NodeID) // the pinned node, not whoever carries the name
		case p != nil && p.nodeID != 0:
			v.Online = r.onlineByIDLocked(p.nodeID)
		default:
			v.Online = r.onlineLocked(name)
		}
		if p != nil && p.node.ID != 0 {
			v.NodeID, v.Address = p.node.ID, nodeIP(p.node)
			v.Hostname, v.GivenName = plainText(p.node.Name, 63), plainText(p.node.GivenName, 63)
			v.MachineKeyPref = keyPrefix(p.node.MachineKey)
		}
		switch {
		case p == nil:
			v.Approval, v.Detail = stUnreachable, "the node is not in the node list"
		case p.info == nil:
			v.Approval, v.Detail = p.state, p.detail
		default:
			ing := p.info.ing
			infos[k] = p.info
			v.Host, v.Fingerprint = ing.Host, p.info.fp
			v.PublicKeyPref, v.ShortIDPref = prefix(ing.PublicKey), prefix(ing.ShortID)
			switch {
			case !approved:
				v.Approval = stPending
			case row.NodeID == 0 || p.nodeID == 0:
				v.Approval = stChanged
				if row.Fingerprint == p.info.fp {
					v.Detail = "approval predates node pinning, approve again"
				}
			case row.MachineKey == "" || p.node.MachineKey == "":
				// an approval from before machine keys were pinned, or an API that does not report them:
				// never honoured, always asked again
				v.Approval, v.Detail = stChanged, "approval predates machine key pinning (or the node list has no machine key), approve again"
			case row.Fingerprint == p.info.fp && row.NodeID == p.nodeID && row.MachineKey == p.node.MachineKey:
				v.Approval = stApproved
			case row.Fingerprint == p.info.fp:
				v.Approval, v.Detail = stChanged, "another node publishes these parameters"
			default:
				v.Approval = stChanged
			}
			if p.state != "" {
				v.Detail = "the last check failed: " + p.detail
			}
		}
		if p != nil && p.dup && v.Detail == "" {
			v.Detail = "another tagged node carries this name; it is ignored"
		}
		if aerr != nil && v.Approval == stPending {
			v.Detail = "the stored approvals could not be read"
		}
		views[k] = v
	}
	// Hosts are claimed in passes: configured ingresses first (above), then approved ones, then the
	// rest, so a node that is not approved can never invalidate one that is.
	claim := func(k int) {
		if infos[k] == nil {
			return
		}
		host := infos[k].ing.Host
		if hosts[host] {
			views[k].Approval, views[k].Detail = stInvalid, "the host is already used by another bridge"
			return
		}
		hosts[host] = true
	}
	for k := range views {
		if views[k].Approval == stApproved {
			claim(k)
		}
	}
	for k := range views {
		if views[k].Approval != stApproved {
			claim(k)
		}
	}
	var extra []SyncTarget
	for k, v := range views {
		if v.Approval != stApproved && r.unverifiedLocked(v.Name, approvals[v.Name]) {
			// approved on this very node, parameters not read: keep its set current, nothing more
			extra = append(extra, r.discoveredTarget(v.Name, approvals[v.Name].NodeID, approvals[v.Name].MachineKey, Ingress{Name: v.Name, UUIDs: uuidsDerived}))
			views[k].Detail = "approved; the parameters have not been verified yet"
		}
	}
	for k, v := range views {
		if v.Approval == stApproved {
			v.Active = true
			p := r.probes[v.Name]
			t := r.discoveredTarget(v.Name, p.nodeID, p.node.MachineKey, infos[k].ing)
			cur.Cfg.Ingresses = append(cur.Cfg.Ingresses, t.Ingress)
			cur.Targets = append(cur.Targets, t)
			snap := t.State.Snapshot()
			v.Sync = &snap
		} else {
			cur.Pending = append(cur.Pending, v.Name)
		}
		cur.Views = append(cur.Views, v)
	}
	cur.SyncTargets = append(append([]SyncTarget(nil), cur.Targets...), extra...)
	return cur
}

// unverifiedLocked reports whether an approved discovered ingress has no reading of its parameters
// (none yet, or the last one failed) while the node the approval is pinned to is in the node list
// and its token variable is set.
func (r *Registry) unverifiedLocked(name string, row ApprovalRow) bool {
	if row.NodeID == 0 || row.MachineKey == "" || r.getenv(tokenVar(name)) == "" {
		return false
	}
	if p := r.probes[name]; p != nil && !(p.info == nil && p.state == stUnreachable && p.nodeID == row.NodeID) {
		return false
	}
	for _, n := range r.nodes {
		if pinnedTo(n, row.NodeID, row.MachineKey) {
			return true
		}
	}
	return false
}

// discoveredTarget is the sync target of an approved discovered ingress; its state lives as long as
// the registry, its agent as long as the node id it talks to.
func (r *Registry) discoveredTarget(name string, nodeID uint64, machineKey string, ing Ingress) SyncTarget {
	st := r.dstate[name]
	if st == nil {
		st = NewSyncState(r.now)
		r.dstate[name] = st
	}
	na, ok := r.dagent[name]
	if !ok || na.id != nodeID || na.key != machineKey {
		na = nodeAgent{nodeID, machineKey, NewAgentFunc(r.resolverFor(name, nodeID, machineKey), r.getenv(tokenVar(name)), r.hc)}
		r.dagent[name] = na
	}
	return SyncTarget{Name: name, Agent: na.agent, State: st, Ingress: ing}
}

// onlineByIDLocked is the online flag of the tagged node with this id, or nil.
func (r *Registry) onlineByIDLocked(id uint64) *bool {
	for _, n := range r.nodes {
		if n.ID == id && hasIngressTag(n) {
			v := n.Online
			return &v
		}
	}
	return nil
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
// again, not from the cache) have the posted fingerprint AND come from the node the operator
// reviewed (nodeID): an impostor holding the name can publish the same values. The approval pins
// that node.
func (r *Registry) Approve(ctx context.Context, name, fingerprint string, nodeID uint64) error {
	if r.isConfigured(name) {
		return &ApprovalError{http.StatusConflict, "already configured"}
	}
	r.mu.Lock()
	off, st := r.discoveryOff != "", r.store
	r.mu.Unlock()
	if off || st == nil {
		return &ApprovalError{http.StatusNotFound, "unknown bridge"}
	}
	r.refresh(ctx, name) // network, outside the lock
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.probes[name]
	if p == nil {
		return &ApprovalError{http.StatusNotFound, "unknown bridge"}
	}
	if p.info == nil || p.state != "" {
		return &ApprovalError{http.StatusConflict, "the bridge parameters cannot be read now"}
	}
	for _, v := range r.buildLocked().Views {
		if v.Name == name && v.Approval == stInvalid {
			return &ApprovalError{http.StatusConflict, "the bridge parameters are not valid: " + v.Detail}
		}
	}
	if nodeID == 0 || p.nodeID != nodeID {
		return &ApprovalError{http.StatusConflict, "the node that publishes this bridge is not the one that was reviewed; reload and review it"}
	}
	if p.info.fp != fingerprint {
		return &ApprovalError{http.StatusConflict, "the fingerprint does not match the parameters the bridge publishes now; reload and review them"}
	}
	if p.node.MachineKey == "" {
		return &ApprovalError{http.StatusConflict, "the node list does not report the machine key of this node, so it cannot be pinned"}
	}
	return r.store.ApproveNode(name, fingerprint, p.nodeID, p.node.MachineKey)
}

// Unapprove removes the approval: the ingress leaves profiles and stops being synced (its agent
// keeps the last set it received).
func (r *Registry) Unapprove(ctx context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.isConfigured(name) {
		return &ApprovalError{http.StatusConflict, "a configured bridge is not approved here; remove it from ENROLL_CONFIG"}
	}
	if r.discoveryOff != "" || r.store == nil {
		return &ApprovalError{http.StatusNotFound, "unknown bridge"}
	}
	approvals, err := r.store.Approvals()
	if err != nil {
		return err
	}
	_, has := approvals[name]
	if _, known := r.probes[name]; !has && !known {
		return &ApprovalError{http.StatusNotFound, "unknown bridge"}
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
