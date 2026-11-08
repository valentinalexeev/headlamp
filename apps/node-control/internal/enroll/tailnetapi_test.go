package enroll

import (
	"fmt"
	"strings"
	"testing"
)

func grantsFx(t *testing.T, pol *fakePolicy) *adminFx {
	cfg := adminCfg
	cfg.Ingresses = append([]Ingress(nil), cfg.Ingresses...)
	cfg.Ingresses[0].TailnetAccess = TailnetGrants
	cfg.Targets = gTargets
	return taclFxCfg(t, cfg, []string{"tag:lan:*"}, pol)
}

func bridgeName(f *adminFx) string { return f.admin.reg.LinkConfig().Ingresses[0].Name }

func TestGrantAPIDeviceAndOwner(t *testing.T) {
	pol := &fakePolicy{text: `{"tagOwners":{"tag:ingress":["e2e@"]},"acls":[]}` + "\n"}
	f := grantsFx(t, pol)
	d1, _, _ := f.store.CreateDevice("one", "alice", "sing-box", adminNow+3600)
	d2, _, _ := f.store.CreateDevice("two", "carol", "sing-box", adminNow+3600)
	b := bridgeName(f)
	if rec := f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "device", "subject": fmt.Sprint(d1.ID), "targets": []string{"nas"}}); rec.Code != 200 {
		t.Fatalf("device grant: %d %s", rec.Code, rec.Body)
	}
	if rec := f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "owner", "subject": "carol", "targets": []string{"nas", "printer"}}); rec.Code != 200 {
		t.Fatalf("owner grant: %d", rec.Code)
	}
	if rec := f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "group", "subject": "group:ops", "targets": []string{"nas"}}); rec.Code != 200 {
		t.Fatalf("group grant: %d", rec.Code)
	}
	v := decode(t, f.do("GET", "/enroll/tailnet-grants", "", ""))
	br := v["bridges"].([]any)[0].(map[string]any)
	groups := br["groups"].([]any)
	if br["mode"] != TailnetGrants || len(groups) != 2 || groups[0].(map[string]any)["status"] != "needs_provisioning" {
		t.Fatalf("view %v", br)
	}
	if fmt.Sprint(br["unresolved"]) != "[group:ops]" {
		t.Errorf("unresolved %v", br["unresolved"])
	}
	// the push carries the group ids
	last := f.agent.pushes[len(f.agent.pushes)-1].creds
	ids := map[string]int{}
	for _, c := range last {
		ids[c.Tailnet]++
	}
	if ids[GroupID([]string{"nas"})] != len(d1.UUIDs) || ids[GroupID([]string{"nas", "printer"})] != len(d2.UUIDs) {
		t.Errorf("pushed %v", ids)
	}
	// preview, then one change with both group rules
	st := decode(t, f.do("GET", "/enroll/tailnet-acl", "", ""))
	if st["status"] != "out_of_sync" || len(st["changes"].([]any)) != 2 {
		t.Fatalf("preview %v", st)
	}
	if rec := f.post("/enroll/tailnet-acl", map[string]any{}); rec.Code != 202 || pol.changes != 1 {
		t.Fatalf("apply %d", rec.Code)
	}
	if !strings.Contains(pol.text, GroupTag(GroupID([]string{"nas"}))) || strings.Contains(pol.text, `"tag:lan:*"`) {
		t.Errorf("policy:\n%s", pol.text)
	}
	// revoke: the preview shows the removal
	f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "device", "subject": fmt.Sprint(d1.ID), "targets": []string{}})
	ch := decode(t, f.do("GET", "/enroll/tailnet-acl", "", ""))["changes"].([]any)
	if len(ch) != 1 || ch[0].(map[string]any)["op"] != "remove" {
		t.Errorf("after revoke %v", ch)
	}
	log, _ := f.store.AuditLog(20)
	n := 0
	for _, e := range log {
		if strings.HasPrefix(e.Action, "tailnet.") {
			n++
		}
	}
	if n != 4 {
		t.Errorf("audit entries %d", n)
	}
}

