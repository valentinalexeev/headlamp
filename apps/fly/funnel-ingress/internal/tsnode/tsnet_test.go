package tsnode

import (
	"net/netip"
	"testing"
)

func TestWhoIsRemoteAddrParsing(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"100.64.0.1:1234", "100.64.0.1", true},
		{"100.64.0.1", "100.64.0.1", true},
		{"[fd7a:115c:a1e0::1]:443", "fd7a:115c:a1e0::1", true},
		{"fd7a:115c:a1e0::1", "fd7a:115c:a1e0::1", true},
		{"[::ffff:100.64.0.1]:1234", "100.64.0.1", true},
		{"::ffff:100.64.0.1", "100.64.0.1", true},
		{"", "", false},
		{"not-an-address", "", false},
		{"100.64.0.1:notaport", "", false},
		{"example.com:80", "", false},
	}
	for _, c := range cases {
		got, err := parseRemote(c.in)
		if (err == nil) != c.ok {
			t.Errorf("parseRemote(%q): err=%v, want ok=%v", c.in, err, c.ok)
			continue
		}
		if c.ok && got != netip.MustParseAddr(c.want) {
			t.Errorf("parseRemote(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestFakeImplementsNode(t *testing.T) {
	var _ Node = NewFake()
	var _ Node = (*TSNet)(nil)
}
