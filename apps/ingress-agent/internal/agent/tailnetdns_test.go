package agent

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

var tdnsOK = &TailnetDNS{Server: "tcp://100.64.0.1:53",
	Domains: []string{"full:llm-proxy.example.com", "domain:vpn.example.com", "domain:svc.example.com"}}

func dnsBody(dns any, cs ...Cred) map[string]any {
	b := bodyOf(cs...)
	l := b["credentials"].([]map[string]any)
	for i, c := range cs {
		if c.Tailnet != "" {
			l[i]["tailnet"] = c.Tailnet
		}
	}
	if dns != nil {
		b["tailnet_dns"] = dns
	}
	return b
}

func servers(cfg map[string]any) []any {
	return cfg["dns"].(map[string]any)["servers"].([]any)
}

// The tailnet resolver is the first server, for its names only: no fallback either way, tailnet answers only.
func TestTailnetDNSServerComesFirst(t *testing.T) {
	b, err := Render(withTailnetOut(t), []Cred{{UUID: u1, Mode: "auto", NotAfter: 9, Tailnet: "all"}},
		nil, []string{"xx"}, nil, tdnsOK)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	_ = json.Unmarshal(b, &cfg)
	s := servers(cfg)
	jsonEq(t, s[0], `{"address":"tcp://100.64.0.1:53","domains":["domain:svc.example.com","domain:vpn.example.com","full:llm-proxy.example.com"],"skipFallback":true,"finalQuery":true,"expectedIPs":["100.64.0.0/10","fd7a:115c:a1e0::/48"]}`)
	// the direct-list resolver and the exit resolvers follow, unchanged
	jsonEq(t, s[1], `{"address":"localhost","domains":["geosite:xx"],"skipFallback":true}`)
	if s[2] != DefaultExitDNS[0] || len(s) != 2+len(DefaultExitDNS) {
		t.Errorf("servers: %v", s)
	}
	// the routing order is unchanged: dns rule, tailnet IP rule, block, modes
	rules := cfg["routing"].(map[string]any)["rules"].([]any)
	jsonEq(t, rules[0], `{"type":"field","inboundTag":["dns-internal"],"outboundTag":"via-exit"}`)
	if rules[1].(map[string]any)["outboundTag"] != "via-tailnet" || rules[2].(map[string]any)["outboundTag"] != "blocked" {
		t.Errorf("rule order: %v", rules[:3])
	}
}

