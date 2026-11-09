package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	// InboundTag is the VLESS Reality inbound the base config must have.
	InboundTag = "reality-in"
	// TailnetOut is the outbound for granted tailnet traffic (#132): a SOCKS outbound to the bridge's own
	// tailscaled, which routes tailnet addresses itself (the headscale ACL applies). Needed only when a credential
	// has a tailnet grant.
	TailnetOut = "via-tailnet"
	// HY2Tag is the optional Hysteria 2 inbound (#110). When the base has it, every credential is a user there
	// too: the uuid is the Hysteria auth and the label is the same, so routing rules and stats cover both.
	HY2Tag = "hy2-in"
	flow   = "xtls-rprx-vision"
	// DNSTag is the routing inbound tag of xray's own DNS queries. Render routes it to
	// via-exit, so the resolver of this host's network does not see the names clients visit.
	DNSTag = "dns-internal"
	// ExitOut is the outbound to the exit node (the tailscaled SOCKS).
	ExitOut = "via-exit"
)

var (
	blockedIPs = []any{"geoip:private", "100.64.0.0/10"}
	// tailnetIPs are the headscale address ranges a granted client may reach through via-tailnet.
	tailnetIPs = []any{"100.64.0.0/10", "fd7a:115c:a1e0::/48"}
	listEntry  = regexp.MustCompile(`^[A-Za-z0-9!@._-]{1,64}$`)
	// DefaultExitDNS are the resolvers xray queries through the exit node when
	// INGRESS_EXIT_DNS is empty.
	DefaultExitDNS = []string{"https://1.1.1.1/dns-query", "https://1.0.0.1/dns-query"}
	// exitDNSEntry admits DNS-over-HTTPS or DNS-over-TCP to an IPv4 literal: both run over
	// TCP, so they pass the SOCKS outbound, and a literal needs no lookup of its own (that
	// lookup would leave locally).
	exitDNSEntry = regexp.MustCompile(
		`^(https://` + ipv4Port + `/[A-Za-z0-9._/-]*|tcp://` + ipv4Port + `)$`)
)

// ipv4Port is a dotted IPv4 literal with an optional port, for exitDNSEntry.
const ipv4Port = `[0-9]{1,3}(\.[0-9]{1,3}){3}(:[0-9]{1,5})?`

type obj = map[string]any

// Render builds the live xray config from the base and the credentials. directGeoIP and
// directGeoSite are geodata list names whose destinations leave from this host in auto mode.
// exitDNS are the
// resolvers xray uses for its own lookups, reached through via-exit (empty: DefaultExitDNS).
// The base is not changed.
func Render(base []byte, creds []Cred,
	directGeoIP, directGeoSite, exitDNS []string) ([]byte, error) {
	for _, e := range append(append([]string{}, directGeoIP...), directGeoSite...) {
		if !listEntry.MatchString(e) {
			return nil, fmt.Errorf("invalid direct list entry: %q", e)
		}
	}
	var cfg obj
	if err := json.Unmarshal(base, &cfg); err != nil {
		return nil, fmt.Errorf("base config: %w", err)
	}
	inbounds, _ := cfg["inbounds"].([]any)
	find := func(tag string) obj {
		for _, i := range inbounds {
			if m, ok := i.(obj); ok && m["tag"] == tag {
				return m
			}
		}
		return nil
	}
	in := find(InboundTag)
	if in == nil {
		return nil, fmt.Errorf("base config has no inbound tagged %s", InboundTag)
	}
	routing, ok := cfg["routing"].(obj)
	if !ok {
		return nil, errors.New("base config has no routing section")
	}
	addClients(in, func(c Cred) obj { return obj{"id": c.UUID, "email": EmailFor(c.UUID), "flow": flow} }, creds)
	tags := []any{InboundTag}
	if hy := find(HY2Tag); hy != nil {
		if hy["protocol"] != "hysteria" {
			return nil, fmt.Errorf("inbound %s must use the hysteria protocol", HY2Tag)
		}
		addClients(hy, func(c Cred) obj { return obj{"auth": c.UUID, "email": EmailFor(c.UUID)} }, creds)
		tags = append(tags, HY2Tag)
	}

	// Domain destinations must resolve before the block rule.
	routing["domainStrategy"] = "IPOnDemand"
	dnsRule, err := setDNS(cfg, directGeoSite, exitDNS)
	if err != nil {
		return nil, err
	}
	rules := []any{dnsRule}
	// Granted tailnet traffic goes first, before the block rule, and only to the tailnet
	// ranges: private ranges stay blocked for everyone, and so does the tailnet for every
	// other credential.
	tailnetRules, err := tailnetRules(cfg, creds, tags)
	if err != nil {
		return nil, err
	}
	rules = append(rules, tailnetRules...)
	rules = append(rules, obj{"type": "field", "inboundTag": tags, "ip": blockedIPs,
		"outboundTag": "blocked"})
	rules = append(rules, modeRules(creds, directGeoIP, directGeoSite)...)
	if old, ok := routing["rules"].([]any); ok {
		rules = append(rules, old...)
	}
	routing["rules"] = rules
	return json.Marshal(cfg)
}

