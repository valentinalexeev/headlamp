package enroll

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	adminNow    int64 = 1_800_000_000
	adminFuture       = adminNow + 86400*30
)

var adminCfg = singleOf(IngressCfg{Host: "ingress.example.test", PublicKey: "PUB", ShortID: "ab", SNI: "sni.example.test", Port: 443, ProfileName: "tunnel"})

// stubAgent is an Agent whose behaviour tests switch at will.
type stubAgent struct {
	mu     sync.Mutex
	down   bool
	fail   error
	pushes []put
	stats  map[string]any
}

func (a *stubAgent) PutCredentials(ctx context.Context, creds []Credential, confirmEmpty bool) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail != nil {
		return a.fail
	}
	if a.down {
		return &AgentUnavailable{Outcome: OutcomeUnreachable}
	}
	a.pushes = append(a.pushes, put{append([]Credential{}, creds...), confirmEmpty})
	return nil
}

func (a *stubAgent) Stats(ctx context.Context) map[string]any {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.down || a.stats == nil {
		return map[string]any{}
	}
	return a.stats
}

func (a *stubAgent) last() put {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pushes[len(a.pushes)-1]
}

type adminFx struct {
	t     *testing.T
	store *Store
	agent *stubAgent
	admin *Admin
	mux   *http.ServeMux
}

func newStore(t *testing.T, now int64) *Store {
	t.Helper()
	s, err := OpenStore(filepath.Join(t.TempDir(), "e.db"), testKey, func() time.Time { return time.Unix(now, 0) })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func newAdminFx(t *testing.T) *adminFx { return newAdminFxCfg(t, adminCfg) }

func newAdminFxCfg(t *testing.T, cfg IngressConfig) *adminFx {
	t.Helper()
	store := newStore(t, adminNow)
	agent := &stubAgent{}
	a := NewAdmin(store, cfg, agent)
	mux := http.NewServeMux()
	a.Register(mux)
	return &adminFx{t: t, store: store, agent: agent, admin: a, mux: mux}
}

func (f *adminFx) do(method, path, ctype, body string) *httptest.ResponseRecorder {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec
}

func (f *adminFx) post(path string, body any) *httptest.ResponseRecorder {
	f.t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		f.t.Fatal(err)
	}
	return f.do("POST", path, "application/json", string(b))
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON (%d): %q", rec.Code, rec.Body.String())
	}
	return m
}

func (f *adminFx) create(over map[string]any) *httptest.ResponseRecorder {
	body := map[string]any{"name": "Anna phone", "owner": "anna", "client": "sing-box", "expires_at": adminFuture}
	for k, v := range over {
		body[k] = v
	}
	return f.post("/enroll/devices", body)
}

func (f *adminFx) createID() int64 {
	f.t.Helper()
	rec := f.create(nil)
	if rec.Code != 200 {
		f.t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	return int64(decode(f.t, rec)["id"].(float64))
}

func idPath(id int64, tail string) string {
	return "/enroll/devices/" + strconv.FormatInt(id, 10) + "/" + tail
}

func syncOf(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	s, ok := m["sync"].(map[string]any)
	if !ok {
		t.Fatalf("no sync in %v", m)
	}
	return s
}

func TestCreatePushesThreeCredentials(t *testing.T) {
	f := newAdminFx(t)
	rec := f.create(nil)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content type %q", ct)
	}
	body := decode(t, rec)
	if body["sync_pending"] != false {
		t.Fatalf("%v", body)
	}
	if !strings.HasPrefix(body["subscription_url"].(string), "https://ingress.example.test/sub/") {
		t.Fatalf("%v", body["subscription_url"])
	}
	p := f.agent.last()
	if len(p.creds) != 3 || p.confirmEmpty {
		t.Fatalf("%+v", p)
	}
	devs, _ := f.store.ListDevices()
	want := map[string]bool{}
	for _, u := range devs[0].UUIDs {
		want[u] = true
	}
	for _, c := range p.creds {
		if !want[c.UUID] {
			t.Fatalf("pushed unknown uuid %s", c.UUID)
		}
	}
}

