// Package servedns answers DNS for the serve zone (#117) and the tailnet names (#286). headscale
// sends the zone to node-control with dns.nameservers.split, and node-control answers on its
// tailnet address. The zone must not be under the MagicDNS base_domain: clients answer every name
// under it themselves and never ask a split resolver. Bridges ask node-control for every tailnet
// name, MagicDNS names included: their xray resolver is not a tailnet client.
//
// A Server answers from several tables, in a fixed order (Slots): the first table that holds a
// name, or whose zone contains it, gives the answer. A name in no table is refused.
//
// The answers come from a Table that the serve module replaces as a whole. A name is either exact
// ("grafana.svc.example.com") or a wildcard ("*.apps.svc.example.com"), which matches one or more
// labels in front of its suffix. An exact name wins over a wildcard; a longer wildcard wins over a
// shorter one.
package servedns

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// TTL is short: replicas come and go, and the answer follows the online nodes of a service.
const TTL = 30

// Record maps a name in the zone to the data that answers it. Addrs answer A and AAAA queries;
// RRs hold the other types (#285). Rules, when set, give other data to some clients: the first rule
// that matches the client answers, and a client that no rule matches gets Addrs and RRs.
type Record struct {
	Name  string // exact, or "*.<suffix>"
	Addrs []netip.Addr
	RRs   []RR
	Rules []Rule
}

// RR is one record of another type than A or AAAA (#285): CNAME (Target), TXT (Text) or SRV
// (Priority, Weight, Port, Target).
type RR struct {
	Type     dnsmessage.Type
	Target   string // CNAME and SRV: a normalised name
	Text     []string
	Priority uint16
	Weight   uint16
	Port     uint16
	addr     netip.Addr // A and AAAA answers, inside this package
}

// Rule gives Addrs and RRs to the clients it matches: a tailnet login in Users, or a node with a
// tag in Tags (#285). The data of a rule may be empty: the matched clients then get no answer of
// the type (NODATA).
type Rule struct {
	Users []string
	Tags  []string
	Addrs []netip.Addr
	RRs   []RR
}

// Who identifies a client: its tailnet login and the tags of its node.
type Who struct {
	Login string
	Tags  []string
}

func (r Rule) matches(w *Who) bool {
	if w == nil {
		return false
	}
	for _, u := range r.Users {
		if w.Login != "" && strings.EqualFold(u, w.Login) {
			return true
		}
	}
	for _, t := range r.Tags {
		for _, x := range w.Tags {
			if t == x {
				return true
			}
		}
	}
	return false
}

// set is the data of one Record: a name can hold several (one per record type).
type set struct {
	addrs []netip.Addr
	rrs   []RR
	rules []Rule
}

// pick gives the data for the client w: the first rule that matches, else the default.
func (s set) pick(w *Who) ([]netip.Addr, []RR) {
	for _, r := range s.rules {
		if r.matches(w) {
			return r.Addrs, r.RRs
		}
	}
	return s.addrs, s.rrs
}

// Table is an immutable lookup table for one zone, or for a set of exact names (NewNames), or for
// the zones node-control is authoritative for (NewZones).
type Table struct {
	zone  string           // normalised, no trailing dot; "" for a set of names
	zones []string         // NewZones: names in these zones that no other table has do not exist
	exact map[string][]set // normalised name -> data
	wild  []wildEntry      // longest suffix first
}

type wildEntry struct {
	suffix string // "apps.svc.example.com"
	addrs  []netip.Addr
}

// Normalize lower-cases a name and drops one trailing dot.
func Normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

func toSet(r Record) set {
	return set{addrs: append([]netip.Addr(nil), r.Addrs...), rrs: append([]RR(nil), r.RRs...),
		rules: append([]Rule(nil), r.Rules...)}
}

// NewTable builds a table for zone. Records outside the zone are dropped; so is the zone apex as a name.
func NewTable(zone string, recs []Record) *Table {
	t := &Table{zone: Normalize(zone), exact: map[string][]set{}}
	for _, r := range recs {
		n := Normalize(r.Name)
		if suf, ok := strings.CutPrefix(n, "*."); ok {
			if t.inZone(suf) || suf == t.zone {
				addrs := append([]netip.Addr(nil), r.Addrs...)
				t.wild = append(t.wild, wildEntry{suffix: suf, addrs: addrs})
			}
			continue
		}
		if t.inZone(n) {
			t.exact[n] = append(t.exact[n], toSet(r))
		}
	}
	sort.SliceStable(t.wild, func(i, j int) bool { return len(t.wild[i].suffix) > len(t.wild[j].suffix) })
	return t
}

