package servedns

import (
	"slices"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// The first table that holds a name, or whose zone contains it, answers (#286).
func TestSlotsOrder(t *testing.T) {
	s := &Server{}
	if r := parse(t, s.Answer(query(t, "a.example.com.", dnsmessage.TypeA))); r.rc != dnsmessage.RCodeServerFailure {
		t.Errorf("no tables: %v", r.rc)
	}
	s.SetSlot(SlotMagic, NewTable("vpn.example.com", []Record{{Name: "nas.vpn.example.com", Addrs: mustAddrs("100.64.0.30")}}))
	s.SetSlot(SlotExtra, NewNames([]Record{
		{Name: "llm.example.com", Addrs: mustAddrs("100.64.0.20")},
		{Name: "*.wild.example.com", Addrs: mustAddrs("100.64.0.21")}, // dropped: names only
	}))
	s.SetSlot(SlotRecords, NewNames([]Record{{Name: "llm.example.com", Addrs: mustAddrs("100.64.0.41")}}))
	s.SetTable(NewTable("svc.example.com", []Record{{Name: "grafana.svc.example.com", Addrs: mustAddrs("100.64.0.50")}}))
	for _, c := range []struct {
		name  string
		rcode dnsmessage.RCode
		addrs []string
	}{
		{"llm.example.com", dnsmessage.RCodeSuccess, []string{"100.64.0.41"}}, // records win over extra
		{"nas.vpn.example.com", dnsmessage.RCodeSuccess, []string{"100.64.0.30"}},
		{"grafana.svc.example.com", dnsmessage.RCodeSuccess, []string{"100.64.0.50"}},
		{"other.vpn.example.com", dnsmessage.RCodeNameError, nil},
		{"x.wild.example.com", dnsmessage.RCodeRefused, nil},
		{"www.example.org", dnsmessage.RCodeRefused, nil},
	} {
		r := parse(t, s.Answer(query(t, c.name+".", dnsmessage.TypeA)))
		got := r.addrs
		if r.rc != c.rcode || !slices.Equal(got, c.addrs) {
			t.Errorf("%s: %v %v", c.name, r.rc, got)
		}
	}
	s.SetSlot(SlotRecords, nil)
	if r := parse(t, s.Answer(query(t, "llm.example.com.", dnsmessage.TypeA))); len(r.addrs) != 1 || r.addrs[0] != "100.64.0.20" {
		t.Errorf("after removing records: %+v", r)
	}
	if got := s.Slot(SlotExtra).Names(); !slices.Equal(got, []string{"llm.example.com"}) {
		t.Errorf("names: %v", got)
	}
}
