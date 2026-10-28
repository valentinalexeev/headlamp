package auth

import (
	"errors"
	"sync"
	"testing"

	"funnel-ingress/internal/config"
)

func policy() Policy {
	ns := NewNamespaces()
	ns.Set(map[string][]config.PortRange{
		"pdl":    {{Lo: 8080, Hi: 8080}, {Lo: 9000, Hi: 9010}},
		"other":  {{Lo: 3000, Hi: 3000}},
		"closed": nil,
	})
	return Policy{FunnelTag: "tag:funnel", NSTagPrefix: "tag:ns-", NS: ns}
}

func TestAuthorizeMatrix(t *testing.T) {
	cases := []struct {
		name string
		tags []string
		ns   string
		port int
		want string
		err  error
	}{
		{"funnel tag only", []string{"tag:funnel"}, "pdl", 8080, "", ErrForbidden},
		{"ns tag only", []string{"tag:ns-pdl"}, "pdl", 8080, "", ErrForbidden},
		{"no tags", nil, "pdl", 8080, "", ErrForbidden},
		{"both, explicit ns", []string{"tag:funnel", "tag:ns-pdl"}, "pdl", 8080, "pdl", nil},
		{"both, implicit ns", []string{"tag:funnel", "tag:ns-pdl"}, "", 9005, "pdl", nil},
		{"two ns tags, none named", []string{"tag:funnel", "tag:ns-pdl", "tag:ns-other"}, "", 8080, "", ErrNamespaceRequired},
		{"two ns tags, one named", []string{"tag:funnel", "tag:ns-pdl", "tag:ns-other"}, "other", 3000, "other", nil},
		{"ns not among tags", []string{"tag:funnel", "tag:ns-pdl"}, "other", 3000, "", ErrForbidden},
		{"port below ranges", []string{"tag:funnel", "tag:ns-pdl"}, "pdl", 8079, "", ErrPortNotAllowed},
		{"port between ranges", []string{"tag:funnel", "tag:ns-pdl"}, "pdl", 8500, "", ErrPortNotAllowed},
		{"port at range end", []string{"tag:funnel", "tag:ns-pdl"}, "pdl", 9010, "pdl", nil},
		{"port above ranges", []string{"tag:funnel", "tag:ns-pdl"}, "pdl", 9011, "", ErrPortNotAllowed},
		{"ns tag but namespace not configured", []string{"tag:funnel", "tag:ns-ghost"}, "ghost", 80, "", ErrForbidden},
		{"configured with no ranges", []string{"tag:funnel", "tag:ns-closed"}, "closed", 80, "", ErrPortNotAllowed},
		{"bare prefix tag", []string{"tag:funnel", "tag:ns-"}, "", 80, "", ErrForbidden},
		{"funnel tag lookalike suffix", []string{"tag:funnelx", "tag:ns-pdl"}, "pdl", 8080, "", ErrForbidden},
		{"funnel tag different case", []string{"tag:Funnel", "tag:ns-pdl"}, "pdl", 8080, "", ErrForbidden},
		{"ns tag different case", []string{"tag:funnel", "tag:NS-pdl"}, "pdl", 8080, "", ErrForbidden},
		{"ns tag without tag: prefix", []string{"tag:funnel", "ns-pdl"}, "pdl", 8080, "", ErrForbidden},
		{"duplicate ns tags", []string{"tag:funnel", "tag:ns-pdl", "tag:ns-pdl"}, "", 8080, "pdl", nil},
	}
	p := policy()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := p.Authorize(c.tags, c.ns, c.port)
			if !errors.Is(err, c.err) || got != c.want {
				t.Fatalf("got %q, %v; want %q, %v", got, err, c.want, c.err)
			}
		})
	}
}

func TestNamespacesOf(t *testing.T) {
	p := policy()
	if got := p.NamespacesOf([]string{"tag:ns-b", "tag:funnel", "tag:ns-a", "tag:other"}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("got %v", got)
	}
	if got := p.NamespacesOf([]string{"tag:ns-a"}); got != nil {
		t.Fatalf("without the funnel tag: %v", got)
	}
}

func TestNamespacesSetCopiesItsInput(t *testing.T) {
	n := NewNamespaces()
	in := map[string][]config.PortRange{"a": {{Lo: 1, Hi: 2}}}
	n.Set(in)
	in["a"][0].Hi = 65535
	delete(in, "a")
	if !n.Allowed("a", 2) || n.Allowed("a", 3) {
		t.Fatal("Set kept a reference to the caller's map")
	}
}

func TestNamespacesNames(t *testing.T) {
	if got := policy().NS.Names(); len(got) != 3 || got[0] != "closed" || got[1] != "other" || got[2] != "pdl" {
		t.Fatalf("Names() = %v", got)
	}
}

func TestNamespacesSetReplacesAtomically(t *testing.T) {
	n := NewNamespaces()
	a := map[string][]config.PortRange{"x": {{Lo: 1, Hi: 10}}}
	b := map[string][]config.PortRange{"y": {{Lo: 20, Hi: 30}}}
	n.Set(a)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			if i%2 == 0 {
				n.Set(b)
			} else {
				n.Set(a)
			}
		}
	}()
	for i := 0; i < 20000; i++ {
		// One read sees exactly one generation, never a mix or an empty set.
		names := n.Names()
		if len(names) != 1 || (names[0] != "x" && names[0] != "y") {
			t.Fatalf("Names() = %v", names)
		}
		n.Allowed("x", 5)
	}
	close(stop)
	wg.Wait()
}
