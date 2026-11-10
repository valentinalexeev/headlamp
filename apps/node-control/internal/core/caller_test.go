package core

import (
	"context"
	"net"
	"testing"
)

type dnsTailnet struct{ *Local }

func (dnsTailnet) ListenDNS() (net.PacketConn, net.Listener, error) { return nil, nil, nil }

func TestCallerAware(t *testing.T) {
	tn := CallerAware(NewLocal("127.0.0.1"))
	if l, _, _ := tn.WhoIs(context.Background(), "x"); l != "local" {
		t.Fatalf("without a caller WhoIs asks the tailnet: %q", l)
	}
	ctx := WithCaller(context.Background(), Caller{Login: "alice@example.com"})
	if l, tags, _ := tn.WhoIs(ctx, "x"); l != "alice@example.com" || tags != nil {
		t.Fatalf("with a caller: %q %v", l, tags)
	}
	if CallerAware(tn) != tn {
		t.Fatal("wrapping twice must not stack")
	}
	if _, ok := CallerAware(dnsTailnet{NewLocal("127.0.0.1")}).(DNSListener); !ok {
		t.Fatal("the DNS listener must survive the wrap")
	}
	if _, ok := CallerFrom(WithCaller(context.Background(), Caller{})); ok {
		t.Fatal("an empty login is no caller")
	}
}
