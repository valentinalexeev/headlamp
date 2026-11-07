package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// InboundTag is the VLESS Reality inbound the base config must have.
	InboundTag = "reality-in"
	// HY2Tag is the optional Hysteria 2 inbound (#110). When the base has it, every credential is a user there
	// too: the uuid is the Hysteria auth and the label is the same, so routing rules and stats cover both.
	HY2Tag = "hy2-in"
	flow   = "xtls-rprx-vision"
)

var (
	blockedIPs = []any{"geoip:private", "100.64.0.0/10"}
	listEntry  = regexp.MustCompile(`^[A-Za-z0-9!@._-]{1,64}$`)
)

type obj = map[string]any

// Render builds the live xray config from the base and the credentials. directGeoIP and directGeoSite are geodata
// list names whose destinations leave from this host in auto mode. The base is not changed.
func Render(base []byte, creds []Cred, directGeoIP, directGeoSite []string) ([]byte, error) {
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

	users := func(mode string) []any {
		var out []any
		for _, c := range creds {
			if c.Mode == mode {
				out = append(out, EmailFor(c.UUID))
			}
		}
		return out
	}
	routing["domainStrategy"] = "IPOnDemand" // domain destinations must resolve before the block rule
	rules := []any{obj{"type": "field", "inboundTag": tags, "ip": blockedIPs, "outboundTag": "blocked"}}
	if u := users("direct"); u != nil {
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": "direct"})
	}
	if u := users("exit"); u != nil {
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": "via-exit"})
	}
	if u := users("auto"); u != nil {
		if len(directGeoIP) > 0 {
			rules = append(rules, obj{"type": "field", "user": u, "ip": prefixed("geoip:", directGeoIP), "outboundTag": "direct"})
		}
		if len(directGeoSite) > 0 {
			rules = append(rules, obj{"type": "field", "user": u, "domain": prefixed("geosite:", directGeoSite), "outboundTag": "direct"})
		}
		rules = append(rules, obj{"type": "field", "user": u, "outboundTag": "via-exit"})
	}
	if old, ok := routing["rules"].([]any); ok {
		rules = append(rules, old...)
	}
	routing["rules"] = rules
	return json.Marshal(cfg)
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
