package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// Ingress is one ingress node: where its clients connect, its Reality parameters and how the
// controller reaches its agent. Name is the stable identifier shown on the page and used for the
// agent token variable and for headscale discovery.
type Ingress struct {
	Name      string `json:"name"`
	Host      string `json:"host"`
	PublicKey string `json:"public_key"`
	ShortID   string `json:"short_id"`
	SNI       string `json:"sni"`
	Port      int    `json:"port"`
	AgentURL  string `json:"agent_url"`
	TokenEnv  string `json:"token_env"`
	// UUIDs is "base" (the stored device UUIDs) or "derived" (computed per ingress, see UUIDFor).
	UUIDs string `json:"uuids"`
	// Node is the tailnet name of the machine this bridge runs on, when that differs from Name. Optional.
	Node string `json:"node,omitempty"`
	// Intermittent says the bridge is expected to come and go: an unreachable agent is a notice, not a risk. Optional.
	Intermittent bool `json:"intermittent,omitempty"`
	// Hy2Port is the public UDP port of the bridge's Hysteria 2 inbound (#110). 0: the bridge has none.
	Hy2Port int `json:"hy2_port,omitempty"`
	// Hy2Obfs is the Salamander password, from HY2_OBFS_<NAME> (see hy2Var). Never in config, info or fingerprint.
	Hy2Obfs string `json:"-"`
	// TailnetAccess is "off" (the default, "" too), "all" or "grants". With "all", every credential
	// of this bridge may reach the tailnet through the bridge's tailscaled (#132), and the
	// headscale ACL for tag:ingress decides which hosts. With "grants" (#139), only the credentials
	// of devices with a grant may, each grant group through its own tailnet identity. The console
	// can override the value per bridge.
	TailnetAccess string `json:"tailnet_access,omitempty"`
}

// Tailnet access values of a bridge.
const (
	TailnetOff = "off"
	TailnetAll = "all"
)

// GrantsTailnet says whether the bridge gives its clients tailnet access.
func (in Ingress) GrantsTailnet() bool { return in.TailnetAccess == TailnetAll }