func TestNoTailnetDNSNoServer(t *testing.T) {
	cfg := render(t, base, []Cred{cred(u1, "auto", 9)}, nil, nil)
	if strings.Contains(string(mustJSON(t, cfg["dns"])), "100.64.0.1") {
		t.Error("a tailnet DNS server without tailnet_dns")
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestTailnetDNSValidate(t *testing.T) {
	bad := []TailnetDNS{
		{Server: "tcp://192.0.2.1:53", Domains: []string{"full:a.example.com"}},      // not a tailnet address
		{Server: "udp://100.64.0.1:53", Domains: []string{"full:a.example.com"}},     // not tcp
		{Server: "tcp://100.64.0.1:0", Domains: []string{"full:a.example.com"}},      // port
		{Server: "tcp://100.64.0.1:53"},                                              // no names
		{Server: "tcp://100.64.0.1:53", Domains: []string{"a.example.com"}},          // no matcher prefix
		{Server: "tcp://100.64.0.1:53", Domains: []string{"regexp:.*"}},              // other matcher kinds
		{Server: "tcp://100.64.0.1:53", Domains: []string{"full:example"}},           // one label
		{Server: "tcp://100.64.0.1:53", Domains: []string{"full:A.example.com"}},     // upper case
		{Server: "tcp://100.64.0.1:53", Domains: []string{"geosite:private"}},        // geodata
		{Server: "tcp://100.64.0.1:53", Domains: []string{`full:a.example.com","x`}}, // injection
	}
	for _, b := range bad {
		if b.Validate() == nil {
			t.Errorf("accepted %+v", b)
		}
	}
	if err := tdnsOK.Validate(); err != nil {
		t.Error(err)
	}
}

func TestTailnetDNSOverTheAPI(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(r.a.BasePath, withTailnetOut(t), 0o640)
	g := Cred{UUID: u1, Mode: "auto", NotAfter: now0 + 1000, Tailnet: "all"}
	if c := r.put(dnsBody(tdnsOK, g), "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	if s := servers(r.config()); !strings.Contains(string(mustJSON(t, s[0])), "tcp://100.64.0.1:53") {
		t.Fatalf("servers: %v", s)
	}
	got, err := r.a.CurrentDNS()
	if err != nil || got == nil || got.Server != tdnsOK.Server {
		t.Fatalf("persisted: %+v %v", got, err)
	}
	// the same push, names in another order, changes nothing
	r.proc.reset()
	same := &TailnetDNS{Server: tdnsOK.Server, Domains: []string{tdnsOK.Domains[2], tdnsOK.Domains[0], tdnsOK.Domains[1]}}
	if c := r.put(dnsBody(same, g), "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	if len(r.proc.calls) != 0 {
		t.Errorf("an unchanged push ran %v", r.proc.calls)
	}
	// new names alone re-render
	more := &TailnetDNS{Server: tdnsOK.Server, Domains: append([]string{"full:new.example.com"}, tdnsOK.Domains...)}
	if c := r.put(dnsBody(more, g), "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	if !strings.Contains(string(mustJSON(t, servers(r.config())[0])), "full:new.example.com") {
		t.Error("new names not rendered")
	}
	// a push without tailnet_dns (an older node-control, or the bridge lost tailnet access) removes it
	if c := r.put(dnsBody(nil, g), "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	if strings.Contains(string(mustJSON(t, r.config()["dns"])), "100.64.0.1") {
		t.Error("tailnet DNS server kept")
	}
	if got, _ := r.a.CurrentDNS(); got != nil {
		t.Errorf("tailnet DNS still persisted: %+v", got)
	}
}

func TestTailnetDNSBadBodyIs422AndChangesNothing(t *testing.T) {
	r := newRig(t)
	g := cred(u1, "auto", now0+1000)
	for _, dns := range []any{
		map[string]any{"server": "tcp://192.0.2.1:53", "domains": []string{"full:a.example.com"}},
		map[string]any{"server": "tcp://100.64.0.1:53", "domains": []string{"regexp:.*"}},
		map[string]any{"server": "tcp://100.64.0.1:53", "domains": []string{"full:a.example.com"}, "extra": 1},
		"tcp://100.64.0.1:53",
	} {
		if c := r.put(dnsBody(dns, g), "").Code; c != 422 {
			t.Errorf("%v: %d", dns, c)
		}
	}
	if _, err := os.Stat(r.a.ConfigPath); err == nil {
		t.Error("a refused push wrote a config")
	}
}

// After a restart the persisted resolver is rendered again; the expiry sweep keeps it.
func TestTailnetDNSSurvivesRestoreAndExpiry(t *testing.T) {
	r := newRig(t)
	_ = os.WriteFile(r.a.BasePath, withTailnetOut(t), 0o640)
	a := Cred{UUID: u1, Mode: "auto", NotAfter: now0 + 10, Tailnet: "all"}
	b := Cred{UUID: u2, Mode: "auto", NotAfter: now0 + 1000, Tailnet: "all"}
	if c := r.put(dnsBody(tdnsOK, a, b), "").Code; c != 200 {
		t.Fatalf("%d", c)
	}
	r.now = now0 + 100
	if changed, err := r.a.Expire(t.Context()); err != nil || !changed {
		t.Fatalf("expire: %v %v", changed, err)
	}
	if !strings.Contains(string(mustJSON(t, r.config()["dns"])), "100.64.0.1") {
		t.Error("expiry dropped the tailnet DNS server")
	}
	_ = os.Remove(r.a.ConfigPath)
	if err := r.a.Restore(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(mustJSON(t, r.config()["dns"])), "100.64.0.1") {
		t.Error("restore dropped the tailnet DNS server")
	}
}