func TestRevokeReportsSyncPending(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()

	t.Run("pushes the set without the device and confirms empty", func(t *testing.T) {
		rec := f.post(idPath(id, "revoke"), map[string]any{})
		if rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
		p := f.agent.last()
		if len(p.creds) != 0 || !p.confirmEmpty {
			t.Fatalf("%+v", p)
		}
		b := decode(t, rec)
		if b["ok"] != true || b["sync_pending"] != false {
			t.Fatalf("%v", b)
		}
	})

	t.Run("reports a failed push as pending", func(t *testing.T) {
		g := newAdminFx(t)
		id := g.createID()
		g.agent.fail = &AgentUnavailable{Outcome: OutcomeRejected, Status: 503}
		b := decode(t, g.post(idPath(id, "revoke"), map[string]any{}))
		if b["sync_pending"] != true {
			t.Fatalf("%v", b)
		}
		s := syncOf(t, b)
		if s["pending"] != true || s["state"] != "rejected" || s["since"] != float64(adminNow) {
			t.Fatalf("%v", s)
		}
		list := decode(t, g.do("GET", "/enroll/devices", "", ""))
		if s := syncOf(t, list); s["pending"] != true || s["state"] != "rejected" || s["since"] != float64(adminNow) {
			t.Fatalf("%v", s)
		}
		if list["devices"].([]any)[0].(map[string]any)["revoked"] != true {
			t.Fatalf("%v", list)
		}
	})
}

func TestListReportsSyncStateForTheBanner(t *testing.T) {
	f := newAdminFx(t)
	r := decode(t, f.do("GET", "/enroll/devices", "", ""))
	if devs, ok := r["devices"].([]any); !ok || len(devs) != 0 {
		t.Fatalf("devices must be an empty array: %v", r)
	}
	s := syncOf(t, r)
	if s["pending"] != false || s["state"] != "unknown" || s["since"] != nil {
		t.Fatalf("%v", s)
	}
	if _, ok := s["since"]; !ok {
		t.Fatal("since must be present (null)")
	}
}

func TestExtendReportsSyncState(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	f.agent.fail = &AgentUnavailable{Outcome: OutcomeUnauthorized, Status: 401}
	b := decode(t, f.post(idPath(id, "extend"), map[string]any{"expires_at": adminFuture + 10}))
	if s := syncOf(t, b); s["state"] != "unauthorized" || s["pending"] != true || s["since"] != float64(adminNow) || b["sync_pending"] != true {
		t.Fatalf("%v", b)
	}
	f.agent.fail = nil
	b = decode(t, f.post(idPath(id, "extend"), map[string]any{"expires_at": adminFuture + 20}))
	if s := syncOf(t, b); s["state"] != "ok" || s["pending"] != false || s["since"] != nil || b["sync_pending"] != false {
		t.Fatalf("%v", b)
	}
}