// modeRules sends each credential by its mode: direct, exit, or auto (direct for destinations
// on the direct lists, the exit node for the rest).
func modeRules(creds []Cred, directGeoIP, directGeoSite []string) []any {
	users := func(mode string) []any {
		var out []any
		for _, c := range creds {
			if c.Mode == mode {
				out = append(out, EmailFor(c.UUID))
			}
		}
		return out
	}
	var rules []any
	if u := users("direct"); u != nil {
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": "direct"})
	}
	if u := users("exit"); u != nil {
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": ExitOut})
	}
	if u := users("auto"); u != nil {
		if len(directGeoIP) > 0 {
			rules = append(rules, obj{"type": "field", "user": u,
				"ip": prefixed("geoip:", directGeoIP), "outboundTag": "direct"})
		}
		if len(directGeoSite) > 0 {
			rules = append(rules, obj{"type": "field", "user": u,
				"domain": prefixed("geosite:", directGeoSite), "outboundTag": "direct"})
		}
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": ExitOut})
	}
	return rules
}

// setDNS writes xray's resolver config into cfg (replacing any dns section of the base) and
// returns the routing rule that must come first: it sends xray's own queries to via-exit.
// Without it, IPOnDemand looks up every name a client visits with the system resolver of
// this host, in the network the clients want to avoid.
//
// Names on the direct site lists resolve with the system resolver (their traffic leaves from
// this host, so the local answer is the right one); skipFallback keeps every other name off
// it. All other lookups use exitDNS (empty: DefaultExitDNS).
func setDNS(cfg obj, directGeoSite, exitDNS []string) (obj, error) {
	if len(exitDNS) == 0 {
		exitDNS = DefaultExitDNS
	}
	if !hasOutbound(cfg, ExitOut) {
		return nil, fmt.Errorf("base config has no outbound tagged %s", ExitOut)
	}
	var servers []any
	if len(directGeoSite) > 0 {
		servers = append(servers, obj{"address": "localhost",
			"domains": prefixed("geosite:", directGeoSite), "skipFallback": true})
	}
	for _, e := range exitDNS {
		if !exitDNSEntry.MatchString(e) {
			return nil, fmt.Errorf("invalid exit DNS server "+
				"(want https://<ipv4>/<path> or tcp://<ipv4>[:port]): %q", e)
		}
		servers = append(servers, e)
	}
	cfg["dns"] = obj{"tag": DNSTag, "queryStrategy": "UseIPv4", "servers": servers}
	return obj{"type": "field", "inboundTag": []any{DNSTag}, "outboundTag": ExitOut}, nil
}

// tailnetRules gives one rule for the "all" grants (outbound via-tailnet) and one rule per grant
// group (outbound via-tailnet-<id>, #139). A group without its outbound gets no rule: the block
// rule denies it until the bridge has the SOCKS instance of the group (deny by default).
func tailnetRules(cfg obj, creds []Cred, tags []any) ([]any, error) {
	byGrant := map[string][]any{}
	var order []string
	for _, c := range creds {
		if c.Tailnet == "" {
			continue
		}
		if byGrant[c.Tailnet] == nil {
			order = append(order, c.Tailnet)
		}
		byGrant[c.Tailnet] = append(byGrant[c.Tailnet], EmailFor(c.UUID))
	}
	slices.Sort(order)
	var out []any
	for _, g := range order {
		outbound := TailnetOut
		if g != "all" {
			outbound = GroupOutbound(g)
			if !hasOutbound(cfg, outbound) {
				continue
			}
		} else if !hasOutbound(cfg, TailnetOut) {
			return nil, fmt.Errorf("a credential has a tailnet grant, "+
				"but the base config has no outbound tagged %s", TailnetOut)
		}
		rule := obj{"type": "field", "inboundTag": tags, "user": byGrant[g], "ip": tailnetIPs}
		rule["outboundTag"] = outbound
		out = append(out, rule)
	}
	return out, nil
}