// hy2Var is the variable that holds the Salamander password of a bridge.
func hy2Var(name string) string {
	return "HY2_OBFS_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// IngressConfig is the parsed ENROLL_CONFIG: one or more ingresses, the host that serves
// subscription and one-time links, and the neutral label of the import links.
type IngressConfig struct {
	ProfileName string    `json:"profile_name"`
	LinkHost    string    `json:"link_host"`
	Ingresses   []Ingress `json:"ingresses"`
	// Targets are the named tailnet targets of enroll.json (#139), for bridges in grants mode.
	Targets []TailnetTarget `json:"tailnet_targets,omitempty"`

	key []byte // ENROLL_SECRET_KEY, set by WithKey; never serialised
}

const (
	maxIngresses     = 8
	defaultIngress   = "default"
	errMixedShapes   = "ENROLL_CONFIG: use either ingresses or the single-host keys, not both"
	ingressNamePatt  = `^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`
	ingressTokenPatt = `^[A-Z][A-Z0-9_]{0,63}$`
)

var (
	ingressNameRe  = regexp.MustCompile(ingressNamePatt)
	ingressTokenRe = regexp.MustCompile(ingressTokenPatt)
	dnsLabelRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	singleHostKeys = []string{"host", "public_key", "short_id", "sni", "port"}
)

// Single reports whether exactly one ingress is configured.
func (c IngressConfig) Single() bool { return len(c.Ingresses) == 1 }

// Link is the single-host view used for subscription URLs, one-time links and import links: the
// host is the shared link host, everything else comes from the first ingress.
func (c IngressConfig) Link() IngressCfg {
	first := c.Ingresses[0]
	return IngressCfg{Host: c.LinkHost, PublicKey: first.PublicKey, ShortID: first.ShortID,
		SNI: first.SNI, Port: first.Port, ProfileName: c.ProfileName}
}

// ParseIngressConfig reads the ENROLL_CONFIG JSON. With an "ingresses" key it is the list shape;
// without it, the old single-host shape (same validation and errors as ParseIngressCfg) becomes
// one ingress named "default". Errors name a field and an ingress, never key material.
func ParseIngressConfig(data []byte) (IngressConfig, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return IngressConfig{}, errors.New("ENROLL_CONFIG: must be a JSON object")
	}
	list, hasList := raw["ingresses"]
	if !hasList {
		old, err := ParseIngressCfg(data)
		if err != nil {
			return IngressConfig{}, err
		}
		return IngressConfig{ProfileName: old.ProfileName, LinkHost: old.Host, Ingresses: []Ingress{{
			Name: defaultIngress, Host: old.Host, PublicKey: old.PublicKey, ShortID: old.ShortID,
			SNI: old.SNI, Port: old.Port, UUIDs: uuidsBase}}}, nil
	}
	for _, k := range singleHostKeys {
		if _, ok := raw[k]; ok {
			return IngressConfig{}, errors.New(errMixedShapes)
		}
	}
	if err := rejectUnknownKeys(raw, "", newShapeTopKeys); err != nil {
		return IngressConfig{}, err
	}
	cfg := IngressConfig{ProfileName: "tunnel"}
	if v, ok := raw["profile_name"]; ok {
		s, ok := rawString(v)
		if !ok {
			return IngressConfig{}, errors.New("ENROLL_CONFIG: profile_name must be a non-empty string")
		}
		cfg.ProfileName = s
	}
	var items []json.RawMessage
	if err := json.Unmarshal(list, &items); err != nil || items == nil {
		return IngressConfig{}, errors.New("ENROLL_CONFIG: ingresses must be an array of objects")
	}
	if len(items) == 0 {
		return IngressConfig{}, errors.New("ENROLL_CONFIG: at least one bridge is required")
	}
	if len(items) > maxIngresses {
		return IngressConfig{}, fmt.Errorf("ENROLL_CONFIG: at most %d bridges are supported", maxIngresses)
	}
	names := map[string]bool{}
	hosts := map[string]bool{}
	for i, item := range items {
		in, err := parseIngress(i, item)
		if err != nil {
			return IngressConfig{}, err
		}
		if names[in.Name] {
			return IngressConfig{}, fmt.Errorf("ENROLL_CONFIG: bridge %s: duplicate name", in.Name)
		}
		if hosts[in.Host] {
			return IngressConfig{}, fmt.Errorf("ENROLL_CONFIG: bridge %s: duplicate host", in.Name)
		}
		names[in.Name], hosts[in.Host] = true, true
		cfg.Ingresses = append(cfg.Ingresses, in)
	}
	firstBase := ""
	for i := range cfg.Ingresses {
		in := &cfg.Ingresses[i]
		if in.UUIDs == uuidsBase {
			if firstBase != "" {
				return IngressConfig{}, fmt.Errorf("ENROLL_CONFIG: bridge %s: uuids \"base\" is already used by bridge %s (at most one bridge may use the stored credentials)", in.Name, firstBase)
			}
			firstBase = in.Name
		}
	}
	if v, ok := raw["tailnet_targets"]; ok {
		ts, err := parseTailnetTargets(v)
		if err != nil {
			return IngressConfig{}, err
		}
		cfg.Targets = ts
	}
	if v, ok := raw["link_host"]; ok {
		s, ok := rawString(v)
		if !ok || !validDNSName(s) {
			return IngressConfig{}, errors.New("ENROLL_CONFIG: link_host must be a DNS name (lowercase, no scheme, port or path)")
		}
		cfg.LinkHost = s
	} else if len(cfg.Ingresses) == 1 {
		cfg.LinkHost = cfg.Ingresses[0].Host
	} else {
		return IngressConfig{}, errors.New("ENROLL_CONFIG: link_host is required with several bridges")
	}
	return cfg, nil
}

