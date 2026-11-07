package servedns

import (
	"context"
	"encoding/binary"
	"io"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func query(t *testing.T, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 4242, RecursionDesired: true})
	_ = b.StartQuestions()
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

type result struct {
	rc    dnsmessage.RCode
	aa    bool
	addrs []string
	id    uint16
}

func parse(t *testing.T, msg []byte) result {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !h.Response {
		t.Fatal("not a response")
	}
	_ = p.SkipAllQuestions()
	r := result{rc: h.RCode, aa: h.Authoritative, id: h.ID}
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if rh.TTL != TTL {
			t.Errorf("TTL %d, want %d", rh.TTL, TTL)
		}
		switch rh.Type {
		case dnsmessage.TypeA:
			a, _ := p.AResource()
			r.addrs = append(r.addrs, netip.AddrFrom4(a.A).String())
		case dnsmessage.TypeAAAA:
			a, _ := p.AAAAResource()
			r.addrs = append(r.addrs, netip.AddrFrom16(a.AAAA).String())
		default:
			_ = p.SkipAnswer()
		}
	}
	return r
}

func mustAddrs(s ...string) []netip.Addr {
	var out []netip.Addr
	for _, a := range s {
		out = append(out, netip.MustParseAddr(a))
	}
	return out
}

func testServer() *Server {
	s := &Server{}
	s.SetTable(NewTable("svc.example.com.", []Record{
		{Name: "grafana.svc.example.com", Addrs: mustAddrs("100.64.0.5", "100.64.0.6", "fd7a:115c:a1e0::5")},
		{Name: "*.apps.svc.example.com", Addrs: mustAddrs("100.64.0.9")},
		{Name: "*.b.apps.svc.example.com", Addrs: mustAddrs("100.64.0.10")},
		{Name: "special.apps.svc.example.com", Addrs: mustAddrs("100.64.0.11")},
		{Name: "outside.example.org", Addrs: mustAddrs("100.64.0.12")},
		{Name: "*.example.com", Addrs: mustAddrs("100.64.0.13")},
	}))
	return s
}

func TestAnswers(t *testing.T) {
	s := testServer()
	cases := []struct {
		name  string
		typ   dnsmessage.Type
		rc    dnsmessage.RCode
		addrs []string
	}{
		{"grafana.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.5", "100.64.0.6"}},
		{"GRAFANA.Svc.Example.COM.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.5", "100.64.0.6"}},
		{"grafana.svc.example.com.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, []string{"fd7a:115c:a1e0::5"}},
		{"a.apps.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.9"}},
		{"x.y.apps.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.9"}},
		// the longer wildcard wins
		{"q.b.apps.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.10"}},
		// an exact name wins over a wildcard
		{"special.apps.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []string{"100.64.0.11"}},
		// the wildcard does not match its own suffix
		{"apps.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		// a name that exists but has no AAAA: NODATA
		{"a.apps.svc.example.com.", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, nil},
		{"a.apps.svc.example.com.", dnsmessage.TypeTXT, dnsmessage.RCodeSuccess, nil},
		{"nope.svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"svc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		// outside the zone: refused, never recursed; records outside the zone were dropped
		{"outside.example.org.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
		{"www.example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
		{"evilsvc.example.com.", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
	}
	for _, c := range cases {
		r := parse(t, s.Answer(query(t, c.name, c.typ)))
		if r.rc != c.rc || r.id != 4242 {
			t.Errorf("%s %v: rcode %v id %d, want %v", c.name, c.typ, r.rc, r.id, c.rc)
			continue
		}
		if len(r.addrs) != len(c.addrs) {
			t.Errorf("%s %v: %v, want %v", c.name, c.typ, r.addrs, c.addrs)
			continue
		}
		for i := range c.addrs {
			if r.addrs[i] != c.addrs[i] {
				t.Errorf("%s %v: %v, want %v", c.name, c.typ, r.addrs, c.addrs)
			}
		}
		if c.rc == dnsmessage.RCodeSuccess && !r.aa {
			t.Errorf("%s: not authoritative", c.name)
		}
	}
}

func TestBadInput(t *testing.T) {
	s := testServer()
	if s.Answer([]byte{1, 2, 3}) != nil {
		t.Error("garbage must get no answer")
	}
	// a response is never answered (no reflection loops)
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 1, Response: true})
	msg, _ := b.Finish()
	if s.Answer(msg) != nil {
		t.Error("a response must get no answer")
	}
	// no question
	b = dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7})
	msg, _ = b.Finish()
	if r := parse(t, s.Answer(msg)); r.rc != dnsmessage.RCodeFormatError {
		t.Errorf("no question: %v", r.rc)
	}
	// no table yet
	empty := &Server{}
	if r := parse(t, empty.Answer(query(t, "a.svc.example.com.", dnsmessage.TypeA))); r.rc != dnsmessage.RCodeServerFailure {
		t.Errorf("no table: %v", r.rc)
	}
}

