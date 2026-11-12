package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"
)

// Selection is how a device profile chooses between bridges (#313). Every field is optional. A
// profile without these options is the one node-control always produced.
type Selection struct {
	// Tolerance is the latency difference, in ms, that makes the client move to a faster bridge.
	// nil: the built-in default. 0 is a valid value: always the fastest.
	Tolerance *int
	// Interval is how often the client tests the bridges.
	Interval time.Duration
	// URL is the address the client tests each bridge with.
	URL string
	// Strategy is the Clash/mihomo group type: "" (url-test), "round-robin" or
	// "consistent-hashing" (load-balance). sing-box has no such strategy: it always uses urltest.
	Strategy string
}

// Bounds of the options, so one bad value cannot make every phone test all the time or never.
const (
	minTolerance = 0
	maxTolerance = 1000
	minInterval  = 30 * time.Second
	maxInterval  = 24 * time.Hour
	maxURLLen    = 200

	strategyRoundRobin     = "round-robin"
	strategyConsistentHash = "consistent-hashing"
)

var selectionKeys = []string{"tolerance", "interval", "url", "strategy"}

// tolerance, interval and url give the effective values: the option, or the built-in default.
func (s Selection) tolerance() int {
	if s.Tolerance != nil {
		return *s.Tolerance
	}
	return probeTolerance
}

func (s Selection) intervalText() string {
	if s.Interval > 0 {
		return durationText(s.Interval)
	}
	return probeInterval
}

func (s Selection) intervalSeconds() int {
	if s.Interval > 0 {
		return int(s.Interval / time.Second)
	}
	return probeIntervalS
}

func (s Selection) url() string {
	if s.URL != "" {
		return s.URL
	}
	return probeURL
}

// durationText writes d the way sing-box reads it, in whole seconds, minutes or hours.
func durationText(d time.Duration) string {
	switch {
	case d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return fmt.Sprintf("%ds", d/time.Second)
	}
}

// parseSelection reads the optional "urltest" object of ENROLL_CONFIG. Errors name the field only.
func parseSelection(v json.RawMessage) (Selection, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(v, &raw); err != nil || raw == nil {
		return Selection{}, errors.New("ENROLL_CONFIG: urltest must be an object")
	}
	for k := range raw {
		if !slices.Contains(selectionKeys, k) {
			return Selection{}, fmt.Errorf("ENROLL_CONFIG: urltest: unknown key %q", k)
		}
	}
	var s Selection
	if v, ok := raw["tolerance"]; ok {
		n, err := parseIntLiteral(v)
		if err != nil || n < minTolerance || n > maxTolerance {
			return Selection{}, fmt.Errorf(
				"ENROLL_CONFIG: urltest.tolerance must be an integer %d-%d (ms)",
				minTolerance, maxTolerance)
		}
		s.Tolerance = &n
	}
	if v, ok := raw["interval"]; ok {
		str, _ := rawString(v)
		d, err := time.ParseDuration(str)
		if err != nil || d < minInterval || d > maxInterval || d%time.Second != 0 {
			return Selection{}, fmt.Errorf(
				"ENROLL_CONFIG: urltest.interval must be a duration from %s to %s, "+
					"in whole seconds (for example \"3m\")",
				durationText(minInterval), durationText(maxInterval))
		}
		s.Interval = d
	}
	if v, ok := raw["url"]; ok {
		str, _ := rawString(v)
		u, err := url.Parse(str)
		badScheme := err != nil || (u.Scheme != "http" && u.Scheme != "https")
		if str == "" || len(str) > maxURLLen || badScheme || u.Hostname() == "" || u.User != nil ||
			strings.ContainsAny(str, " \"'\\") {
			return Selection{}, errors.New("ENROLL_CONFIG: urltest.url must be an http(s) URL " +
				"with a host, no credentials and no quotes")
		}
		s.URL = str
	}
	if v, ok := raw["strategy"]; ok {
		str, _ := rawString(v)
		if str != strategyRoundRobin && str != strategyConsistentHash {
			return Selection{}, fmt.Errorf("ENROLL_CONFIG: urltest.strategy must be %q or %q",
				strategyRoundRobin, strategyConsistentHash)
		}
		s.Strategy = str
	}
	return s, nil
}

