package servedns

import (
	"net/netip"
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// typed is one parsed answer: its owner name, type and a text form of its data.
type typed struct {
	name, typ, data string
}

func parseTyped(t *testing.T, msg []byte) (dnsmessage.RCode, []typed) {
	t.Helper()
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		t.Fatal(err)
	}
	_ = p.SkipAllQuestions()
	var out []typed
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			break
		}
		x := typed{name: rh.Name.String(), typ: rh.Type.String()}
		switch rh.Type {
		case dnsmessage.TypeA:
			a, _ := p.AResource()
			x.data = netip.AddrFrom4(a.A).String()
		case dnsmessage.TypeAAAA:
			a, _ := p.AAAAResource()
			x.data = netip.AddrFrom16(a.AAAA).String()
		case dnsmessage.TypeCNAME:
			c, _ := p.CNAMEResource()
			x.data = c.CNAME.String()
		case dnsmessage.TypeTXT:
			c, _ := p.TXTResource()
			for _, s := range c.TXT {
				x.data += "[" + s + "]"
			}
		case dnsmessage.TypeSRV:
			c, _ := p.SRVResource()
			x.data = c.Target.String() + ":" + itoa(int(c.Port)) + "/" + itoa(int(c.Priority)) + "/" + itoa(int(c.Weight))
		default:
			_ = p.SkipAnswer()
		}
		out = append(out, x)
	}
	return h.RCode, out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

func typedServer() *Server {
	s := &Server{}
	s.SetSlot(SlotRecords, NewNames([]Record{
		{Name: "app.example.com", Addrs: mustAddrs("100.64.0.10", "fd7a:115c:a1e0::10")},
		{Name: "alias.example.com", RRs: []RR{{Type: dnsmessage.TypeCNAME, Target: "app.example.com"}}},
		{Name: "loop1.example.com", RRs: []RR{{Type: dnsmessage.TypeCNAME, Target: "loop2.example.com"}}},
		{Name: "loop2.example.com", RRs: []RR{{Type: dnsmessage.TypeCNAME, Target: "loop1.example.com"}}},
		{Name: "out.example.com", RRs: []RR{{Type: dnsmessage.TypeCNAME, Target: "www.example.org"}}},
		{Name: "info.example.com", RRs: []RR{{Type: dnsmessage.TypeTXT, Text: []string{"v=1", "hello"}}}},
		{Name: "_http._tcp.example.com", RRs: []RR{{Type: dnsmessage.TypeSRV, Target: "app.example.com", Port: 8080, Priority: 10, Weight: 5}}},
		{Name: "split.example.com", Addrs: mustAddrs("100.64.0.1"), Rules: []Rule{
			{Users: []string{"alice@example.com"}, Addrs: mustAddrs("100.64.0.2")},
			{Tags: []string{"tag:b"}, Addrs: mustAddrs("100.64.0.3")},
			{Tags: []string{"tag:none"}},
		}},
		{Name: "rulealias.example.com", Rules: []Rule{
			{Tags: []string{"tag:b"}, RRs: []RR{{Type: dnsmessage.TypeCNAME, Target: "split.example.com"}}},
		}},
	}))
	s.SetSlot(SlotZones, NewZones([]string{"example.com"}))
	who := map[string]*Who{
		"100.64.1.1": {Login: "Alice@example.com"},
		"100.64.1.2": {Login: "bob@example.com", Tags: []string{"tag:b"}},
		"100.64.1.3": {Tags: []string{"tag:none"}},
	}
	s.SetIdentify(func(a netip.Addr) *Who { return who[a.String()] })
	return s
}

func ask(t *testing.T, s *Server, name string, typ dnsmessage.Type, src string) (dnsmessage.RCode, []typed) {
	t.Helper()
	var a netip.Addr
	if src != "" {
		a = netip.MustParseAddr(src)
	}
	return parseTyped(t, s.AnswerFrom(query(t, name+".", typ), a))
}

