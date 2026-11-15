package dnsrec

import (
	"context"
	"net"
	"strings"
	"testing"
)

// #384: the lookup checks the wildcard through a probe name and never writes.
func TestLookupCheck(t *testing.T) {
	answers := map[string][]string{}
	var asked []string
	l := &Lookup{Resolve: func(_ context.Context, name, typ string) ([]string, error) {
		asked = append(asked, typ+" "+name)
		if a, ok := answers[typ+" "+name]; ok {
			return a, nil
		}
		return nil, &net.DNSError{Err: "no such host", Name: name, IsNotFound: true}
	}}
	ctx := context.Background()
	rec, _ := For("*.f.example.com", "198.51.100.7")

	if ok, err := l.Check(ctx, rec); ok || err == nil || !strings.Contains(err.Error(), "no A record") {
		t.Fatalf("missing: %v %v", ok, err)
	}
	if asked[0] != "A headlamp-probe.f.example.com" {
		t.Fatalf("the wildcard is checked through a probe name: %v", asked)
	}
	answers["A headlamp-probe.f.example.com"] = []string{"203.0.113.1", "198.51.100.7"}
	if ok, err := l.Check(ctx, rec); !ok || err != nil {
		t.Fatalf("one of several addresses is the target: %v %v", ok, err)
	}
	answers["A headlamp-probe.f.example.com"] = []string{"203.0.113.1"}
	if ok, err := l.Check(ctx, rec); ok || err == nil || !strings.Contains(err.Error(), "want 198.51.100.7") {
		t.Fatalf("wrong address: %v %v", ok, err)
	}

	cn, _ := For("*.f.example.com", "edge.example.net")
	answers["CNAME headlamp-probe.f.example.com"] = []string{"Edge.Example.NET."}
	if ok, err := l.Check(ctx, cn); !ok || err != nil {
		t.Fatalf("CNAME, case and trailing dot: %v %v", ok, err)
	}
	if err := l.Put(ctx, rec); err != ErrNotWritten {
		t.Fatalf("the lookup never writes: %v", err)
	}
	if changed, err := Ensure(ctx, l, rec); !changed || err != ErrNotWritten {
		t.Fatalf("Ensure on a lookup reports that it cannot write: %v %v", changed, err)
	}
}