// groups returns the bridge groups in the order of their first bridge, and false when no bridge
// has a group (then the profile has one flat group per mode, as before).
func (c IngressConfig) groups() ([]string, bool) {
	var out []string
	seen := map[string]bool{}
	grouped := false
	for _, i := range c.Ingresses {
		if i.Group != "" {
			grouped = true
		}
		g := i.groupName()
		if !seen[g] {
			seen[g] = true
			out = append(out, g)
		}
	}
	return out, grouped
}

// defaultGroup holds the bridges without a group, when other bridges have one.
const defaultGroup = "default"

func (i Ingress) groupName() string {
	if i.Group != "" {
		return i.Group
	}
	return defaultGroup
}

// groupTag is the tag of one mode's group of bridges. The "@" cannot be in a bridge name, so
// the tag never equals the tag of one bridge (mode + "-" + name).
func groupTag(mode, group string) string { return mode + "@" + group }

// groupMembers are the outbound tags of one mode in one group.
func groupMembers(cfg IngressConfig, m, g string) []string {
	var out []string
	for _, i := range cfg.Ingresses {
		if i.groupName() != g {
			continue
		}
		out = append(out, outboundTag(cfg, m, i))
		if i.Hy2Port != 0 {
			out = append(out, hy2Tag(m, i))
		}
	}
	return out
}

// singBoxGroups are the urltest outbounds of a profile with several outbounds per mode. Without
// groups: one per mode over every outbound of that mode, as before. With groups: one per mode and
// group, and one per mode over those groups, so the client keeps the fastest group and, in it,
// the fastest bridge.
func singBoxGroups(cfg IngressConfig) []any {
	s := cfg.Selection
	urltest := func(tag string, members []string) obj {
		return obj{{"type", "urltest"}, {"tag", tag}, {"outbounds", strs(members)},
			{"url", s.url()}, {"interval", s.intervalText()}, {"tolerance", s.tolerance()}}
	}
	gs, grouped := cfg.groups()
	var out []any
	for _, m := range Modes {
		if !grouped {
			out = append(out, urltest(m, members(cfg, m)))
			continue
		}
		tags := make([]string, len(gs))
		for i, g := range gs {
			tags[i] = groupTag(m, g)
		}
		out = append(out, urltest(m, tags))
		for _, g := range gs {
			out = append(out, urltest(groupTag(m, g), groupMembers(cfg, m, g)))
		}
	}
	return out
}

// clashGroups are the same groups for Clash/mihomo. With a strategy, each group of bridges is a
// load-balance group; the top-level group over the groups stays url-test.
func clashGroups(cfg IngressConfig) []any {
	s := cfg.Selection
	urltest := func(name string, proxies []string) obj {
		return obj{{"name", name}, {"type", "url-test"}, {"proxies", strs(proxies)},
			{"url", s.url()}, {"interval", s.intervalSeconds()}, {"tolerance", s.tolerance()}}
	}
	leaf := func(name string, proxies []string) obj {
		if s.Strategy == "" {
			return urltest(name, proxies)
		}
		return obj{{"name", name}, {"type", "load-balance"}, {"proxies", strs(proxies)},
			{"url", s.url()}, {"interval", s.intervalSeconds()}, {"strategy", s.Strategy}}
	}
	gs, grouped := cfg.groups()
	var out []any
	for _, m := range Modes {
		if !grouped {
			out = append(out, leaf(m, members(cfg, m)))
			continue
		}
		names := make([]string, len(gs))
		for i, g := range gs {
			names[i] = groupTag(m, g)
		}
		out = append(out, urltest(m, names))
		for _, g := range gs {
			out = append(out, leaf(groupTag(m, g), groupMembers(cfg, m, g)))
		}
	}
	return out
}