func TestAgentDownOnCreateThenReconcile(t *testing.T) {
	f := newAdminFx(t)
	f.agent.down = true
	rec := f.create(nil)
	if rec.Code != 200 || decode(t, rec)["sync_pending"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if devs, _ := f.store.ListDevices(); len(devs) != 1 {
		t.Fatal("device not stored")
	}
	if len(f.agent.pushes) != 0 {
		t.Fatal("nothing should have been pushed")
	}
	f.agent.down = false
	if !f.admin.State().Snapshot().Pending {
		t.Fatal("state must be pending")
	}
	if !Sync(context.Background(), f.store, f.agent, f.admin.State()) {
		t.Fatal("reconcile failed")
	}
	if f.admin.State().Snapshot().Pending {
		t.Fatal("still pending")
	}
	if len(f.agent.last().creds) != 3 {
		t.Fatalf("%+v", f.agent.last())
	}
}

func TestExtendMovesNotAfter(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	n := adminFuture + 1000
	if rec := f.post(idPath(id, "extend"), map[string]any{"expires_at": n}); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	for _, c := range f.agent.last().creds {
		if c.NotAfter != n {
			t.Fatalf("%+v", c)
		}
	}
}

func TestLinkURL(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	rec := f.post(idPath(id, "links"), map[string]any{"ttl": 3600, "max_uses": 2})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	url := decode(t, rec)["url"].(string)
	const prefix = "https://ingress.example.test/e/"
	if !strings.HasPrefix(url, prefix) {
		t.Fatal(url)
	}
	d, err := f.store.ConsumeToken(strings.TrimPrefix(url, prefix))
	if err != nil || d == nil {
		t.Fatalf("token does not redeem: %v %v", d, err)
	}
}

func TestAdminRotateSecret(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	old, _ := f.store.SubscriptionSecret(id)
	rec := f.post(idPath(id, "rotate-secret"), map[string]any{})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u := decode(t, rec)["subscription_url"].(string)
	cur, _ := f.store.SubscriptionSecret(id)
	if strings.Contains(u, old) || !strings.Contains(u, cur) || old == cur {
		t.Fatalf("%s old=%s cur=%s", u, old, cur)
	}
	if n := len(f.agent.pushes); n != 1 {
		t.Fatalf("rotate must not push credentials, pushes=%d", n)
	}
}

func TestListLeaksNothingAndHasTraffic(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	link := decode(t, f.post(idPath(id, "links"), map[string]any{"ttl": 3600, "max_uses": 1}))["url"].(string)
	dev, _ := f.store.GetDevice(id)
	sum := sha256.Sum256([]byte(dev.UUIDs["auto"]))
	opaque := hex.EncodeToString(sum[:])[:8]
	f.agent.stats = map[string]any{opaque: map[string]any{"up": 5, "down": 7}}
	rec := f.do("GET", "/enroll/devices", "", "")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	raw := rec.Body.String()
	secret, _ := f.store.SubscriptionSecret(id)
	leaks := append([]string{secret, strings.Split(secret, ".")[1], link[strings.LastIndex(link, "/")+1:]}, mapValues(dev.UUIDs)...)
	for _, l := range leaks {
		if strings.Contains(raw, l) {
			t.Fatalf("response leaks %q", l)
		}
	}
	if strings.Contains(strings.ToLower(raw), "token") {
		t.Fatal("response mentions token")
	}
	row := decode(t, rec)["devices"].([]any)[0].(map[string]any)
	tr := row["traffic"].(map[string]any)
	if tr["up"] != float64(5) || tr["down"] != float64(7) {
		t.Fatalf("%v", row)
	}
	want := map[string]any{"id": float64(id), "name": "Anna phone", "owner": "anna", "client": "sing-box", "expires_at": float64(adminFuture), "revoked": false}
	for k, v := range want {
		if row[k] != v {
			t.Fatalf("%s = %v want %v", k, row[k], v)
		}
	}
	// Plus traffic and the three activity numbers (enrolled_at, enrol_count, last_active): times and a count, no secrets.
	for _, k := range []string{"enrolled_at", "enrol_count", "last_active"} {
		if v, ok := row[k].(float64); !ok || v != 0 {
			t.Fatalf("%s = %v, want 0 for a device nobody enrolled", k, row[k])
		}
	}
	if len(row) != len(want)+1+3 {
		t.Fatalf("unexpected fields: %v", row)
	}
}

func mapValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestListWithoutAgentStats(t *testing.T) {
	f := newAdminFx(t)
	f.createID()
	f.agent.down = true
	row := decode(t, f.do("GET", "/enroll/devices", "", ""))["devices"].([]any)[0].(map[string]any)
	v, ok := row["traffic"]
	if !ok || v != nil {
		t.Fatalf("traffic must be present and null: %v", row)
	}
}

func TestValidation422(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	pushes := len(f.agent.pushes)
	devices := func() int { d, _ := f.store.ListDevices(); return len(d) }

	t.Run("create", func(t *testing.T) {
		cases := []struct {
			name string
			over map[string]any
		}{
			{"unknown client", map[string]any{"client": "wireguard"}},
			{"expiry in the past", map[string]any{"expires_at": adminNow - 1}},
			{"expiry now", map[string]any{"expires_at": adminNow}},
			{"expiry not a number", map[string]any{"expires_at": "soon"}},
			{"expiry fraction", map[string]any{"expires_at": 1.5}},
			{"expiry bool", map[string]any{"expires_at": true}},
			{"empty name", map[string]any{"name": ""}},
			{"long name", map[string]any{"name": strings.Repeat("x", 101)}},
			{"empty owner", map[string]any{"owner": ""}},
			{"long owner", map[string]any{"owner": strings.Repeat("x", 101)}},
			{"name not a string", map[string]any{"name": 5}},
			{"missing client", map[string]any{"client": nil}},
		}
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				rec := f.create(c.over)
				if rec.Code != 422 {
					t.Fatalf("%d %s", rec.Code, rec.Body)
				}
				if _, ok := decode(t, rec)["detail"]; !ok {
					t.Fatalf("no detail: %s", rec.Body)
				}
			})
		}
		t.Run("limits are accepted", func(t *testing.T) {
			for _, over := range []map[string]any{
				{"name": strings.Repeat("я", 100)},
				{"owner": "o"},
				{"client": "karing"},
				{"client": "clash"},
			} {
				if rec := f.create(over); rec.Code != 200 {
					t.Fatalf("%v: %d %s", over, rec.Code, rec.Body)
				}
			}
		})
		t.Run("detail names the client rule", func(t *testing.T) {
			d := decode(t, f.create(map[string]any{"client": "wireguard"}))["detail"]
			if s, _ := d.(string); !strings.Contains(s, "client must be one of") {
				t.Fatalf("%v", d)
			}
		})
	})

	t.Run("links", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"ttl": 59, "max_uses": 1}, {"ttl": 604801, "max_uses": 1},
			{"ttl": 3600, "max_uses": 0}, {"ttl": 3600, "max_uses": 101},
			{"ttl": "x", "max_uses": 1}, {"max_uses": 1}, {"ttl": 3600},
		} {
			if rec := f.post(idPath(id, "links"), body); rec.Code != 422 {
				t.Fatalf("%v: %d", body, rec.Code)
			}
		}
		for _, body := range []map[string]any{{"ttl": 60, "max_uses": 1}, {"ttl": 604800, "max_uses": 100}} {
			if rec := f.post(idPath(id, "links"), body); rec.Code != 200 {
				t.Fatalf("%v: %d", body, rec.Code)
			}
		}
	})

	t.Run("extend", func(t *testing.T) {
		for _, body := range []map[string]any{{"expires_at": adminNow - 5}, {"expires_at": adminNow}, {}, {"expires_at": "x"}} {
			if rec := f.post(idPath(id, "extend"), body); rec.Code != 422 {
				t.Fatalf("%v: %d", body, rec.Code)
			}
		}
	})

	t.Run("malformed JSON", func(t *testing.T) {
		for _, b := range []string{"", "{", "[]", "null", `"x"`} {
			if rec := f.do("POST", "/enroll/devices", "application/json", b); rec.Code != 422 {
				t.Fatalf("%q: %d", b, rec.Code)
			}
		}
	})

	t.Run("path id", func(t *testing.T) {
		if rec := f.post("/enroll/devices/abc/revoke", map[string]any{}); rec.Code != 422 {
			t.Fatalf("%d", rec.Code)
		}
	})

	if devices() != 5 || len(f.agent.pushes) < pushes {
		t.Fatalf("rejected requests changed state: devices=%d", devices())
	}
}

