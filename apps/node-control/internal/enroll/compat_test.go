package enroll

import (
	"context"
	"testing"
)

// strictCase is one run of TestStrictRefusesAnIncompatibleAgent.
type strictCase struct {
	name     string
	strict   bool
	api      any // nil: the agent sends no "api"
	wantPush bool
}

// #300: under COMPAT_STRICT a discovered bridge whose agent reports an agent API outside the matrix gets no
// credentials, and its sync state says "incompatible". Without strict mode, and for an agent that reports no API
// version (older than #300), the push goes out as before.
func TestStrictRefusesAnIncompatibleAgent(t *testing.T) {
	for _, c := range []strictCase{
		{"warn only: an incompatible agent still gets its set", false, 9, true},
		{"strict: an incompatible agent is refused", true, 9, false},
		{"strict: a compatible agent gets its set", true, 1, true},
		{"strict: an agent older than #300 is not refused", true, nil, true},
	} {
		checkStrict(t, c)
	}
}

func checkStrict(t *testing.T, c strictCase) {
	t.Helper()
	ctx := context.Background()
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	if c.api != nil {
		a.set("api", c.api)
	}
	f := newRegFx(t, func(rc *RegistryConfig) { rc.Strict = c.strict })
	f.nodes.set(ingressNode("ingress-3", true))
	if _, _, err := f.store.CreateDevice("n", "o", "sing-box", adminFuture); err != nil {
		t.Fatal(err)
	}
	if err := f.approve("ingress-3"); err != nil {
		t.Fatal(err)
	}
	cur := f.reg.Current(ctx, true)
	SyncAll(ctx, f.store, cur.Cfg, cur.SyncTargets)
	if got := a.putCount() > 0; got != c.wantPush {
		t.Fatalf("%s: pushed=%v, want %v", c.name, got, c.wantPush)
	}
	v := f.view("ingress-3")
	if !c.wantPush && (v.Sync == nil || !v.Sync.Pending || v.Sync.State != "incompatible") {
		t.Errorf("%s: sync %+v", c.name, v.Sync)
	}
	if c.api == 9 && v.AgentAPI != 9 {
		t.Errorf("%s: the view must show the reported api: %d", c.name, v.AgentAPI)
	}
}