func parseIngress(i int, item json.RawMessage) (Ingress, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(item, &raw); err != nil || raw == nil {
		return Ingress{}, fmt.Errorf("ENROLL_CONFIG: ingresses[%d] must be an object", i)
	}
	name, _ := rawString(raw["name"])
	if !ingressNameRe.MatchString(name) {
		return Ingress{}, fmt.Errorf("ENROLL_CONFIG: ingresses[%d]: name must match %s", i, ingressNamePatt)
	}
	if err := rejectUnknownKeys(raw, name, ingressKeys); err != nil {
		return Ingress{}, err
	}
	fail := func(format string, a ...any) (Ingress, error) {
		return Ingress{}, fmt.Errorf("ENROLL_CONFIG: bridge %s: %s", name, fmt.Sprintf(format, a...))
	}
	in := Ingress{Name: name, Port: 443}
	host, _ := rawString(raw["host"])
	if !validDNSName(host) {
		return fail("host must be a DNS name (lowercase, no scheme, port or path)")
	}
	in.Host = host
	var ok bool
	if in.PublicKey, ok = rawString(raw["public_key"]); !ok {
		return fail("public_key must be a non-empty string")
	}
	if in.ShortID, ok = rawString(raw["short_id"]); !ok {
		return fail("short_id must be a non-empty string")
	}
	in.SNI = host
	if v, present := raw["sni"]; present {
		if in.SNI, ok = rawString(v); !ok {
			return fail("sni must be a non-empty string")
		}
	}
	if v, present := raw["port"]; present {
		n, err := parseIntLiteral(v)
		if err != nil || n < 1 || n > 65535 {
			return fail("port must be an integer 1-65535")
		}
		in.Port = n
	}
	if v, present := raw["agent_url"]; present {
		s, _ := rawString(v)
		u, err := url.Parse(s)
		if s == "" || err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
			return fail("agent_url must be an http(s) URL with a host and no credentials")
		}
		in.AgentURL = s
	}
	if v, present := raw["token_env"]; present {
		s, _ := rawString(v)
		if !ingressTokenRe.MatchString(s) {
			return fail("token_env must match %s", ingressTokenPatt)
		}
		in.TokenEnv = s
	}
	// No implicit default: a missing value would change meaning when entries are added or removed.
	s, _ := rawString(raw["uuids"])
	if s != uuidsBase && s != uuidsDerived {
		return Ingress{}, fmt.Errorf("ENROLL_CONFIG: ingresses[%d] %s: uuids must be %q or %q", i, name, uuidsBase, uuidsDerived)
	}
	in.UUIDs = s
	if v, present := raw["node"]; present {
		if in.Node, ok = rawString(v); !ok || !validDNSName(in.Node) {
			return fail("node must be a tailnet name (lowercase letters, digits, hyphens and dots)")
		}
	}
	if v, present := raw["hy2_port"]; present {
		n, err := parseIntLiteral(v)
		if err != nil || n < 1 || n > 65535 {
			return fail("hy2_port must be an integer 1-65535")
		}
		in.Hy2Port = n
	}
	if v, present := raw["tailnet_access"]; present {
		if in.TailnetAccess, ok = rawString(v); !ok || !ValidTailnetMode(in.TailnetAccess) {
			return fail("tailnet_access must be \"off\", \"all\" or \"grants\"")
		}
	}
	if v, present := raw["intermittent"]; present {
		if json.Unmarshal(v, &in.Intermittent) != nil || string(v) == "null" {
			return fail("intermittent must be true or false")
		}
	}
	return in, nil
}

var (
	newShapeTopKeys = []string{"profile_name", "link_host", "ingresses", "tailnet_targets"}
	ingressKeys     = []string{"name", "host", "public_key", "short_id", "sni", "port", "agent_url", "token_env", "uuids", "node", "intermittent", "hy2_port", "tailnet_access"}
	keyNameRe       = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,40}$`)
)

// rejectUnknownKeys fails on a key outside allowed, so a misspelt option is never silently
// ignored. The key name is echoed only when it looks like a name (never an arbitrary string).
func rejectUnknownKeys(raw map[string]json.RawMessage, ingress string, allowed []string) error {
	ok := map[string]bool{}
	for _, k := range allowed {
		ok[k] = true
	}
	var bad []string
	for k := range raw {
		if !ok[k] {
			bad = append(bad, k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	k := bad[0]
	if !keyNameRe.MatchString(k) {
		k = "(a key with an unusual name)"
	}
	if ingress == "" {
		return fmt.Errorf("ENROLL_CONFIG: unknown key %q", k)
	}
	return fmt.Errorf("ENROLL_CONFIG: bridge %s: unknown key %q", ingress, k)
}

// rawString decodes a non-empty JSON string; anything else (absent, null, number, "") is !ok.
func rawString(v json.RawMessage) (string, bool) {
	if len(v) == 0 || v[0] != '"' {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil || s == "" {
		return "", false
	}
	return s, true
}

// validDNSName accepts lowercase letters, digits, hyphens and dots only: each label 1-63 chars,
// no leading or trailing hyphen, at most 253 characters in total.
func validDNSName(s string) bool {
	if s == "" || len(s) > 253 {
		return false
	}
	for _, l := range strings.Split(s, ".") {
		if !dnsLabelRe.MatchString(l) {
			return false
		}
	}
	return true
}
