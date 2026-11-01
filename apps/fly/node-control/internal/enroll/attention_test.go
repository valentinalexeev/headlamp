package enroll

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

var _ core.AttentionSource = (*Module)(nil)

func attentionFor(f *regFx) *Module {
	return &Module{reg: f.reg, store: f.store, now: f.clk.now}
}

func attentionOf(t *testing.T, f *regFx) ([]core.AttentionItem, core.SystemLine) {
	t.Helper()
	items, lines, err := attentionFor(f).Attention(context.Background(), "alice@")
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Name != "Devices" || lines[0].Path != "/enroll" {
		t.Fatalf("lines: %+v", lines)
	}
	return items, lines[0]
}

func itemWithID(items []core.AttentionItem, id string) *core.AttentionItem {
	for i := range items {
		if items[i].ID == id {
			return &items[i]
		}
	}
	return nil
}

func TestEnrollAttentionQuiet(t *testing.T) {
	f := newRegFx(t, nil)
	f.store.CreateDevice("phone", "alice", "sing-box", adminFuture)
	items, line := attentionOf(t, f)
	if len(items) != 0 || line.State != "ok" || !strings.Contains(line.Summary, "1 active") {
		t.Fatalf("%+v %+v", items, line)
	}
}

func TestEnrollAttentionBridgeOutOfSync(t *testing.T) {
	f := newRegFx(t, nil)
	f.reg.Current(context.Background(), true).Targets[0].State.Failed("unreachable")
	items, line := attentionOf(t, f)
	it := itemWithID(items, "bridge-sync-ingress-1")
	if it == nil || len(items) != 1 {
		t.Fatalf("%+v", items)
	}
	if it.Level != core.LevelCritical || it.Source != "nodes" || !strings.Contains(it.Title, "ingress-1") ||
		!strings.Contains(it.Detail, "Revocations and extensions are not in effect there") ||
		!strings.Contains(it.Detail, "Retrying every 5 minutes.") ||
		it.ActionPath != "/nodes?sel=ingress-1" || it.Since.IsZero() {
		t.Errorf("%+v", *it)
	}
	if line.State != "critical" {
		t.Errorf("%+v", line)
	}
}

func TestEnrollAttentionStoppedBridgeIsNotAtRisk(t *testing.T) {
	f := newRegFx(t, nil)
	f.reg.Current(context.Background(), true).Targets[0].State.Failed("unreachable")
	m := attentionFor(f)
	m.stopped = func(context.Context) map[string]bool { return map[string]bool{"ingress-1": true} }
	items, lines, err := m.Attention(context.Background(), "alice@")
	if err != nil || len(items) != 1 || len(lines) != 1 {
		t.Fatalf("%+v %+v %v", items, lines, err)
	}
	it := items[0]
	if it.Level != core.LevelInfo || it.ID != "bridge-sync-ingress-1" || !strings.Contains(it.Title, "stopped") || !strings.Contains(it.Detail, "when it is started") {
		t.Errorf("%+v", it)
	}
	if lines[0].State != "ok" || !strings.Contains(lines[0].Summary, "1 bridge sync pending") {
		t.Errorf("%+v", lines[0])
	}

	m.stopped = func(context.Context) map[string]bool { return map[string]bool{"other": true} }
	items, lines, _ = m.Attention(context.Background(), "alice@")
	if len(items) != 1 || items[0].Level != core.LevelCritical || lines[0].State != "critical" {
		t.Errorf("a bridge not known to be stopped stays critical: %+v %+v", items, lines)
	}
}

func TestEnrollAttentionEachBridgeIsNamedInItsOwnItem(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))
	if err := f.approve("ingress-3"); err != nil {
		t.Fatal(err)
	}
	for _, tg := range f.reg.Current(context.Background(), true).SyncTargets {
		tg.State.Failed("unreachable")
	}
	items, _ := attentionOf(t, f)
	if itemWithID(items, "bridge-sync-ingress-1") == nil || itemWithID(items, "bridge-sync-ingress-3") == nil || len(items) != 2 {
		t.Fatalf("%+v", items)
	}
}

func TestEnrollAttentionApproval(t *testing.T) {
	a := newInfoAgent(t, "ingress-3")
	usePort(t, a)
	f := newRegFx(t, nil)
	f.nodes.set(ingressNode("ingress-3", true))

	items, line := attentionOf(t, f)
	it := itemWithID(items, "bridge-approval-ingress-3")
	if it == nil || len(items) != 1 {
		t.Fatalf("pending: %+v", items)
	}
	if it.Source != "nodes" || it.Level != core.LevelWaiting || it.ActionPath != "/enroll/approve?name=ingress-3" || !strings.Contains(it.Title, "ingress-3") {
		t.Errorf("%+v", *it)
	}
	if line.State != "waiting" {
		t.Errorf("%+v", line)
	}

	if err := f.approve("ingress-3"); err != nil {
		t.Fatal(err)
	}
	if items, _ = attentionOf(t, f); len(items) != 0 {
		t.Fatalf("approved: %+v", items)
	}

	a.set("public_key", "ANOTHER-KEY-0123456789")
	f.clk.advance(infoTTL + time.Second)
	items, line = attentionOf(t, f)
	it = itemWithID(items, "bridge-approval-ingress-3")
	if it == nil || len(items) != 1 {
		t.Fatalf("changed: %+v", items)
	}
	if it.Source != "nodes" || it.Level != core.LevelCritical || it.ActionPath != "/enroll/approve?name=ingress-3" || !strings.Contains(it.Detail, "identity") {
		t.Errorf("%+v", *it)
	}
	if line.State != "critical" {
		t.Errorf("%+v", line)
	}
}

