package enroll

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Rendering of the client enrolment profiles and import links. Nothing here carries the device
// name or owner: the deep link and the profile both pass through the ingress host, which must not
// learn who the clients are. The deep link label is the neutral IngressCfg.ProfileName.
//
// Output is byte-for-byte what the Python service produced: same key order, same separators, same
// string escaping. The sing-box profile is compact JSON with raw non-ASCII; the Clash profile is
// json.dumps defaults (", " and ": " separators, ASCII-only escapes). Neither has a trailing newline.

// IngressCfg is the ENROLL_CONFIG file: where the clients connect and the Reality parameters.
type IngressCfg struct {
	Host        string
	PublicKey   string
	ShortID     string
	SNI         string
	Port        int    // default 443
	ProfileName string // default "tunnel"
}

// ParseIngressCfg builds an IngressCfg from the ENROLL_CONFIG JSON, applying defaults and the
// same validation (and error text) as the Python cfg_from_dict.
func ParseIngressCfg(data []byte) (IngressCfg, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil || raw == nil {
		return IngressCfg{}, errors.New("ENROLL_CONFIG: must be a JSON object")
	}
	str := func(k string) (string, bool) {
		v, ok := raw[k]
		if !ok || len(v) == 0 || v[0] != '"' {
			return "", false
		}
		var s string
		if json.Unmarshal(v, &s) != nil || s == "" {
			return "", false
		}
		return s, true
	}
	var missing []string
	vals := map[string]string{}
	for _, k := range []string{"host", "public_key", "short_id", "sni"} {
		s, ok := str(k)
		if !ok {
			missing = append(missing, k)
		}
		vals[k] = s
	}
	if len(missing) > 0 {
		return IngressCfg{}, fmt.Errorf("ENROLL_CONFIG: missing or empty: %s", strings.Join(missing, ", "))
	}
	port := 443
	if v, ok := raw["port"]; ok {
		n, err := parseIntLiteral(v)
		if err != nil || n < 1 || n > 65535 {
			return IngressCfg{}, errors.New("ENROLL_CONFIG: port must be an integer 1-65535")
		}
		port = n
	}
	name := "tunnel"
	if _, ok := raw["profile_name"]; ok {
		s, ok := str("profile_name")
		if !ok {
			return IngressCfg{}, errors.New("ENROLL_CONFIG: profile_name must be a non-empty string")
		}
		name = s
	}
	return IngressCfg{Host: vals["host"], PublicKey: vals["public_key"], ShortID: vals["short_id"],
		SNI: vals["sni"], Port: port, ProfileName: name}, nil
}

// parseIntLiteral accepts only a JSON integer literal (no fraction, exponent, string or bool).
func parseIntLiteral(v json.RawMessage) (int, error) {
	s := string(bytes.TrimSpace(v))
	digits := strings.TrimPrefix(s, "-")
	if digits == "" || strings.Trim(digits, "0123456789") != "" {
		return 0, errors.New("not an integer")
	}
	n, err := strconv.ParseInt(s, 10, 32)
	return int(n), err
}

// SubURL is the subscription URL a device imports.
func SubURL(cfg IngressCfg, secret string) string {
	return "https://" + cfg.Host + "/sub/" + secret
}

// DeepLinkFor builds the import link of one client ("sing-box", "karing" or "clash") for a
// subscription URL and a label. An unknown client is an error.
func DeepLinkFor(client, subURL, name string) (string, error) {
	url := quote(subURL)
	label := quote(name)
	switch client {
	case "sing-box":
		return "sing-box://import-remote-profile?url=" + url + "#" + label, nil
	case "karing":
		return "karing://install-config?url=" + url + "&name=" + label, nil
	case "clash":
		return "clash://install-config?url=" + url + "&name=" + label, nil
	}
	return "", fmt.Errorf("unknown client: %q", client)
}

// SingBoxDeepLink is the sing-box import link for the device's subscription secret.
func SingBoxDeepLink(cfg IngressCfg, secret string) string {
	s, _ := DeepLinkFor("sing-box", SubURL(cfg, secret), cfg.ProfileName)
	return s
}

// KaringDeepLink is the Karing import link for the device's subscription secret.
func KaringDeepLink(cfg IngressCfg, secret string) string {
	s, _ := DeepLinkFor("karing", SubURL(cfg, secret), cfg.ProfileName)
	return s
}

// ClashDeepLink is the Clash import link for the device's subscription secret.
func ClashDeepLink(cfg IngressCfg, secret string) string {
	s, _ := DeepLinkFor("clash", SubURL(cfg, secret), cfg.ProfileName)
	return s
}

// DeepLink is the default (sing-box) import link for the device's subscription secret.
func DeepLink(cfg IngressCfg, secret string) string { return SingBoxDeepLink(cfg, secret) }

