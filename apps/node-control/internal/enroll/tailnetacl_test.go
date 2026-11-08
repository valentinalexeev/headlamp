package enroll

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/policy"
)

type fakePolicy struct {
	text    string
	changes int
	err     error
}

func (p *fakePolicy) Current(context.Context) (string, string, error) { return p.text, "", nil }
func (p *fakePolicy) Change(_ context.Context, _ string, tr func(string) (string, error), _ string) (int64, time.Time, error) {
	if p.err != nil {
		return 0, time.Time{}, p.err
	}
	next, err := tr(p.text)
	if err != nil {
		return 0, time.Time{}, err
	}
	if next == p.text {
		return 0, time.Time{}, policy.ErrNoChange
	}
	p.text = next
	p.changes++
	return int64(p.changes), time.Unix(adminNow+300, 0), nil
}

func taclFx(t *testing.T, grant bool, dst []string, pol *fakePolicy) *adminFx {
	cfg := adminCfg
	cfg.Ingresses = append([]Ingress(nil), cfg.Ingresses...)
	if grant {
		cfg.Ingresses[0].TailnetAccess = TailnetAll
	}
	return taclFxCfg(t, cfg, dst, pol)
}

func taclFxCfg(t *testing.T, cfg IngressConfig, dst []string, pol *fakePolicy) *adminFx {
	f := newAdminFxCfg(t, cfg)
	f.admin.tacl = &tailnetACL{Tag: "tag:ingress", Dst: dst, pol: func() PolicyService {
		if pol == nil {
			return nil
		}
		return pol
	}}
	f.mux = core.NewMux()
	f.admin.Register(f.mux)
	return f
}

func TestTailnetACLApplyAndState(t *testing.T) {
	pol := &fakePolicy{text: `{"acls":[{"action":"accept","src":["group:a"],"dst":["*:*"]}]}` + "\n"}
	f := taclFx(t, true, []string{"tag:lan:*"}, pol)
	if s := decode(t, f.do("GET", "/enroll/tailnet-acl", "", ""))["status"]; s != "out_of_sync" {
		t.Errorf("before: %v", s)
	}
	rec := f.post("/enroll/tailnet-acl", map[string]any{})
	if rec.Code != 202 || decode(t, rec)["change_id"] != 1.0 {
		t.Fatalf("apply: %d %s", rec.Code, rec.Body)
	}
	if !strings.Contains(pol.text, `"tag:ingress"`) || !strings.Contains(pol.text, `"tag:lan:*"`) || !strings.Contains(pol.text, "group:a") {
		t.Errorf("policy:\n%s", pol.text)
	}
	if s := decode(t, f.do("GET", "/enroll/tailnet-acl", "", ""))["status"]; s != "in_sync" {
		t.Errorf("after: %v", s)
	}
	if rec := f.post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 200 {
		t.Errorf("again: %d", rec.Code)
	}
}

func TestTailnetACLRemovedWhenNoBridgeGrants(t *testing.T) {
	pol := &fakePolicy{text: `{"acls":[{"action":"accept","src":["tag:ingress"],"dst":["tag:lan:*"]}]}` + "\n"}
	f := taclFx(t, false, []string{"tag:lan:*"}, pol)
	if rec := f.post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 202 || strings.Contains(pol.text, "tag:ingress") {
		t.Errorf("%d\n%s", rec.Code, pol.text)
	}
}

func TestTailnetACLRefusals(t *testing.T) {
	if rec := taclFx(t, true, []string{"tag:lan:*"}, nil).post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 409 {
		t.Errorf("no console: %d", rec.Code)
	}
	if rec := taclFx(t, true, nil, &fakePolicy{text: "{}"}).post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 422 {
		t.Errorf("no dst: %d", rec.Code)
	}
	if rec := taclFx(t, true, []string{"x"}, &fakePolicy{text: "{}", err: policy.ErrBusy}).post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 409 {
		t.Errorf("busy: %d", rec.Code)
	}
	if rec := taclFx(t, true, []string{"x"}, &fakePolicy{text: "{} // c"}).post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 422 {
		t.Errorf("hujson: %d", rec.Code)
	}
}