func TestEnrollAttentionExpiringDevices(t *testing.T) {
	day := int64(86400)
	f := newRegFx(t, nil)
	f.store.CreateDevice("soon-1", "alice", "sing-box", adminNow+4*day)
	f.store.CreateDevice("soon-2", "alice", "sing-box", adminNow+7*day)
	f.store.CreateDevice("later", "alice", "sing-box", adminNow+8*day)
	f.store.CreateDevice("gone", "alice", "sing-box", adminNow-day)
	d, _, _ := f.store.CreateDevice("revoked", "alice", "sing-box", adminNow+2*day)
	f.store.Revoke(d.ID)

	items, line := attentionOf(t, f)
	if len(items) != 1 {
		t.Fatalf("want one item, got %+v", items)
	}
	it := items[0]
	if it.Source != "enroll" || it.Level != core.LevelWaiting || it.ActionPath != "/enroll?filter=expiring" || !strings.HasPrefix(it.Title, "2 devices expire within 7 days") ||
		!it.Deadline.Equal(time.Unix(adminNow+4*day, 0)) {
		t.Errorf("%+v", it)
	}
	if line.State != "waiting" {
		t.Errorf("%+v", line)
	}

	g := newRegFx(t, nil)
	g.store.CreateDevice("one", "alice", "sing-box", adminNow+day)
	if items, _ = attentionOf(t, g); len(items) != 1 || !strings.HasPrefix(items[0].Title, "1 device expires within 7 days") {
		t.Errorf("%+v", items)
	}
}

func TestEnrollAttentionInactiveModuleReportsNothing(t *testing.T) {
	items, lines, err := (&Module{}).Attention(context.Background(), "alice@")
	if err != nil || len(items) != 0 || len(lines) != 0 {
		t.Fatalf("%+v %+v %v", items, lines, err)
	}
}

var (
	_ core.ActivitySource = (*Module)(nil)
	_ core.SearchSource   = (*Module)(nil)
)

func TestEnrollActivityMapsDeviceAndBridgeActions(t *testing.T) {
	f := newRegFx(t, nil)
	for _, e := range [][3]string{
		{"alice@", "device.create", "phone, expires 2027-01-01"},
		{"alice@", "node.stop", "exit-a: ok"}, // the nodes module's entry in the shared log
		{"bob@", "bridge.approve", "ingress-1"},
	} {
		if err := f.store.Audit(e[0], e[1], e[2]); err != nil {
			t.Fatal(err)
		}
	}
	m := attentionFor(f)
	got, err := m.Activity(context.Background(), "alice@", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want the two device and bridge entries, got %+v", got)
	}
	for _, e := range got {
		if e.Source != "Devices" || e.At.IsZero() || e.Actor == "" {
			t.Errorf("%+v", e)
		}
	}
	if got[0].Action != "bridge.approve" || got[1].Action != "device.create" || got[1].Detail != "phone, expires 2027-01-01" {
		t.Errorf("newest first: %+v", got)
	}
	if one, _ := m.Activity(context.Background(), "alice@", 1); len(one) != 1 || one[0].Action != "bridge.approve" {
		t.Errorf("limit: %+v", one)
	}
	if !m.HasActivity() {
		t.Error("an active module has an activity log")
	}
}

func TestEnrollWithoutStoreGivesNothing(t *testing.T) {
	m := &Module{}
	if got, err := m.Activity(context.Background(), "alice@", 10); got != nil || err != nil {
		t.Errorf("%v %v", got, err)
	}
	if got, err := m.Search(context.Background(), "alice@"); got != nil || err != nil {
		t.Errorf("%v %v", got, err)
	}
	if m.HasActivity() {
		t.Error("an inactive module has no activity log")
	}
}

func TestEnrollSearchListsDeviceNames(t *testing.T) {
	f := newRegFx(t, nil)
	f.store.CreateDevice("phone-1", "alice", "sing-box", adminFuture)
	f.store.CreateDevice("tablet", "alice", "sing-box", adminFuture)
	items, err := attentionFor(f).Search(context.Background(), "alice@")
	if err != nil || len(items) != 2 {
		t.Fatalf("%v %+v", err, items)
	}
	for _, it := range items {
		if it.Kind != "Device" || it.Path != "/enroll" || it.Label == "" {
			t.Errorf("%+v", it)
		}
	}
}

// The Attention item, the Devices page and the Nodes page all say "Retrying every 5 minutes": change them with it.
func TestReconcilePeriodMatchesTheRetryCopy(t *testing.T) {
	if reconcilePeriod != 5*time.Minute {
		t.Fatalf("the pages promise a retry every 5 minutes, the loop waits %v", reconcilePeriod)
	}
}