// GroupOutbound is the outbound tag of a grant group.
func GroupOutbound(group string) string { return TailnetOut + "-" + group }

// GroupOutbounds lists the grant groups the base config has an outbound for (#139), sorted.
func GroupOutbounds(base []byte) []string {
	var cfg obj
	if json.Unmarshal(base, &cfg) != nil {
		return []string{}
	}
	outs, _ := cfg["outbounds"].([]any)
	res := []string{}
	for _, o := range outs {
		m, _ := o.(obj)
		tag, _ := m["tag"].(string)
		if g := strings.TrimPrefix(tag, TailnetOut+"-"); g != tag && groupIDRe.MatchString(g) {
			res = append(res, g)
		}
	}
	slices.Sort(res)
	return res
}

func hasOutbound(cfg obj, tag string) bool {
	outs, _ := cfg["outbounds"].([]any)
	for _, o := range outs {
		if m, ok := o.(obj); ok && m["tag"] == tag {
			return true
		}
	}
	return false
}

func addClients(in obj, mk func(Cred) obj, creds []Cred) {
	settings, ok := in["settings"].(obj)
	if !ok {
		settings = obj{}
		in["settings"] = settings
	}
	clients, _ := settings["clients"].([]any)
	for _, c := range creds {
		clients = append(clients, mk(c))
	}
	if clients == nil {
		clients = []any{}
	}
	settings["clients"] = clients
}

func prefixed(p string, xs []string) []any {
	out := make([]any, len(xs))
	for i, x := range xs {
		out[i] = p + x
	}
	return out
}

// RunFunc runs a command and returns its exit code and stdout. A non-nil error means it could not be run.
type RunFunc func(ctx context.Context, args []string) (int, string, error)

func execRun(ctx context.Context, args []string) (int, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, args[0], args[1:]...).Output()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), string(out), nil
	}
	if err != nil {
		return -1, "", err
	}
	return 0, string(out), nil
}

// Runner drives the xray binary and its service.
type Runner struct {
	Bin, Unit, API string
	RestartCmd     []string // replaces `systemctl restart <Unit>`, for a host without systemd
	Run            RunFunc
}

func (r *Runner) run(ctx context.Context, args []string) (int, string, error) {
	if r.Run != nil {
		return r.Run(ctx, args)
	}
	return execRun(ctx, args)
}

// TestConfig reports whether xray accepts the config at path.
func (r *Runner) TestConfig(ctx context.Context, path string) bool {
	code, _, err := r.run(ctx, []string{r.Bin, "run", "-test", "-c", path})
	return err == nil && code == 0
}

// Restart restarts xray.
func (r *Runner) Restart(ctx context.Context) error {
	args := r.RestartCmd
	if len(args) == 0 {
		args = []string{"systemctl", "restart", r.Unit}
	}
	code, _, err := r.run(ctx, args)
	if err != nil {
		return err
	}
	if code != 0 {
		return errors.New("xray restart failed")
	}
	return nil
}

// Traffic is the byte counts of one user.
type Traffic struct {
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

// Stats reads the per-user traffic from the xray stats API, keyed by label. Any failure gives an empty map.
func (r *Runner) Stats(ctx context.Context) map[string]Traffic {
	out := map[string]Traffic{}
	code, text, err := r.run(ctx, []string{r.Bin, "api", "statsquery", "-s", r.API, "-pattern", "user>>>"})
	if err != nil || code != 0 {
		return out
	}
	var doc struct {
		Stat []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"stat"`
	}
	if strings.TrimSpace(text) == "" || json.Unmarshal([]byte(text), &doc) != nil {
		return out
	}
	for _, e := range doc.Stat {
		parts := strings.Split(e.Name, ">>>")
		if len(parts) != 4 || parts[0] != "user" {
			continue
		}
		v := strings.Trim(string(e.Value), `"`)
		n, _ := strconv.ParseInt(v, 10, 64)
		t := out[parts[1]]
		switch parts[3] {
		case "uplink":
			t.Up = n
		case "downlink":
			t.Down = n
		default:
			continue
		}
		out[parts[1]] = t
	}
	return out
}
