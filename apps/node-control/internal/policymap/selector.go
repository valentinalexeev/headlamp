package policymap

import (
	"encoding/json"
	"net/netip"
	"strconv"
	"strings"
)

const portMax = 65535

// classify returns the kind of a selector. hosts are the names of the "hosts" section.
func classify(id string, hosts map[string]string) Kind {
	switch {
	case id == "*":
		return KindAny
	case strings.HasPrefix(id, "group:"):
		return KindGroup
	case strings.HasPrefix(id, "tag:"):
		return KindTag
	case id == "autogroup:internet":
		return KindInternet
	case strings.HasPrefix(id, "autogroup:"):
		return KindAutogroup
	}
	if _, ok := hosts[id]; ok {
		return KindHost
	}
	if strings.Contains(id, "@") {
		return KindUser
	}
	if isAddress(id) {
		return KindCIDR
	}
	return KindUnknown
}

func label(id string, kind Kind) string {
	switch kind {
	case KindAny:
		return "any"
	case KindInternet:
		return "internet (exit node)"
	}
	return id
}

func isAddress(s string) bool {
	if _, err := netip.ParsePrefix(s); err == nil {
		return true
	}
	_, err := netip.ParseAddr(s)
	return err == nil
}

// splitDestination splits an ACL destination "alias:ports": "tag:web:80,443", "[fd7a::1]:22",
// "*:*".
// ok is false when the ports part is missing or not valid; alias is then the whole text.
func splitDestination(dst string) (alias, ports string, ok bool) {
	if strings.HasPrefix(dst, "[") {
		end := strings.Index(dst, "]:")
		if end < 0 || !validPorts(dst[end+2:]) {
			return dst, "", false
		}
		return dst[1:end], dst[end+2:], true
	}
	i := strings.LastIndex(dst, ":")
	// An alias that ends with ":" is an IPv6 address without brackets: "fd7a::1" is not
	// "fd7a:" with port 1.
	if i < 0 || strings.HasSuffix(dst[:i], ":") || !validPorts(dst[i+1:]) {
		return dst, "", false
	}
	return dst[:i], dst[i+1:], true
}

// validPorts accepts "*" or a comma list of ports and port ranges ("22", "80,443", "1000-2000").
func validPorts(ports string) bool {
	if ports == "*" {
		return true
	}
	if ports == "" {
		return false
	}
	for _, part := range strings.Split(ports, ",") {
		low, high, isRange := strings.Cut(part, "-")
		if !validPort(low) {
			return false
		}
		if isRange && (!validPort(high) || portNumber(low) > portNumber(high)) {
			return false
		}
	}
	return true
}

// portNumber returns the number of a port that validPort accepted.
func portNumber(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return -1 // not reached: the caller checked s with validPort
	}
	return n
}

func validPort(s string) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= 0 && n <= portMax && s == strconv.Itoa(n)
}

// protoString returns the "proto" of an ACL rule, which is a name ("tcp") or a number ("6").
func protoString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var name string
	if err := json.Unmarshal(raw, &name); err == nil {
		return name
	}
	return strings.TrimSpace(string(raw))
}

// access is one protocol and ports, or one set of apps, of a grant.
type access struct {
	proto, ports string
	apps         []string
}

// grantAccesses returns the accesses of a grant: one for each "ip" entry ("*", "tcp:443", "443",
// "icmp"), and one for its "app" names.
func grantAccesses(rule grantRule) []access {
	out := make([]access, 0, len(rule.IP)+1)
	for _, entry := range distinct(rule.IP) {
		out = append(out, ipAccess(entry))
	}
	if len(rule.App) > 0 {
		out = append(out, access{apps: sortedKeys(rule.App)})
	}
	return out
}

func ipAccess(entry string) access {
	if entry == "*" {
		return access{ports: "*"}
	}
	if proto, ports, ok := strings.Cut(entry, ":"); ok {
		return access{proto: proto, ports: ports}
	}
	if validPorts(entry) {
		return access{ports: entry}
	}
	return access{proto: entry}
}