func TestSetTableSwaps(t *testing.T) {
	s := testServer()
	s.SetTable(NewTable("svc.example.com", []Record{{Name: "new.svc.example.com", Addrs: mustAddrs("100.64.1.1")}}))
	if r := parse(t, s.Answer(query(t, "grafana.svc.example.com.", dnsmessage.TypeA))); r.rc != dnsmessage.RCodeNameError {
		t.Errorf("old name still answered: %v", r.rc)
	}
	if r := parse(t, s.Answer(query(t, "new.svc.example.com.", dnsmessage.TypeA))); len(r.addrs) != 1 {
		t.Errorf("new name: %v", r.addrs)
	}
}

func TestUDPAndTCP(t *testing.T) {
	s := testServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.ServeUDP(ctx, pc)
	c, err := net.Dial("udp", pc.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write(query(t, "x.apps.svc.example.com.", dnsmessage.TypeA)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	if r := parse(t, buf[:n]); r.rc != dnsmessage.RCodeSuccess || len(r.addrs) != 1 || r.addrs[0] != "100.64.0.9" {
		t.Errorf("udp: %+v", r)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.ServeTCP(ctx, ln)
	tc, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
	// two queries on one connection
	for _, name := range []string{"grafana.svc.example.com.", "nope.svc.example.com."} {
		q := query(t, name, dnsmessage.TypeA)
		if _, err := tc.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(q))), q...)); err != nil {
			t.Fatal(err)
		}
		var l [2]byte
		if _, err := io.ReadFull(tc, l[:]); err != nil {
			t.Fatal(err)
		}
		resp := make([]byte, binary.BigEndian.Uint16(l[:]))
		if _, err := io.ReadFull(tc, resp); err != nil {
			t.Fatal(err)
		}
		r := parse(t, resp)
		if name == "grafana.svc.example.com." && (r.rc != dnsmessage.RCodeSuccess || len(r.addrs) != 2) {
			t.Errorf("tcp %s: %+v", name, r)
		}
		if name == "nope.svc.example.com." && r.rc != dnsmessage.RCodeNameError {
			t.Errorf("tcp %s: %+v", name, r)
		}
	}
}

func TestLargeAnswerTruncatesOverUDP(t *testing.T) {
	var many []netip.Addr
	for i := 1; i <= 60; i++ {
		many = append(many, netip.AddrFrom4([4]byte{100, 64, 2, byte(i)}))
	}
	s := &Server{}
	s.SetTable(NewTable("svc.example.com", []Record{{Name: "big.svc.example.com", Addrs: many}}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pc, _ := net.ListenPacket("udp", "127.0.0.1:0")
	go s.ServeUDP(ctx, pc)
	c, _ := net.Dial("udp", pc.LocalAddr().String())
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	_, _ = c.Write(query(t, "big.svc.example.com.", dnsmessage.TypeA))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var p dnsmessage.Parser
	h, _ := p.Start(buf[:n])
	if !h.Truncated || n > 512 {
		t.Errorf("want a truncated answer under 512 bytes, got TC=%v len=%d", h.Truncated, n)
	}
}