func (t *Table) inZone(n string) bool { return t.zone != "" && strings.HasSuffix(n, "."+t.zone) }

// NewNames builds a table of exact names in any domain (#286): managed records and the headscale
// extra_records. It holds only the names it has: any other name is outside it. Wildcards are
// dropped.
func NewNames(recs []Record) *Table {
	t := &Table{exact: map[string][]set{}}
	for _, r := range recs {
		n := Normalize(r.Name)
		if n == "" || strings.HasPrefix(n, "*.") {
			continue
		}
		t.exact[n] = append(t.exact[n], toSet(r))
	}
	return t
}

// NewZones builds a table that holds no names but claims the zones (#285): a name under a zone
// that no earlier table has does not exist (NXDOMAIN), instead of being refused.
func NewZones(zones []string) *Table {
	t := &Table{exact: map[string][]set{}}
	for _, z := range zones {
		if z = Normalize(z); z != "" {
			t.zones = append(t.zones, z)
		}
	}
	return t
}

// Names gives the exact names of the table, sorted.
func (t *Table) Names() []string {
	out := make([]string, 0, len(t.exact))
	for n := range t.exact {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Zones gives the zones of a NewZones table.
func (t *Table) Zones() []string { return append([]string(nil), t.zones...) }

// Zone returns the normalised zone.
func (t *Table) Zone() string { return t.zone }

// Lookup returns the default addresses for name (rules are not applied) and whether the name
// exists. inZone is false for a name outside the zone.
func (t *Table) Lookup(name string) (addrs []netip.Addr, found, inZone bool) {
	sets, found, inZone := t.lookup(name)
	for _, s := range sets {
		addrs = append(addrs, s.addrs...)
	}
	return addrs, found, inZone
}

func (t *Table) lookup(name string) (sets []set, found, inZone bool) {
	n := Normalize(name)
	if t.zone == "" {
		if s, ok := t.exact[n]; ok {
			return s, true, true
		}
		for _, z := range t.zones {
			if n == z || strings.HasSuffix(n, "."+z) {
				return nil, false, true
			}
		}
		return nil, false, false
	}
	if n != t.zone && !t.inZone(n) {
		return nil, false, false
	}
	if s, ok := t.exact[n]; ok {
		return s, true, true
	}
	for _, w := range t.wild {
		if strings.HasSuffix(n, "."+w.suffix) {
			return []set{{addrs: w.addrs}}, true, true
		}
	}
	return nil, false, true
}

// The table slots of a Server, in the order a query checks them (#286).
const (
	SlotRecords = "records" // records managed in node-control
	SlotExtra   = "extra"   // the headscale dns.extra_records, from the netmap
	SlotZones   = "zones"   // the zones managed in node-control (#285): NXDOMAIN for others
	SlotServe   = "serve"   // the serve zone (#117)
	SlotMagic   = "magic"   // the MagicDNS names of the nodes
)

// Slots is the order in which a query checks the tables.
var Slots = []string{SlotRecords, SlotExtra, SlotZones, SlotServe, SlotMagic}

// Server answers queries from its tables. The serve module and the tailnet DNS module share one
// Server and one listener.
type Server struct {
	mu     sync.Mutex
	slots  map[string]*Table
	tables atomic.Pointer[[]*Table] // the non-nil slots, in Slots order
	listen sync.Once
	Log    *slog.Logger
	// identify, when set, names the client of a query from its source address (#285): the rules
	// of a record use it. It is called only for a name that has rules.
	identify atomic.Pointer[func(netip.Addr) *Who]
}

// SetIdentify sets how the server names a client from its source address. A nil function turns
// the rules off: every client then gets the default data.
func (s *Server) SetIdentify(f func(netip.Addr) *Who) {
	if f == nil {
		s.identify.Store(nil)
		return
	}
	s.identify.Store(&f)
}

func (s *Server) who(src netip.Addr) func() *Who {
	var done bool
	var w *Who
	return func() *Who {
		if done {
			return w
		}
		done = true
		if f := s.identify.Load(); f != nil && src.IsValid() {
			w = (*f)(src.Unmap())
		}
		return w
	}
}

// SetTable replaces the answers of the serve zone atomically.
func (s *Server) SetTable(t *Table) { s.SetSlot(SlotServe, t) }

// SetSlot replaces the table of one slot atomically. A nil table removes the slot.
func (s *Server) SetSlot(slot string, t *Table) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.slots == nil {
		s.slots = map[string]*Table{}
	}
	if t == nil {
		delete(s.slots, slot)
	} else {
		s.slots[slot] = t
	}
	var ts []*Table
	for _, k := range Slots {
		if x := s.slots[k]; x != nil {
			ts = append(ts, x)
		}
	}
	s.tables.Store(&ts)
}

// Slot gives the table of one slot, or nil.
func (s *Server) Slot(slot string) *Table {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.slots[slot]
}

// lookup asks the tables in order; the first that holds the name or its zone decides.
func (s *Server) lookup(name string) (sets []set, found, known, ready bool) {
	p := s.tables.Load()
	if p == nil || len(*p) == 0 {
		return nil, false, false, false
	}
	for _, t := range *p {
		if d, f, in := t.lookup(name); in {
			return d, f, true, true
		}
	}
	return nil, false, false, true
}

// Listener gives a DNS server its sockets: UDP and TCP on port 53 of the node's tailnet address.
type Listener interface {
	ListenDNS() (net.PacketConn, net.Listener, error)
}

// ListenOnce opens the listeners and serves them until ctx ends. Only the first call opens them;
// later calls (the second module that shares the Server) return nil.
func (s *Server) ListenOnce(ctx context.Context, l Listener, log *slog.Logger) error {
	var err error
	s.listen.Do(func() {
		var pc net.PacketConn
		var ln net.Listener
		pc, ln, err = l.ListenDNS()
		if err != nil {
			return
		}
		go func() {
			if e := s.ServeUDP(ctx, pc); e != nil {
				log.Error("dns: UDP stopped", "error", e)
			}
		}()
		go func() {
			if e := s.ServeTCP(ctx, ln); e != nil {
				log.Error("dns: TCP stopped", "error", e)
			}
		}()
	})
	return err
}

// Answer builds the response to one query message from an unknown client: rules never match it.
// It returns nil for a message it cannot parse.
func (s *Server) Answer(msg []byte) []byte { return s.AnswerFrom(msg, netip.Addr{}) }

// maxChase is how many CNAME links an answer follows inside the tables.
const maxChase = 8

// AnswerFrom builds the response to one query message from the client at src. A name with rules
// gives the data of the first rule that matches the client (#285).
func (s *Server) AnswerFrom(msg []byte, src netip.Addr) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil || h.Response {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return reply(h, nil, dnsmessage.RCodeFormatError, nil)
	}
	if h.OpCode != 0 {
		return reply(h, &q, dnsmessage.RCodeNotImplemented, nil)
	}
	who := s.who(src)
	sets, found, inZone, ready := s.lookup(q.Name.String())
	switch {
	case !ready:
		return reply(h, &q, dnsmessage.RCodeServerFailure, nil)
	case !inZone:
		// not our zone: refuse, never recurse
		return reply(h, &q, dnsmessage.RCodeRefused, nil)
	case !found:
		return reply(h, &q, dnsmessage.RCodeNameError, nil)
	}
	var ans []answer
	name := q.Name
	seen := map[string]bool{}
	for hop := 0; ; hop++ {
		seen[Normalize(name.String())] = true
		got, cname := pickAll(sets, q.Type, who)
		for _, g := range got {
			ans = append(ans, answer{name: name, rr: g})
		}
		// A CNAME answers every type except CNAME itself; follow it while the target is ours.
		if cname == nil || q.Type == dnsmessage.TypeCNAME || hop >= maxChase {
			break
		}
		ans = append(ans, answer{name: name, rr: *cname})
		tn, err := dnsmessage.NewName(cname.Target + ".")
		if err != nil || seen[cname.Target] {
			break
		}
		next, f, _, _ := s.lookup(cname.Target)
		if !f {
			break
		}
		name, sets = tn, next
	}
	// a name that exists with no record of this type is NOERROR with no answers (NODATA)
	return reply(h, &q, dnsmessage.RCodeSuccess, ans)
}