func TestTypedRecords(t *testing.T) {
	s := typedServer()
	cases := []struct {
		name string
		typ  dnsmessage.Type
		rc   dnsmessage.RCode
		want []typed
	}{
		{"app.example.com", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []typed{{"app.example.com.", "TypeA", "100.64.0.10"}}},
		{"app.example.com", dnsmessage.TypeAAAA, dnsmessage.RCodeSuccess, []typed{{"app.example.com.", "TypeAAAA", "fd7a:115c:a1e0::10"}}},
		{"app.example.com", dnsmessage.TypeTXT, dnsmessage.RCodeSuccess, nil},
		// CNAME: the chain, then the target's data
		{"alias.example.com", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []typed{
			{"alias.example.com.", "TypeCNAME", "app.example.com."}, {"app.example.com.", "TypeA", "100.64.0.10"}}},
		{"alias.example.com", dnsmessage.TypeCNAME, dnsmessage.RCodeSuccess, []typed{{"alias.example.com.", "TypeCNAME", "app.example.com."}}},
		{"alias.example.com", dnsmessage.TypeTXT, dnsmessage.RCodeSuccess, []typed{{"alias.example.com.", "TypeCNAME", "app.example.com."}}},
		// a target outside the tables: only the CNAME, the client resolves the rest
		{"out.example.com", dnsmessage.TypeA, dnsmessage.RCodeSuccess, []typed{{"out.example.com.", "TypeCNAME", "www.example.org."}}},
		{"info.example.com", dnsmessage.TypeTXT, dnsmessage.RCodeSuccess, []typed{{"info.example.com.", "TypeTXT", "[v=1][hello]"}}},
		{"info.example.com", dnsmessage.TypeA, dnsmessage.RCodeSuccess, nil},
		{"_http._tcp.example.com", dnsmessage.TypeSRV, dnsmessage.RCodeSuccess, []typed{{"_http._tcp.example.com.", "TypeSRV", "app.example.com.:8080/10/5"}}},
		// zones: an unknown name in a managed zone does not exist; outside, it is refused
		{"nothing.example.com", dnsmessage.TypeA, dnsmessage.RCodeNameError, nil},
		{"www.example.org", dnsmessage.TypeA, dnsmessage.RCodeRefused, nil},
	}
	for _, c := range cases {
		rc, got := ask(t, s, c.name, c.typ, "")
		if rc != c.rc || !slices.Equal(got, c.want) {
			t.Errorf("%s %v: %v %v, want %v %v", c.name, c.typ, rc, got, c.rc, c.want)
		}
	}
	// a CNAME loop stops
	if rc, got := ask(t, s, "loop1.example.com", dnsmessage.TypeA, ""); rc != dnsmessage.RCodeSuccess || len(got) != 2 {
		t.Errorf("loop: %v %v", rc, got)
	}
}

func TestRulesPickTheFirstMatch(t *testing.T) {
	s := typedServer()
	for src, want := range map[string][]string{
		"":           {"100.64.0.1"}, // unknown client: the default
		"100.64.9.9": {"100.64.0.1"}, // a client no rule matches
		"100.64.1.1": {"100.64.0.2"}, // user rule, login case-insensitive
		"100.64.1.2": {"100.64.0.3"}, // tag rule
		"100.64.1.3": nil,            // a rule with no data: NODATA
	} {
		rc, got := ask(t, s, "split.example.com", dnsmessage.TypeA, src)
		var ips []string
		for _, g := range got {
			ips = append(ips, g.data)
		}
		if rc != dnsmessage.RCodeSuccess || !slices.Equal(ips, want) {
			t.Errorf("from %q: %v %v, want %v", src, rc, ips, want)
		}
	}
	// a CNAME given by a rule is followed with the same client
	_, got := ask(t, s, "rulealias.example.com", dnsmessage.TypeA, "100.64.1.2")
	if len(got) != 2 || got[1].data != "100.64.0.3" {
		t.Errorf("rule CNAME: %v", got)
	}
	if rc, got := ask(t, s, "rulealias.example.com", dnsmessage.TypeA, "100.64.1.1"); rc != dnsmessage.RCodeSuccess || len(got) != 0 {
		t.Errorf("rule CNAME, no match: %v %v", rc, got)
	}
	// identify is called only for names with rules
	calls := 0
	s.SetIdentify(func(netip.Addr) *Who { calls++; return nil })
	ask(t, s, "app.example.com", dnsmessage.TypeA, "100.64.1.1")
	if calls != 0 {
		t.Errorf("identify called %d times for a name without rules", calls)
	}
	ask(t, s, "rulealias.example.com", dnsmessage.TypeA, "100.64.1.1")
	if calls != 1 {
		t.Errorf("identify called %d times for one query", calls)
	}
}
