package enroll

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
)

// dnsAgent is a fakeAgent that also takes the tailnet resolver (#286).
type dnsAgent struct {
	fakeAgent
	mu  sync.Mutex
	dns []*TailnetDNS
}

func (a *dnsAgent) PutCredentialsDNS(ctx context.Context, creds []Credential, dns *TailnetDNS, confirmEmpty bool) error {
	a.mu.Lock()
	a.dns = append(a.dns, dns)
	a.mu.Unlock()
	return a.PutCredentials(ctx, creds, confirmEmpty)
}

var testDNS = TailnetDNS{Server: "tcp://100.64.0.9:53", Domains: []string{"full:llm-proxy.example.com"}}

// Only a bridge with tailnet access gets the resolver; one with "off" gets none, so its agent removes any it had.
func TestSyncAllPushesTailnetDNSOnlyWithTailnetAccess(t *testing.T) {
	e := newEnv(t)
	e.make("a", 0)
	cfg := twoIngressCfg().WithTailnetDNS(func() (TailnetDNS, bool) { return testDNS, true })
	cfg.Ingresses[0].TailnetAccess = TailnetAll
	a1, a2 := &dnsAgent{}, &dnsAgent{}
	ts := targetsFor(cfg, a1, a2)
	if !SyncAll(context.Background(), e.store, cfg, ts) {
		t.Fatal("want true")
	}
	if len(a1.dns) != 1 || a1.dns[0] == nil || !reflect.DeepEqual(*a1.dns[0], testDNS) {
		t.Errorf("granted bridge: %+v", a1.dns)
	}
	if len(a2.dns) != 1 || a2.dns[0] != nil {
		t.Errorf("bridge without tailnet access got a resolver: %+v", a2.dns)
	}
	// no resolver known yet (the DNS module is off or has no address): none is sent
	cfg = cfg.WithTailnetDNS(func() (TailnetDNS, bool) { return TailnetDNS{}, false })
	a3 := &dnsAgent{}
	if !SyncAll(context.Background(), e.store, cfg, targetsFor(cfg, a3, &dnsAgent{})) || a3.dns[0] != nil {
		t.Errorf("unknown resolver sent: %+v", a3.dns)
	}
}

func TestPutCredentialsDNSBody(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(b, &got)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	a := NewAgent(srv.URL, "tok", srv.Client()).(dnsPutter)
	if err := a.PutCredentialsDNS(context.Background(), nil, &testDNS, true); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"server": "tcp://100.64.0.9:53", "domains": []any{"full:llm-proxy.example.com"}}
	if !reflect.DeepEqual(got["tailnet_dns"], want) {
		t.Errorf("body: %v", got)
	}
	if err := a.PutCredentialsDNS(context.Background(), nil, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["tailnet_dns"]; ok {
		t.Errorf("nil resolver sent: %v", got)
	}
}