// pickAll gives the records of type typ in the data the client gets, and the CNAME of the name
// if it has one (then it has no other data).
func pickAll(sets []set, typ dnsmessage.Type, who func() *Who) (out []RR, cname *RR) {
	for _, st := range sets {
		addrs, rrs := st.addrs, st.rrs
		if len(st.rules) > 0 {
			addrs, rrs = st.pick(who())
		}
		for _, a := range addrs {
			switch {
			case typ == dnsmessage.TypeA && a.Is4():
				out = append(out, RR{Type: dnsmessage.TypeA, addr: a})
			case typ == dnsmessage.TypeAAAA && a.Is6() && !a.Is4In6():
				out = append(out, RR{Type: dnsmessage.TypeAAAA, addr: a})
			}
		}
		for i, r := range rrs {
			switch {
			case r.Type == dnsmessage.TypeCNAME && typ != dnsmessage.TypeCNAME:
				c := rrs[i]
				cname = &c
			case r.Type == typ:
				out = append(out, r)
			}
		}
	}
	if cname != nil && typ != dnsmessage.TypeCNAME {
		out = nil
	}
	return out, cname
}

type answer struct {
	name dnsmessage.Name
	rr   RR
}

func reply(h dnsmessage.Header, q *dnsmessage.Question, rc dnsmessage.RCode, ans []answer) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{
		ID: h.ID, Response: true, Authoritative: rc == dnsmessage.RCodeSuccess || rc == dnsmessage.RCodeNameError,
		RecursionDesired: h.RecursionDesired, RCode: rc,
	})
	b.EnableCompression()
	if q == nil {
		out, _ := b.Finish()
		return out
	}
	_ = b.StartQuestions()
	_ = b.Question(*q)
	_ = b.StartAnswers()
	for _, a := range ans {
		rh := dnsmessage.ResourceHeader{Name: a.name, Class: dnsmessage.ClassINET, TTL: TTL}
		var err error
		switch a.rr.Type {
		case dnsmessage.TypeA:
			err = b.AResource(rh, dnsmessage.AResource{A: a.rr.addr.As4()})
		case dnsmessage.TypeAAAA:
			err = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.rr.addr.As16()})
		case dnsmessage.TypeCNAME:
			var n dnsmessage.Name
			if n, err = dnsmessage.NewName(a.rr.Target + "."); err == nil {
				err = b.CNAMEResource(rh, dnsmessage.CNAMEResource{CNAME: n})
			}
		case dnsmessage.TypeTXT:
			err = b.TXTResource(rh, dnsmessage.TXTResource{TXT: a.rr.Text})
		case dnsmessage.TypeSRV:
			var n dnsmessage.Name
			if n, err = dnsmessage.NewName(a.rr.Target + "."); err == nil {
				err = b.SRVResource(rh, dnsmessage.SRVResource{Priority: a.rr.Priority,
					Weight: a.rr.Weight, Port: a.rr.Port, Target: n})
			}
		}
		if err != nil {
			return nil
		}
	}
	out, err := b.Finish()
	if err != nil {
		return nil
	}
	return out
}

