// Package servedns answers DNS for the serve zone (#117). headscale sends the zone to node-control with
// dns.nameservers.split, and node-control answers on its tailnet address. The zone must not be under the MagicDNS
// base_domain: clients answer every name under it themselves and never ask a split resolver.
//
// The answers come from a Table that the serve module replaces as a whole. A name is either exact
// ("grafana.svc.example.com") or a wildcard ("*.apps.svc.example.com"), which matches one or more labels in front of
// its suffix. An exact name wins over a wildcard; a longer wildcard wins over a shorter one.
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

// Record maps a name in the zone to the addresses that answer it.
type Record struct {
	Name  string // exact, or "*.<suffix>"
	Addrs []netip.Addr
}

// Table is an immutable lookup table for one zone.
type Table struct {
	zone  string                  // normalised, no trailing dot
	exact map[string][]netip.Addr // normalised name -> addrs
	wild  []wildEntry             // longest suffix first
}

type wildEntry struct {
	suffix string // "apps.svc.example.com"
	addrs  []netip.Addr
}

// Normalize lower-cases a name and drops one trailing dot.
func Normalize(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// NewTable builds a table for zone. Records outside the zone are dropped; so is the zone apex as a name.
func NewTable(zone string, recs []Record) *Table {
	t := &Table{zone: Normalize(zone), exact: map[string][]netip.Addr{}}
	for _, r := range recs {
		n := Normalize(r.Name)
		addrs := append([]netip.Addr(nil), r.Addrs...)
		if suf, ok := strings.CutPrefix(n, "*."); ok {
			if t.inZone(suf) || suf == t.zone {
				t.wild = append(t.wild, wildEntry{suffix: suf, addrs: addrs})
			}
			continue
		}
		if t.inZone(n) {
			t.exact[n] = append(t.exact[n], addrs...)
		}
	}
	sort.SliceStable(t.wild, func(i, j int) bool { return len(t.wild[i].suffix) > len(t.wild[j].suffix) })
	return t
}

func (t *Table) inZone(n string) bool { return t.zone != "" && strings.HasSuffix(n, "."+t.zone) }

// Zone returns the normalised zone.
func (t *Table) Zone() string { return t.zone }

// Lookup returns the addresses for name and whether the name exists. inZone is false for a name outside the zone.
func (t *Table) Lookup(name string) (addrs []netip.Addr, found, inZone bool) {
	n := Normalize(name)
	if n != t.zone && !t.inZone(n) {
		return nil, false, false
	}
	if a, ok := t.exact[n]; ok {
		return a, true, true
	}
	for _, w := range t.wild {
		if strings.HasSuffix(n, "."+w.suffix) {
			return w.addrs, true, true
		}
	}
	return nil, false, true
}

// Server answers queries from the current table.
type Server struct {
	table atomic.Pointer[Table]
	Log   *slog.Logger
}

// SetTable replaces the answers atomically.
func (s *Server) SetTable(t *Table) { s.table.Store(t) }

// Answer builds the response to one query message. It returns nil for a message it cannot parse.
func (s *Server) Answer(msg []byte) []byte {
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
	t := s.table.Load()
	if t == nil {
		return reply(h, &q, dnsmessage.RCodeServerFailure, nil)
	}
	addrs, found, inZone := t.Lookup(q.Name.String())
	switch {
	case !inZone:
		// not our zone: refuse, never recurse
		return reply(h, &q, dnsmessage.RCodeRefused, nil)
	case !found:
		return reply(h, &q, dnsmessage.RCodeNameError, nil)
	}
	var ans []netip.Addr
	for _, a := range addrs {
		if (q.Type == dnsmessage.TypeA && a.Is4()) || (q.Type == dnsmessage.TypeAAAA && a.Is6() && !a.Is4In6()) {
			ans = append(ans, a)
		}
	}
	// a name that exists with no record of this type is NOERROR with no answers (NODATA)
	return reply(h, &q, dnsmessage.RCodeSuccess, ans)
}

func reply(h dnsmessage.Header, q *dnsmessage.Question, rc dnsmessage.RCode, addrs []netip.Addr) []byte {
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
	for _, a := range addrs {
		rh := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: TTL}
		if a.Is4() {
			_ = b.AResource(rh, dnsmessage.AResource{A: a.As4()})
		} else {
			_ = b.AAAAResource(rh, dnsmessage.AAAAResource{AAAA: a.As16()})
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
		if out := s.Answer(buf[:n]); out != nil {
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
			s.serveConn(c)
		}()
	}
}

func (s *Server) serveConn(c net.Conn) {
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
		out := s.Answer(msg)
		if out == nil {
			return
		}
		frame := binary.BigEndian.AppendUint16(nil, uint16(len(out)))
		if _, err := c.Write(append(frame, out...)); err != nil {
			return
		}
	}
}