// quote matches Python urllib.parse.quote(s, safe=""): everything but the unreserved set is
// percent-encoded byte-wise (uppercase hex), a space becomes %20.
func quote(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' ||
			c == '_' || c == '.' || c == '-' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// Ordered JSON building blocks: obj keeps insertion order, as Python dicts do.
type kv struct {
	k string
	v any
}
type obj []kv

var (
	tunAddresses = []string{"172.19.0.1/30", "fdfe:dcba:9876::1/126"}
	dnsBlock     = func() obj {
		return obj{
			{"servers", []any{
				obj{{"type", "tls"}, {"tag", "remote"}, {"server", "1.1.1.1"}, {"detour", "select"}},
				obj{{"type", "local"}, {"tag", "local"}},
			}},
			{"final", "remote"},
			{"strategy", "prefer_ipv4"},
		}
	}
)

func checkModes(d Device) error {
	var missing []string
	for _, m := range Modes {
		if d.UUIDs[m] == "" {
			missing = append(missing, m)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("device lacks uuids for modes: %s", strings.Join(missing, ", "))
	}
	return nil
}

func strs(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}

// RenderProfile renders the sing-box profile for a device: compact JSON, raw UTF-8.
func RenderProfile(cfg IngressCfg, d Device) ([]byte, error) {
	if err := checkModes(d); err != nil {
		return nil, err
	}
	outbounds := []any{
		obj{{"type", "selector"}, {"tag", "select"}, {"outbounds", strs(Modes)}, {"default", "auto"}},
	}
	for _, m := range Modes {
		outbounds = append(outbounds, obj{
			{"type", "vless"}, {"tag", m}, {"server", cfg.Host}, {"server_port", cfg.Port},
			{"uuid", d.UUIDs[m]}, {"flow", "xtls-rprx-vision"},
			{"tls", obj{
				{"enabled", true},
				{"server_name", cfg.SNI},
				{"utls", obj{{"enabled", true}, {"fingerprint", "chrome"}}},
				{"reality", obj{{"enabled", true}, {"public_key", cfg.PublicKey}, {"short_id", cfg.ShortID}}},
			}},
		})
	}
	outbounds = append(outbounds, obj{{"type", "direct"}, {"tag", "plain"}})
	p := obj{
		{"log", obj{{"level", "warn"}}},
		{"inbounds", []any{
			obj{{"type", "tun"}, {"address", strs(tunAddresses)}, {"auto_route", true}, {"strict_route", true}},
		}},
		{"dns", dnsBlock()},
		{"outbounds", outbounds},
		{"route", obj{
			{"rules", []any{
				obj{{"action", "sniff"}},
				obj{{"protocol", "dns"}, {"action", "hijack-dns"}},
			}},
			{"final", "select"},
			{"auto_detect_interface", true},
			{"default_domain_resolver", obj{{"server", "local"}, {"strategy", "prefer_ipv4"}}},
		}},
	}
	return encodeJSON(p, ",", ":", false), nil
}

// RenderClashProfile renders the same three modes as a Clash/mihomo profile (JSON, valid YAML).
func RenderClashProfile(cfg IngressCfg, d Device) ([]byte, error) {
	if err := checkModes(d); err != nil {
		return nil, err
	}
	var proxies []any
	for _, m := range Modes {
		proxies = append(proxies, obj{
			{"name", m}, {"type", "vless"}, {"server", cfg.Host}, {"port", cfg.Port}, {"uuid", d.UUIDs[m]},
			{"network", "tcp"}, {"tls", true}, {"udp", true}, {"flow", "xtls-rprx-vision"},
			{"servername", cfg.SNI}, {"client-fingerprint", "chrome"},
			{"reality-opts", obj{
				{"public-key", cfg.PublicKey}, {"short-id", cfg.ShortID}, {"support-x25519mlkem768", true},
			}},
		})
	}
	p := obj{
		{"proxies", proxies},
		{"proxy-groups", []any{obj{{"name", "select"}, {"type", "select"}, {"proxies", strs(Modes)}}}},
		{"rules", []any{"MATCH,select"}},
		{"dns", obj{
			{"enable", true}, {"ipv6", false},
			{"nameserver", []any{"tls://1.1.1.1#select"}},
			{"proxy-server-nameserver", []any{"system"}},
		}},
	}
	return encodeJSON(p, ", ", ": ", true), nil
}

// encodeJSON writes v the way Python's json.dumps does for the given separators and ensure_ascii.
func encodeJSON(v any, itemSep, keySep string, ascii bool) []byte {
	var b bytes.Buffer
	var w func(v any)
	w = func(v any) {
		switch x := v.(type) {
		case obj:
			b.WriteByte('{')
			for i, e := range x {
				if i > 0 {
					b.WriteString(itemSep)
				}
				writeString(&b, e.k, ascii)
				b.WriteString(keySep)
				w(e.v)
			}
			b.WriteByte('}')
		case []any:
			b.WriteByte('[')
			for i, e := range x {
				if i > 0 {
					b.WriteString(itemSep)
				}
				w(e)
			}
			b.WriteByte(']')
		case string:
			writeString(&b, x, ascii)
		case int:
			b.WriteString(strconv.Itoa(x))
		case bool:
			b.WriteString(strconv.FormatBool(x))
		default:
			panic(fmt.Sprintf("encodeJSON: unsupported %T", v))
		}
	}
	w(v)
	return b.Bytes()
}

// writeString mirrors Python's json string encoding: ensure_ascii escapes everything outside
// 0x20-0x7e (surrogate pairs above the BMP); otherwise only quote, backslash and control chars.
func writeString(b *bytes.Buffer, s string, ascii bool) {
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r < 0x20:
			fmt.Fprintf(b, `\u%04x`, r)
		case !ascii || (r >= 0x20 && r <= 0x7e):
			b.WriteRune(r)
		case r > 0xffff:
			r -= 0x10000
			fmt.Fprintf(b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(b, `\u%04x`, r)
		}
	}
	b.WriteByte('"')
}