// ServeUDP answers queries on pc until ctx ends or pc fails.
func (s *Server) ServeUDP(ctx context.Context, pc net.PacketConn) error {
	go func() { <-ctx.Done(); pc.Close() }()
	buf := make([]byte, 1500)
	for {
		n, addr, err := pc.ReadFrom(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		if out := s.AnswerFrom(buf[:n], addrOf(addr)); out != nil {
			if len(out) > 512 {
				out = truncated(buf[:n])
			}
			if out != nil {
				_, _ = pc.WriteTo(out, addr)
			}
		}
	}
}

// truncated is an empty answer with TC set, so the client retries over TCP.
func truncated(msg []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return nil
	}
	q, err := p.Question()
	if err != nil {
		return nil
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, Truncated: true, RecursionDesired: h.RecursionDesired})
	_ = b.StartQuestions()
	_ = b.Question(q)
	out, _ := b.Finish()
	return out
}

// ServeTCP answers length-prefixed queries on ln until ctx ends.
func (s *Server) ServeTCP(ctx context.Context, ln net.Listener) error {
	go func() { <-ctx.Done(); ln.Close() }()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		c, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			if errors.Is(err, net.ErrClosed) {
				return err
			}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			s.serveConn(c, addrOf(c.RemoteAddr()))
		}()
	}
}

// addrOf is the IP of a network address, or the zero Addr.
func addrOf(a net.Addr) netip.Addr {
	if a == nil {
		return netip.Addr{}
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap.Addr().Unmap()
	}
	return netip.Addr{}
}

func (s *Server) serveConn(c net.Conn, src netip.Addr) {
	for {
		_ = c.SetDeadline(time.Now().Add(10 * time.Second))
		var l [2]byte
		if _, err := io.ReadFull(c, l[:]); err != nil {
			return
		}
		msg := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(c, msg); err != nil {
			return
		}
		out := s.AnswerFrom(msg, src)
		if out == nil {
			return
		}
		frame := binary.BigEndian.AppendUint16(nil, uint16(len(out)))
		if _, err := c.Write(append(frame, out...)); err != nil {
			return
		}
	}
}
