package clientip

import (
	"net/http"
	"testing"
)

func req(peer string, headers map[string][]string) *http.Request {
	r, _ := http.NewRequest(http.MethodGet, "http://x/", nil)
	r.RemoteAddr = peer
	for k, vs := range headers {
		for _, v := range vs {
			r.Header.Add(k, v)
		}
	}
	return r
}

func must(t *testing.T, header, trusted string) *Resolver {
	t.Helper()
	r, err := New(header, trusted)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestDefaultKeepsEarlierBehaviour(t *testing.T) {
	r := must(t, "", "")
	if r.Header() != DefaultHeader {
		t.Fatalf("header = %q", r.Header())
	}
	cases := []struct {
		name, peer string
		h          map[string][]string
		want       string
	}{
		{"header from any peer", "10.0.0.1:5000", map[string][]string{"Fly-Client-IP": {"203.0.113.7"}}, "203.0.113.7"},
		{"mapped v4 is unmapped", "10.0.0.1:5000", map[string][]string{"Fly-Client-IP": {"::ffff:203.0.113.7"}}, "203.0.113.7"},
		{"invalid header falls back to peer", "10.0.0.1:5000", map[string][]string{"Fly-Client-IP": {"not-an-ip"}}, "10.0.0.1"},
		{"header set twice is ignored", "10.0.0.1:5000", map[string][]string{"Fly-Client-IP": {"203.0.113.7", "198.51.100.1"}}, "10.0.0.1"},
		{"other headers are ignored", "10.0.0.1:5000", map[string][]string{"X-Forwarded-For": {"6.6.6.6"}}, "10.0.0.1"},
		{"v6 peer", "[2001:db8::1]:5000", nil, "2001:db8::1"},
	}
	for _, c := range cases {
		if got := r.ClientIP(req(c.peer, c.h)); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestUntrustedPeerCannotSetTheHeader(t *testing.T) {
	r := must(t, "X-Real-IP", "10.0.0.0/8")
	if got := r.ClientIP(req("192.0.2.9:1", map[string][]string{"X-Real-Ip": {"203.0.113.7"}})); got != "192.0.2.9" {
		t.Fatalf("untrusted peer: got %q", got)
	}
	if got := r.ClientIP(req("10.1.2.3:1", map[string][]string{"X-Real-Ip": {"203.0.113.7"}})); got != "203.0.113.7" {
		t.Fatalf("trusted peer: got %q", got)
	}
	if r.TrustedPeer(req("192.0.2.9:1", nil)) || !r.TrustedPeer(req("10.1.2.3:1", nil)) {
		t.Fatal("TrustedPeer is wrong")
	}
}

func TestForwardedForChain(t *testing.T) {
	r := must(t, "x-forwarded-for", "10.0.0.0/8, 172.16.0.5")
	cases := []struct {
		name string
		h    []string
		want string
	}{
		{"one hop", []string{"203.0.113.7"}, "203.0.113.7"},
		{"client spoofs a left entry", []string{"6.6.6.6, 203.0.113.7"}, "203.0.113.7"},
		{"inner trusted hops are skipped", []string{"203.0.113.7, 172.16.0.5, 10.9.9.9"}, "203.0.113.7"},
		{"split over two header lines", []string{"6.6.6.6", "203.0.113.7, 10.9.9.9"}, "203.0.113.7"},
		{"all trusted gives the left-most", []string{"10.1.1.1, 10.2.2.2"}, "10.1.1.1"},
		{"broken entry falls back to peer", []string{"203.0.113.7, junk"}, "10.0.0.1"},
		{"empty falls back to peer", nil, "10.0.0.1"},
	}
	for _, c := range cases {
		if got := r.ClientIP(req("10.0.0.1:1", map[string][]string{"X-Forwarded-For": c.h})); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestNoneUsesThePeer(t *testing.T) {
	r := must(t, "none", "")
	if r.Header() != "" {
		t.Fatalf("header = %q", r.Header())
	}
	if got := r.ClientIP(req("192.0.2.9:1", map[string][]string{"Fly-Client-IP": {"203.0.113.7"}})); got != "192.0.2.9" {
		t.Fatalf("got %q", got)
	}
}

func TestInvalidSettings(t *testing.T) {
	for _, c := range [][2]string{{"bad header!", ""}, {"", "10.0.0.0/33"}, {"", "nope"}} {
		if _, err := New(c[0], c[1]); err == nil {
			t.Errorf("New(%q, %q) accepted", c[0], c[1])
		}
	}
	if _, err := New("X-Forwarded-For", "10.0.0.1, 2001:db8::/32,"); err != nil {
		t.Errorf("valid list rejected: %v", err)
	}
}

func TestUntrustedPeerInChainMode(t *testing.T) {
	r := must(t, "X-Forwarded-For", "10.0.0.0/8")
	if got := r.ClientIP(req("192.0.2.9:1", map[string][]string{"X-Forwarded-For": {"203.0.113.7"}})); got != "192.0.2.9" {
		t.Fatalf("got %q", got)
	}
}

func TestZonesAndMappedAddresses(t *testing.T) {
	r := must(t, "X-Forwarded-For", "fe80::/10, ::ffff:10.0.0.0/104")
	// A zoned hop is compared without its zone, so a trusted link-local proxy is skipped and the zone never leaks.
	if got := r.ClientIP(req("10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"203.0.113.7, fe80::1%eth0"}})); got != "203.0.113.7" {
		t.Errorf("zoned hop: got %q", got)
	}
	if got := r.ClientIP(req("10.0.0.1:1", map[string][]string{"X-Forwarded-For": {"2001:db8::1%eth0"}})); got != "2001:db8::1" {
		t.Errorf("zone kept: got %q", got)
	}
	// The mapped CIDR became 10.0.0.0/8, so a mapped peer and a plain peer both count as trusted.
	for _, peer := range []string{"[::ffff:10.1.1.1]:1", "10.1.1.1:1"} {
		if !r.TrustedPeer(req(peer, nil)) {
			t.Errorf("peer %s not trusted", peer)
		}
	}
	if _, err := New("", "::ffff:0.0.0.0/80"); err == nil {
		t.Error("mapped prefix shorter than /96 accepted")
	}
}

func TestNoneStillGuardsTheScheme(t *testing.T) {
	r := must(t, "none", "10.0.0.0/8")
	if r.TrustedPeer(req("192.0.2.9:1", nil)) || !r.TrustedPeer(req("10.0.0.1:1", nil)) {
		t.Fatal("TrustedPeer ignores FUNNEL_TRUSTED_PROXIES with header none")
	}
}