func TestUnknownDevice404(t *testing.T) {
	f := newAdminFx(t)
	for _, c := range []struct {
		tail string
		body map[string]any
	}{
		{"links", map[string]any{"ttl": 3600, "max_uses": 1}},
		{"revoke", map[string]any{}},
		{"extend", map[string]any{"expires_at": adminFuture}},
		{"rotate-secret", map[string]any{}},
	} {
		t.Run(c.tail, func(t *testing.T) {
			rec := f.post(idPath(999, c.tail), c.body)
			if rec.Code != 404 {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			if decode(t, rec)["detail"] != "unknown device" {
				t.Fatalf("%s", rec.Body)
			}
		})
	}
	if len(f.agent.pushes) != 0 {
		t.Fatal("unknown device must not push")
	}
}

func TestJSONContentTypeRequired(t *testing.T) {
	f := newAdminFx(t)
	id := f.createID()
	form := "name=x&owner=y&client=sing-box&expires_at=1"
	t.Run("form create", func(t *testing.T) {
		if rec := f.do("POST", "/enroll/devices", "application/x-www-form-urlencoded", form); rec.Code != 422 {
			t.Fatalf("%d", rec.Code)
		}
	})
	t.Run("text plain revoke", func(t *testing.T) {
		if rec := f.do("POST", idPath(id, "revoke"), "text/plain", ""); rec.Code != 422 {
			t.Fatalf("%d", rec.Code)
		}
		if d, _ := f.store.GetDevice(id); d.Revoked {
			t.Fatal("device was revoked")
		}
	})
	t.Run("missing content type", func(t *testing.T) {
		if rec := f.do("POST", idPath(id, "rotate-secret"), "", "{}"); rec.Code != 422 {
			t.Fatalf("%d", rec.Code)
		}
	})
	for _, tail := range []string{"links", "revoke", "extend", "rotate-secret"} {
		t.Run("wrong type on "+tail, func(t *testing.T) {
			if rec := f.do("POST", idPath(id, tail), "text/plain", "{}"); rec.Code != 422 {
				t.Fatalf("%d", rec.Code)
			}
		})
	}
	t.Run("detail", func(t *testing.T) {
		rec := f.do("POST", "/enroll/devices", "text/plain", form)
		if decode(t, rec)["detail"] != "Content-Type must be application/json" {
			t.Fatalf("%s", rec.Body)
		}
	})
	t.Run("parameters and case are accepted", func(t *testing.T) {
		b, _ := json.Marshal(map[string]any{"name": "n", "owner": "o", "client": "sing-box", "expires_at": adminFuture})
		for _, ct := range []string{"application/json; charset=utf-8", "Application/JSON", " application/json "} {
			if rec := f.do("POST", "/enroll/devices", ct, string(b)); rec.Code != 200 {
				t.Fatalf("%q: %d", ct, rec.Code)
			}
		}
	})
	t.Run("content type is checked before the device", func(t *testing.T) {
		if rec := f.do("POST", idPath(999, "revoke"), "text/plain", ""); rec.Code != 422 {
			t.Fatalf("%d", rec.Code)
		}
	})
}

func TestRequestBodyLimit(t *testing.T) {
	f := newAdminFx(t)
	big := `{"name":"` + strings.Repeat("a", 2<<20) + `","owner":"o","client":"sing-box","expires_at":` + strconv.FormatInt(adminFuture, 10) + `}`
	rec := f.do("POST", "/enroll/devices", "application/json", big)
	if rec.Code != 413 && rec.Code != 422 {
		t.Fatalf("%d", rec.Code)
	}
	if devs, _ := f.store.ListDevices(); len(devs) != 0 {
		t.Fatal("oversized body created a device")
	}
	// just under the limit parses (and then fails validation on length, not on size)
	ok := `{"name":"` + strings.Repeat("a", 900<<10) + `","owner":"o","client":"sing-box","expires_at":1}`
	if rec := f.do("POST", "/enroll/devices", "application/json", ok); rec.Code != 422 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestPage(t *testing.T) {
	f := newAdminFx(t)
	rec := f.do("GET", "/enroll", "", "")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Fatal(ct)
	}
	body := rec.Body.String()
	for _, want := range []string{"<form", `id="syncbanner"`, "ingress host out of sync since"} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q", want)
		}
	}
	if !bytes.Equal(rec.Body.Bytes(), enrollHTML) {
		t.Fatal("page differs from the embedded file")
	}
}

func TestAdminWrongMethodIs405(t *testing.T) {
	f := newAdminFx(t)
	if rec := f.do("DELETE", "/enroll/devices", "", ""); rec.Code != 405 {
		t.Fatalf("%d", rec.Code)
	}
	if rec := f.do("GET", "/enroll/devices/1/revoke", "", ""); rec.Code != 405 {
		t.Fatalf("%d", rec.Code)
	}
}

func TestSyncSinceUsesStoreClock(t *testing.T) {
	f := newAdminFx(t)
	f.agent.down = true
	b := decode(t, f.create(nil))
	if syncOf(t, b)["since"] != float64(adminNow) {
		t.Fatalf("%v", b)
	}
}
