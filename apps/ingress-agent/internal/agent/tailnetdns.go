package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// TailnetDNS is the tailnet resolver of the bridge (#286): node-control's DNS server on the tailnet and the names
// it answers, as xray domain matchers ("full:<name>" or "domain:<suffix>"). Render puts it first in xray's dns
// section, for these names only, with skipFallback: the public resolver never sees them, and a lookup gives a
// tailnet address, so the tailnet IP rule of a granted client matches. The answer must be a tailnet address
// (expectedIPs), and a failed lookup does not go to another server (finalQuery).
type TailnetDNS struct {
	Server  string   `json:"server"`
	Domains []string `json:"domains"`
}

const maxTailnetDomains = 1024

var (
	tailnetDNSServerRe = regexp.MustCompile(`^tcp://([0-9]{1,3}(\.[0-9]{1,3}){3}):([0-9]{1,5})$`)
	// domainMatcherRe is "full:" or "domain:" and a lower-case DNS name with at least two labels.
	domainMatcherRe = regexp.MustCompile(
		`^(full|domain):([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	tailnet4 = netip.MustParsePrefix("100.64.0.0/10")
)

// Validate checks the server and the names. The error never quotes a name.
func (t *TailnetDNS) Validate() error {
	if t == nil {
		return nil
	}
	m := tailnetDNSServerRe.FindStringSubmatch(t.Server)
	if m == nil {
		return errors.New("server must be tcp://<tailnet ipv4>:<port>")
	}
	ip, err := netip.ParseAddr(m[1])
	port, perr := strconv.Atoi(m[3])
	if err != nil || !tailnet4.Contains(ip) || perr != nil || port < 1 || port > 65535 {
		return errors.New("server must be tcp://<tailnet ipv4>:<port>")
	}
	if len(t.Domains) == 0 || len(t.Domains) > maxTailnetDomains {
		return fmt.Errorf("domains must have 1-%d entries", maxTailnetDomains)
	}
	for _, d := range t.Domains {
		if len(d) > 7+253 || !domainMatcherRe.MatchString(d) {
			return errors.New("a domain is not full:<dns name> or domain:<dns name>")
		}
	}
	return nil
}

// normalized is a copy with the names sorted and deduplicated; nil stays nil.
func (t *TailnetDNS) normalized() *TailnetDNS {
	if t == nil {
		return nil
	}
	d := slices.Clone(t.Domains)
	slices.Sort(d)
	return &TailnetDNS{Server: t.Server, Domains: slices.Compact(d)}
}

func (t *TailnetDNS) equal(o *TailnetDNS) bool {
	a, b := t.normalized(), o.normalized()
	if a == nil || b == nil {
		return a == b
	}
	return a.Server == b.Server && slices.Equal(a.Domains, b.Domains)
}

// LoadTailnetDNS reads the persisted resolver; a missing file is none.
func LoadTailnetDNS(path string) (*TailnetDNS, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var t TailnetDNS
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("tailnet DNS file: %w", err)
	}
	if err := t.Validate(); err != nil {
		return nil, fmt.Errorf("tailnet DNS file: %w", err)
	}
	return t.normalized(), nil
}

// SaveTailnetDNS writes the resolver atomically with mode 0600. nil removes the file.
func SaveTailnetDNS(path string, t *TailnetDNS) error {
	if t == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	b, _ := json.Marshal(t.normalized())
	return WriteAtomic(path, b, 0o600, "")
}

// parseTailnetDNS reads the optional "tailnet_dns" key of a PUT body. Absent or null is none (a node-control
// before #286, or a bridge without tailnet access). Any problem is false.
func parseTailnetDNS(body []byte) (*TailnetDNS, bool) {
	var raw struct {
		DNS json.RawMessage `json:"tailnet_dns"`
	}
	if json.Unmarshal(body, &raw) != nil {
		return nil, false
	}
	if len(raw.DNS) == 0 || strings.TrimSpace(string(raw.DNS)) == "null" {
		return nil, true
	}
	var t TailnetDNS
	dec := json.NewDecoder(strings.NewReader(string(raw.DNS)))
	dec.DisallowUnknownFields()
	if dec.Decode(&t) != nil || t.Validate() != nil {
		return nil, false
	}
	return t.normalized(), true
}
