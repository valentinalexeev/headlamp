package enroll

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/fly/node-control/internal/core"
)

// the remote address httptest requests carry
const testRemoteIP = "192.0.2.1"

// whoTailnet answers WhoIs from a fake and dials like the local tailnet, which the discovered agents need.
type whoTailnet struct {
	core.Tailnet
	who *authz.FakeTailnet
}

func (w whoTailnet) WhoIs(ctx context.Context, addr string) (string, []string, error) {
	return w.who.WhoIs(ctx, addr)
}

func auditFx(t *testing.T, who authz.Who, known bool) *multiFx {
	t.Helper()
	f := newMultiFx(t)
	f.withDiscovered(t)
	m := map[string]authz.Who{}
	if known {
		m[testRemoteIP] = who
	}
	f.tn = whoTailnet{core.NewLocal("127.0.0.1"), authz.NewFakeTailnet(m)}
	f.register()
	return f
}

func (f *multiFx) entries() []core.AuditEntry {
	f.t.Helper()
	got, err := f.mod.AuditLog().AuditLog(100)
	if err != nil {
		f.t.Fatal(err)
	}
	return got
}

func TestAuditRecordsDeviceActions(t *testing.T) {
	f := auditFx(t, authz.Who{Login: "operator@example.net"}, true)
	id, _ := f.create()
	steps := []struct {
		method, path, body string
	}{
		{"POST", fmt.Sprintf("/enroll/devices/%d/links", id), `{"ttl":3600,"max_uses":1}`},
		{"POST", fmt.Sprintf("/enroll/devices/%d/extend", id), fmt.Sprintf(`{"expires_at":%d}`, adminFuture+86400*60)},
		{"POST", fmt.Sprintf("/enroll/devices/%d/rotate-secret", id), `{}`},
		{"POST", "/enroll/ingresses/ingress-3/approve", "approve"},
		{"POST", "/enroll/ingresses/ingress-3/unapprove", `{}`},
		{"POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), `{}`},
		{"DELETE", fmt.Sprintf("/enroll/devices/%d", id), `{}`},
	}
	for _, s := range steps {
		if s.body == "approve" {
			s.body = f.approveBody("ingress-3")
		}
		if code, m := f.do(s.method, s.path, s.body); code != 200 {
			t.Fatalf("%s %s = %d %v", s.method, s.path, code, m)
		}
	}
	got := f.entries()
	want := []struct{ action, detail string }{
		{"device.delete", "Anna phone"},
		{"device.revoke", "Anna phone"},
		{"bridge.unapprove", "ingress-3"},
		{"bridge.approve", "ingress-3"},
		{"device.rotate_secret", "Anna phone"},
		{"device.extend", "Anna phone, expires 2027-04-15"},
		{"device.link", "Anna phone"},
		{"device.create", "Anna phone, expires 2027-02-14"},
	}
	if len(got) != len(want) {
		t.Fatalf("entries = %+v", got)
	}
	for i, w := range want {
		if got[i].Action != w.action || got[i].Actor != "operator@example.net" || got[i].Detail != w.detail {
			t.Errorf("entry %d = %+v, want %v", i, got[i], w)
		}
	}
}

func TestAuditWhoIsFailureRecordsUnknownActor(t *testing.T) {
	f := auditFx(t, authz.Who{}, false) // the fake answers with an error
	id, _ := f.create()
	if code, _ := f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), `{}`); code != 200 {
		t.Fatalf("revoke = %d", code)
	}
	got := f.entries()
	if len(got) != 2 || got[0].Actor != "unknown" || got[1].Actor != "unknown" {
		t.Fatalf("entries = %+v", got)
	}
}

func TestAuditTaggedCallerKeepsWhoIsLogin(t *testing.T) {
	f := auditFx(t, authz.Who{Login: "tagged-devices", Tags: []string{"tag:ci"}}, true)
	f.create()
	if got := f.entries(); len(got) != 1 || got[0].Actor != "tagged-devices" {
		t.Fatalf("entries = %+v", got)
	}
}

