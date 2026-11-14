package tailnetdns

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/headscale"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/servedns"
)

// Record types (#285). TypeAddr ("A", also given as "" or "AAAA") is the type of #286: fixed
// tailnet addresses, one node, or the online nodes of a tag; it answers A and AAAA queries.
const (
	TypeAddr  = "A"
	TypeCNAME = "CNAME"
	TypeTXT   = "TXT"
	TypeSRV   = "SRV"
)

const (
	maxRules     = 16
	maxSubjects  = 16
	maxTexts     = 8
	maxTextLen   = 255
	maxZones     = 32
	maxLoginLen  = 128
	srvPortLimit = 65535
)

// Target is the data of a record or of a rule. Type says which fields are used: Addresses, Node or
// Tag for an address record; Target for CNAME; Text for TXT; Target, Port, Priority and Weight for
// SRV.
type Target struct {
	Type      string   `json:"type,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	Node      string   `json:"node,omitempty"`
	Tag       string   `json:"tag,omitempty"`
	Target    string   `json:"target,omitempty"`
	Text      []string `json:"text,omitempty"`
	Priority  uint16   `json:"priority,omitempty"`
	Weight    uint16   `json:"weight,omitempty"`
	Port      uint16   `json:"port,omitempty"`
}

// Rule gives other data to some clients (#285): the tailnet logins in Users, and the nodes with a
// tag in Tags. The first rule that matches a client answers it; a client that no rule matches gets
// the data of the record. The data of a rule has the type of its record.
type Rule struct {
	Users []string `json:"users,omitempty"`
	Tags  []string `json:"tags,omitempty"`
	Target
}

// Zone is a DNS zone that node-control is authoritative for (#285): a name under it that has no
// record does not exist (NXDOMAIN), and headscale sends the whole zone to node-control with one
// nameservers.split line.
type Zone struct {
	Name    string    `json:"name"`
	Note    string    `json:"note,omitempty"`
	Created time.Time `json:"created"`
}

func (t Target) empty() bool {
	return !t.hasAddr() && t.Target == "" && len(t.Text) == 0 && !t.hasSRV()
}

func (t Target) hasAddr() bool { return len(t.Addresses) > 0 || t.Node != "" || t.Tag != "" }
func (t Target) hasSRV() bool  { return t.Port != 0 || t.Priority != 0 || t.Weight != 0 }

// InFile says whether the record goes to the headscale extra-records file: an address record
// without rules. Records of other types, and records with rules, only the server answers.
func (r Record) InFile() bool { return r.Type == TypeAddr && len(r.Rules) == 0 }

func normType(t string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(t)) {
	case "", "A", "AAAA":
		return TypeAddr, nil
	case TypeCNAME:
		return TypeCNAME, nil
	case TypeTXT:
		return TypeTXT, nil
	case TypeSRV:
		return TypeSRV, nil
	}
	return "", errors.New("type must be A, AAAA, CNAME, TXT or SRV")
}

// validHost checks a CNAME or SRV target: a DNS name with at least two labels. Labels may start
// with an underscore (service names).
func validHost(n string) error {
	if len(n) > 253 || !strings.Contains(n, ".") {
		return errors.New("target must be a DNS name with at least two labels")
	}
	for _, l := range strings.Split(n, ".") {
		if !labelRE.MatchString(strings.TrimPrefix(l, "_")) {
			return errors.New("target must be lower-case letters, digits and dashes, " +
				"in labels separated by dots")
		}
	}
	return nil
}

// checkTarget normalises and validates data of type typ. allowEmpty: no data is allowed (the
// record of a name that answers only through rules, or a rule that hides the name).
func checkTarget(t *Target, typ string, allowEmpty bool) error {
	t.Type = typ
	if allowEmpty && t.empty() {
		return nil
	}
	switch typ {
	case TypeAddr:
		if t.Target != "" || len(t.Text) > 0 || t.hasSRV() {
			return errors.New("an A record takes addresses, node or tag only")
		}
		return checkAddr(t)
	case TypeCNAME, TypeSRV:
		return checkHostTarget(t, typ)
	case TypeTXT:
		return checkText(t)
	}
	return nil
}

// checkHostTarget validates a CNAME or SRV record: a target name, and a port for SRV.
func checkHostTarget(t *Target, typ string) error {
	if t.hasAddr() || len(t.Text) > 0 {
		return fmt.Errorf("a %s record takes a target name, "+
			"not addresses, node, tag or text", typ)
	}
	if typ == TypeCNAME && t.hasSRV() {
		return errors.New("a CNAME record takes a target only")
	}
	t.Target = servedns.Normalize(t.Target)
	if err := validHost(t.Target); err != nil {
		return err
	}
	if typ == TypeSRV && t.Port == 0 {
		return fmt.Errorf("an SRV record needs a port from 1 to %d", srvPortLimit)
	}
	return nil
}

func notPrintableASCII(r rune) bool { return r > unicode.MaxASCII || !unicode.IsPrint(r) }

// checkText validates a TXT record: 1 to maxTexts printable ASCII strings.
func checkText(t *Target) error {
	if t.hasAddr() || t.Target != "" || t.hasSRV() {
		return errors.New("a TXT record takes text only")
	}
	if len(t.Text) == 0 || len(t.Text) > maxTexts {
		return fmt.Errorf("a TXT record takes 1 to %d strings", maxTexts)
	}
	for _, s := range t.Text {
		if len(s) > maxTextLen {
			return fmt.Errorf("a TXT string is longer than %d bytes", maxTextLen)
		}
		if strings.IndexFunc(s, notPrintableASCII) >= 0 {
			return errors.New("a TXT string must be printable ASCII")
		}
	}
	return nil
}

func checkAddr(t *Target) error {
	set := 0
	if len(t.Addresses) > 0 {
		set++
	}
	if t.Node != "" {
		set++
	}
	if t.Tag != "" {
		set++
	}
	if set != 1 {
		return errors.New("give exactly one of addresses, node or tag")
	}
	if len(t.Addresses) > maxAddresses {
		return fmt.Errorf("at most %d addresses", maxAddresses)
	}
	for i, a := range t.Addresses {
		ip, err := netip.ParseAddr(strings.TrimSpace(a))
		if err != nil || !InTailnet(ip) {
			return errors.New("an address is not a tailnet address " +
				"(100.64.0.0/10 or fd7a:115c:a1e0::/48)")
		}
		t.Addresses[i] = ip.Unmap().String()
	}
	slices.Sort(t.Addresses)
	t.Addresses = slices.Compact(t.Addresses)
	t.Node = strings.ToLower(strings.TrimSpace(t.Node))
	if t.Node != "" && !labelRE.MatchString(t.Node) {
		return errors.New("node must be the given name of a headscale node")
	}
	t.Tag = strings.TrimSpace(t.Tag)
	if t.Tag != "" && !tagRE.MatchString(t.Tag) {
		return errors.New("tag must be tag:<name>")
	}
	return nil
}

// checkRules normalises and validates the rules of a record of type typ.
func checkRules(rules []Rule, typ string) error {
	if len(rules) > maxRules {
		return fmt.Errorf("at most %d rules", maxRules)
	}
	for i := range rules {
		r := &rules[i]
		if len(r.Users)+len(r.Tags) == 0 {
			return fmt.Errorf("rule %d: give users or tags", i+1)
		}
		if len(r.Users)+len(r.Tags) > maxSubjects {
			return fmt.Errorf("rule %d: at most %d users and tags", i+1, maxSubjects)
		}
		for j, u := range r.Users {
			u = strings.TrimSpace(u)
			if u == "" || len(u) > maxLoginLen || strings.IndexFunc(u, func(c rune) bool {
				return unicode.IsSpace(c) || unicode.IsControl(c)
			}) >= 0 {
				return fmt.Errorf("rule %d: a user must be a tailnet login", i+1)
			}
			r.Users[j] = u
		}
		for j, t := range r.Tags {
			t = strings.TrimSpace(t)
			if !tagRE.MatchString(t) {
				return fmt.Errorf("rule %d: tag must be tag:<name>", i+1)
			}
			r.Tags[j] = t
		}
		// A rule without a type has the type of its record.
		rt, err := typ, error(nil)
		if strings.TrimSpace(r.Type) != "" {
			rt, err = normType(r.Type)
		}
		if err != nil || rt != typ {
			return fmt.Errorf("rule %d: the data of a rule has the type of its record (%s)",
				i+1, typ)
		}
		// A rule with no data hides the name from its clients (NODATA).
		if err := checkTarget(&r.Target, typ, true); err != nil {
			return fmt.Errorf("rule %d: %w", i+1, err)
		}
	}
	return nil
}

// --- answers ---

// resolve gives the answer data of t: the addresses of an address target (a node or tag target
// follows the nodes), or the record of another type.
func resolve(t Target, nodes []headscale.Node) ([]netip.Addr, []servedns.RR) {
	switch t.Type {
	case TypeCNAME:
		if t.Target == "" {
			return nil, nil
		}
		return nil, []servedns.RR{{Type: dnsmessage.TypeCNAME, Target: t.Target}}
	case TypeTXT:
		if len(t.Text) == 0 {
			return nil, nil
		}
		return nil, []servedns.RR{{Type: dnsmessage.TypeTXT, Text: slices.Clone(t.Text)}}
	case TypeSRV:
		if t.Target == "" {
			return nil, nil
		}
		return nil, []servedns.RR{{Type: dnsmessage.TypeSRV, Target: t.Target, Port: t.Port,
			Priority: t.Priority, Weight: t.Weight}}
	}
	var addrs []netip.Addr
	switch {
	case t.Node != "":
		for _, n := range nodes {
			if strings.EqualFold(n.GivenName, t.Node) {
				addrs = append(addrs, nodeAddrs(n)...)
			}
		}
	case t.Tag != "":
		for _, n := range nodes {
			if n.Online && slices.Contains(n.Tags, t.Tag) {
				addrs = append(addrs, nodeAddrs(n)...)
			}
		}
	default:
		for _, a := range t.Addresses {
			if ip, err := netip.ParseAddr(a); err == nil && InTailnet(ip) {
				addrs = append(addrs, ip.Unmap())
			}
		}
	}
	slices.SortFunc(addrs, func(a, b netip.Addr) int { return a.Compare(b) })
	return slices.Compact(addrs), nil
}

// answers resolves the targets of the managed records and of their rules. A target with no match is
// a name without data (NODATA).
func answers(recs []Record, nodes []headscale.Node) []servedns.Record {
	out := make([]servedns.Record, 0, len(recs))
	for _, r := range recs {
		addrs, rrs := resolve(r.Target, nodes)
		sr := servedns.Record{Name: r.Name, Addrs: addrs, RRs: rrs}
		for _, rule := range r.Rules {
			a, x := resolve(rule.Target, nodes)
			sr.Rules = append(sr.Rules,
				servedns.Rule{Users: rule.Users, Tags: rule.Tags, Addrs: a, RRs: x})
		}
		out = append(out, sr)
	}
	return out
}

// answerText is the data of t as text, for the console.
func answerText(addrs []netip.Addr, rrs []servedns.RR) []string {
	out := []string{}
	for _, a := range addrs {
		out = append(out, a.String())
	}
	for _, r := range rrs {
		switch r.Type {
		case dnsmessage.TypeCNAME:
			out = append(out, r.Target)
		case dnsmessage.TypeTXT:
			out = append(out, strconv.Quote(strings.Join(r.Text, "")))
		case dnsmessage.TypeSRV:
			out = append(out, fmt.Sprintf("%d %d %d %s", r.Priority, r.Weight, r.Port, r.Target))
		}
	}
	return out
}

// describeTarget is t in one line, for the activity log.
func describeTarget(t Target) string {
	switch t.Type {
	case TypeCNAME:
		return "CNAME " + t.Target
	case TypeTXT:
		return fmt.Sprintf("TXT (%d strings)", len(t.Text))
	case TypeSRV:
		return fmt.Sprintf("SRV %d %d %d %s", t.Priority, t.Weight, t.Port, t.Target)
	}
	switch {
	case t.Node != "":
		return "node " + t.Node
	case t.Tag != "":
		return t.Tag
	case len(t.Addresses) > 0:
		return strings.Join(t.Addresses, ",")
	}
	return "nothing"
}