func TestGrantAPIRefusals(t *testing.T) {
	f := grantsFx(t, nil)
	b := bridgeName(f)
	for _, c := range []struct {
		body map[string]any
		code int
	}{
		{map[string]any{"bridge": "nope", "kind": "owner", "subject": "a", "targets": []string{"nas"}}, 404},
		{map[string]any{"bridge": b, "kind": "device", "subject": "999", "targets": []string{"nas"}}, 404},
		{map[string]any{"bridge": b, "kind": "user", "subject": "a", "targets": []string{"nas"}}, 422},
		{map[string]any{"bridge": b, "kind": "owner", "subject": "a", "targets": []string{"gone"}}, 422},
		{map[string]any{"bridge": b, "kind": "owner", "subject": "", "targets": []string{"nas"}}, 422},
	} {
		if rec := f.post("/enroll/tailnet-grants/subject", c.body); rec.Code != c.code {
			t.Errorf("%v: %d", c.body, rec.Code)
		}
	}
	if rec := f.post("/enroll/tailnet-grants/targets", map[string]any{"name": "nas", "dst": []string{"tag:x:1"}}); rec.Code != 409 {
		t.Errorf("config target changed: %d", rec.Code)
	}
	if rec := f.post("/enroll/tailnet-grants/targets", map[string]any{"name": "web", "dst": []string{"*:*"}}); rec.Code != 422 {
		t.Errorf("bad dst: %d", rec.Code)
	}
	if rec := f.post("/enroll/tailnet-grants/mode", map[string]any{"bridge": b, "mode": "some"}); rec.Code != 422 {
		t.Errorf("bad mode: %d", rec.Code)
	}
	if rec := f.post("/enroll/tailnet-grants/convert", map[string]any{"bridge": b}); rec.Code != 409 {
		t.Errorf("convert from grants: %d", rec.Code)
	}
}

func TestGrantAPIConsoleTargetLifecycle(t *testing.T) {
	f := grantsFx(t, nil)
	b := bridgeName(f)
	if rec := f.post("/enroll/tailnet-grants/targets", map[string]any{"name": "web", "dst": []string{"tag:web:443"}}); rec.Code != 200 {
		t.Fatalf("add: %d %s", rec.Code, rec.Body)
	}
	f.post("/enroll/tailnet-grants/subject", map[string]any{"bridge": b, "kind": "owner", "subject": "a", "targets": []string{"web"}})
	if rec := f.do("DELETE", "/enroll/tailnet-grants/targets/web", "application/json", "{}"); rec.Code != 200 {
		t.Fatalf("delete: %d", rec.Code)
	}
	doc, _ := f.store.TailnetDoc()
	if len(doc.Targets) != 0 || len(doc.Grants) != 0 {
		t.Errorf("left %+v", doc)
	}
}

func TestConvertToGrantsKeepsAccess(t *testing.T) {
	pol := &fakePolicy{text: `{"tagOwners":{"tag:ingress":["e2e@"]},"acls":[{"action":"accept","src":["tag:ingress"],"dst":["tag:lan:8000"]}]}` + "\n"}
	f := taclFx(t, true, []string{"tag:lan:8000"}, pol)
	d1, _, _ := f.store.CreateDevice("one", "alice", "sing-box", adminNow+3600)
	d2, _, _ := f.store.CreateDevice("two", "bob", "sing-box", adminNow+3600)
	_ = f.store.Revoke(d2.ID)
	b := bridgeName(f)
	if rec := f.post("/enroll/tailnet-grants/convert", map[string]any{"bridge": b}); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	doc, _ := f.store.TailnetDoc()
	if doc.Modes[b] != TailnetGrants || len(doc.Grants) != 1 || doc.Grants[0].Subject != fmt.Sprint(d1.ID) || doc.Targets[0].Name != "all" {
		t.Fatalf("doc %+v", doc)
	}
	// the phase 1 rule goes, one group rule with the same dst comes, in one change
	ch := decode(t, f.do("GET", "/enroll/tailnet-acl", "", ""))["changes"].([]any)
	if len(ch) != 2 {
		t.Fatalf("changes %v", ch)
	}
	if rec := f.post("/enroll/tailnet-grants/convert", map[string]any{"bridge": b}); rec.Code != 409 {
		t.Errorf("second convert: %d", rec.Code)
	}
}