func TestAuditNeverHoldsSecrets(t *testing.T) {
	f := auditFx(t, authz.Who{Login: "operator@example.net"}, true)
	code, m := f.do("POST", "/enroll/devices", fmt.Sprintf(`{"name":"Anna phone","owner":"anna","client":"sing-box","expires_at":%d}`, adminFuture))
	if code != 200 {
		t.Fatal(code)
	}
	id := int64(m["id"].(float64))
	secrets := []string{m["subscription_url"].(string)}
	_, l := f.do("POST", fmt.Sprintf("/enroll/devices/%d/links", id), `{"ttl":3600,"max_uses":1}`)
	secrets = append(secrets, l["url"].(string))
	_, r := f.do("POST", fmt.Sprintf("/enroll/devices/%d/rotate-secret", id), `{}`)
	secrets = append(secrets, r["subscription_url"].(string))
	dev, _ := f.mod.store.GetDevice(id)
	for _, u := range dev.UUIDs {
		secrets = append(secrets, u)
	}
	for _, u := range secrets[:3] { // the secret and token parts of the URLs on their own
		secrets = append(secrets, u[strings.LastIndex(u, "/")+1:])
	}
	all, _ := json.Marshal(f.entries())
	for _, s := range secrets {
		if len(s) < 8 {
			t.Fatalf("test secret too short: %q", s)
		}
		if bytes.Contains(all, []byte(s)) {
			t.Errorf("the audit log holds %q", s)
		}
	}
}

// failingAudit makes every audit write fail.
type failingAudit struct{ Backend }

func (failingAudit) Audit(string, string, string) error { return errors.New("disk full") }

func TestAuditFailureDoesNotFailAction(t *testing.T) {
	f := auditFx(t, authz.Who{Login: "operator@example.net"}, true)
	f.mod.admin.store = failingAudit{f.mod.store}
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	code, m := f.do("POST", "/enroll/devices", fmt.Sprintf(`{"name":"Anna phone","owner":"anna","client":"sing-box","expires_at":%d}`, adminFuture))
	if code != 200 || m["subscription_url"] == nil {
		t.Fatalf("create = %d %v", code, m)
	}
	if !strings.Contains(buf.String(), "audit log write failed") || !strings.Contains(buf.String(), "level=ERROR") {
		t.Fatalf("no error logged: %q", buf.String())
	}
	if strings.Contains(buf.String(), "disk full") {
		t.Fatalf("raw error text logged: %q", buf.String())
	}
}

func TestExportOmitsAudit(t *testing.T) {
	f := auditFx(t, authz.Who{Login: "operator@example.net"}, true)
	f.create()
	if len(f.entries()) != 1 {
		t.Fatal("no audit entry")
	}
	snap, err := f.mod.store.(*Store).Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(snap)
	if bytes.Contains(b, []byte("audit")) || bytes.Contains(b, []byte("device.create")) || bytes.Contains(b, []byte("operator@example.net")) {
		t.Fatalf("snapshot holds the audit log: %s", b)
	}
}

func TestRedisAuditShowsAnEntryItCannotRead(t *testing.T) {
	clock := &fakeClock{now: time.Unix(1000, 0)}
	s, mr := newTestRedisStore(t, clock, testCipher(t))
	if err := s.Audit("alice", "device.create", "phone"); err != nil {
		t.Fatal(err)
	}
	if _, err := mr.Lpush(s.auditKey(), "not json"); err != nil {
		t.Fatal(err)
	}
	got, err := s.AuditLog(10)
	if err != nil || len(got) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	if got[0].Action != "audit.unreadable" || got[0].Detail != "" || !got[0].At.Equal(got[1].At) {
		t.Errorf("a gap shows where it is, dated by its neighbour, and repeats nothing of the entry: %+v", got[0])
	}
	if got[1].Action != "device.create" {
		t.Errorf("%+v", got[1])
	}
}
