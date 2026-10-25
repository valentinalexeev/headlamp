package enroll

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/core"
)

const (
	token1 = "first-agent-token-value"
	token2 = "second-agent-token-value"
)

var moduleKey = strings.Repeat("k", 32)

// multiFx runs the module with two configured ingresses (ingress-1 keeps the stored credentials,
// ingress-2 gets derived ones) and, optionally, a discovered ingress-3.
type multiFx struct {
	t      *testing.T
	env    map[string]string
	cfg    string
	a1, a2 *fakeIngressAgent
	a3     *infoAgent
	nodes  *nodeList
	clk    *clock
	r      *core.Routes
	mod    *Module
}

func multiCfgJSON(agent2 string) string {
	return `{"profile_name":"tunnel","link_host":"links.example.com","ingresses":[
	 {"name":"ingress-1","host":"ingress-1.example.com","public_key":"PUBKEY-ONE","short_id":"aa11","uuids":"base"},
	 {"name":"ingress-2","host":"ingress-2.example.com","public_key":"PUBKEY-TWO","short_id":"bb22","uuids":"derived",
	  "agent_url":"` + agent2 + `","token_env":"AGENT_TOKEN_TWO"}]}`
}

func newMultiFx(t *testing.T) *multiFx {
	t.Helper()
	dir := t.TempDir()
	f := &multiFx{t: t, a1: newFakeIngressAgent(t), a2: newFakeIngressAgent(t), nodes: &nodeList{}, clk: &clock{}}
	f.clk.t.Store(adminNow)
	f.cfg = multiCfgJSON(f.a2.srv.URL)
	f.env = map[string]string{
		"ENROLL_DB":         filepath.Join(dir, "e.db"),
		"ENROLL_CONFIG":     filepath.Join(dir, "c.json"),
		"ENROLL_SECRET_KEY": moduleKey,
		"AGENT_URL":         f.a1.srv.URL,
		"AGENT_TOKEN":       token1,
		"AGENT_TOKEN_TWO":   token2,
	}
	return f
}

func (f *multiFx) register() {
	f.t.Helper()
	if err := os.WriteFile(f.env["ENROLL_CONFIG"], []byte(f.cfg), 0o600); err != nil {
		f.t.Fatal(err)
	}
	env := core.MapEnv(f.env)
	f.mod = New(env, WithNodeSource(f.nodes), withClock(f.clk.now)).(*Module)
	f.r = routesFor(env)
	if err := f.mod.Register(f.r); err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { f.mod.Stop(context.Background()) })
}

func (f *multiFx) do(method, path, body string) (int, map[string]any) {
	f.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == "POST" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	f.r.Admin.ServeHTTP(rec, req)
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m
}

func (f *multiFx) pub(path string) (int, string) {
	f.t.Helper()
	rec := httptest.NewRecorder()
	f.r.Public.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
	return rec.Code, rec.Body.String()
}

func (f *multiFx) create() (int64, string) {
	f.t.Helper()
	code, m := f.do("POST", "/enroll/devices", fmt.Sprintf(`{"name":"Anna phone","owner":"anna","client":"sing-box","expires_at":%d}`, adminFuture))
	if code != 200 {
		f.t.Fatalf("create %d %v", code, m)
	}
	return int64(m["id"].(float64)), m["subscription_url"].(string)
}

func ingressRows(t *testing.T, m map[string]any) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	list, ok := m["ingresses"].([]any)
	if !ok {
		t.Fatalf("no ingresses in %v", m)
	}
	for _, r := range list {
		row := r.(map[string]any)
		out[row["name"].(string)] = row
	}
	return out
}

func rowSync(row map[string]any) map[string]any { return row["sync"].(map[string]any) }

func pushedSet(t *testing.T, a *fakeIngressAgent) []Credential {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.puts) == 0 {
		t.Fatal("nothing was pushed")
	}
	var body struct {
		Credentials []Credential `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(a.puts[len(a.puts)-1]), &body); err != nil {
		t.Fatal(err)
	}
	return body.Credentials
}

func TestOldSingleHostConfigUnchangedBehaviour(t *testing.T) {
	agent := newFakeIngressAgent(t)
	e := newModEnv(t, agent.srv.URL)
	env := e.env()
	r := routesFor(env)
	mod := New(env, WithNodeSource(&nodeList{})).(*Module)
	if err := mod.Register(r); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop(context.Background())
	if n := len(mod.reg.Current(context.Background(), false).Targets); n != 1 {
		t.Fatalf("targets %d", n)
	}
	req := httptest.NewRequest("POST", "/enroll/devices", strings.NewReader(fmt.Sprintf(`{"name":"n","owner":"o","client":"sing-box","expires_at":%d}`, time.Now().Unix()+86400)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.Admin.ServeHTTP(rec, req)
	var created map[string]any
	json.Unmarshal(rec.Body.Bytes(), &created)
	if rec.Code != 200 || created["sync_pending"] != false {
		t.Fatalf("%d %v", rec.Code, created)
	}
	if s := created["sync"].(map[string]any); s["pending"] != false || s["state"] != "ok" || s["since"] != nil {
		t.Fatalf("%v", s)
	}
	if !strings.HasPrefix(created["subscription_url"].(string), "https://ingress.example.test/sub/") {
		t.Fatal(created["subscription_url"])
	}
	rows := ingressRows(t, created)
	if len(rows) != 1 || rows["default"]["host"] != "ingress.example.test" {
		t.Fatalf("%v", rows)
	}
	if agent.putCount() != 1 {
		t.Fatalf("pushes %d", agent.putCount())
	}
	agent.mu.Lock()
	if agent.auth[0] != "Bearer agent-token-value" {
		t.Fatalf("auth %q", agent.auth[0])
	}
	agent.mu.Unlock()

	rec = httptest.NewRecorder()
	r.Admin.ServeHTTP(rec, httptest.NewRequest("GET", "/enroll/devices", nil))
	var list map[string]any
	json.Unmarshal(rec.Body.Bytes(), &list)
	for _, k := range []string{"devices", "sync", "ingresses", "pending_ingresses", "stats_partial"} {
		if _, ok := list[k]; !ok {
			t.Fatalf("missing %q in %v", k, list)
		}
	}
	if p, ok := list["pending_ingresses"].([]any); !ok || len(p) != 0 || list["stats_partial"] != false {
		t.Fatalf("%v", list)
	}

	// the profile is the single-ingress profile, byte for byte
	secretURL := created["subscription_url"].(string)
	prec := httptest.NewRecorder()
	r.Public.ServeHTTP(prec, httptest.NewRequest("GET", secretURL[strings.Index(secretURL, "/sub/"):], nil))
	d, _ := mod.store.GetDevice(int64(created["id"].(float64)))
	want, err := RenderProfile(mod.reg.LinkConfig(), d)
	if err != nil || prec.Code != 200 || prec.Body.String() != string(want) {
		t.Fatalf("profile differs from the single-ingress rendering (%d)", prec.Code)
	}
	if strings.Contains(prec.Body.String(), "urltest") {
		t.Fatal("a single ingress must not get fail-over groups")
	}
}

func TestTwoConfiguredIngressesSyncIsolated(t *testing.T) {
	f := newMultiFx(t)
	f.register()
	id, subURL := f.create()
	dev, _ := f.mod.store.GetDevice(id)
	set1, set2 := pushedSet(t, f.a1), pushedSet(t, f.a2)
	if len(set1) != 3 || len(set2) != 3 {
		t.Fatalf("%d %d", len(set1), len(set2))
	}
	for i, c := range set1 {
		if c.UUID != dev.UUIDs[c.Mode] {
			t.Fatalf("ingress-1 must get the stored credentials: %+v", c)
		}
		want := DeriveUUID([]byte(moduleKey), "ingress-2", set2[i].Mode, dev.UUIDs[set2[i].Mode])
		if set2[i].UUID != want || set2[i].UUID == c.UUID {
			t.Fatalf("ingress-2 must get its own derived credentials: %+v", set2[i])
		}
	}
	f.a1.mu.Lock()
	f.a2.mu.Lock()
	if f.a1.auth[0] != "Bearer "+token1 || f.a2.auth[0] != "Bearer "+token2 {
		t.Fatalf("each agent gets its own token: %q %q", f.a1.auth[0], f.a2.auth[0])
	}
	f.a1.mu.Unlock()
	f.a2.mu.Unlock()

	code, body := f.pub(subURL[strings.Index(subURL, "/sub/"):])
	if code != 200 || !strings.Contains(body, "ingress-1.example.com") || !strings.Contains(body, "ingress-2.example.com") || !strings.Contains(body, "urltest") {
		t.Fatalf("%d: the profile must offer both ingresses with fail-over", code)
	}
	for _, c := range set2 { // the derived ingress's credentials are in the profile, the base ones are not offered to it
		if !strings.Contains(body, c.UUID) {
			t.Fatalf("derived credential missing from the profile")
		}
	}
}

func TestSecondIngressTokenFromNamedVariable(t *testing.T) {
	t.Run("token_env", func(t *testing.T) {
		f := newMultiFx(t)
		f.register()
		f.create()
		f.a2.mu.Lock()
		defer f.a2.mu.Unlock()
		if f.a2.auth[0] != "Bearer "+token2 {
			t.Fatal(f.a2.auth[0])
		}
	})
	t.Run("default variable AGENT_TOKEN_<NAME>", func(t *testing.T) {
		f := newMultiFx(t)
		delete(f.env, "AGENT_TOKEN_TWO")
		f.env["AGENT_TOKEN_INGRESS_2"] = "default-named-token"
		f.cfg = strings.Replace(f.cfg, `,"token_env":"AGENT_TOKEN_TWO"`, "", 1)
		f.register()
		f.create()
		f.a2.mu.Lock()
		defer f.a2.mu.Unlock()
		if f.a2.auth[0] != "Bearer default-named-token" {
			t.Fatal(f.a2.auth[0])
		}
	})
}

func TestMissingTokenVariableFailsStartup(t *testing.T) {
	f := newMultiFx(t)
	delete(f.env, "AGENT_TOKEN_TWO")
	os.WriteFile(f.env["ENROLL_CONFIG"], []byte(f.cfg), 0o600)
	env := core.MapEnv(f.env)
	err := New(env, WithNodeSource(f.nodes)).Register(routesFor(env))
	if err == nil || err.Error() != "ingress ingress-2: token variable AGENT_TOKEN_TWO is not set" {
		t.Fatalf("%v", err)
	}
	for _, secret := range []string{token1, token2, moduleKey} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("a value appears in the error: %v", err)
		}
	}
	if _, statErr := os.Stat(f.env["ENROLL_DB"]); statErr == nil {
		t.Fatal("the database was created before the configuration was validated")
	}
}

func TestRevokeNeverBlocksWhenOneIngressDown(t *testing.T) {
	oldWait := syncWait
	syncWait = 150 * time.Millisecond
	defer func() { syncWait = oldWait }()

	t.Run("ingress refusing connections", func(t *testing.T) {
		f := newMultiFx(t)
		f.register()
		id, _ := f.create()
		f.a2.srv.Close()
		start := time.Now()
		code, m := f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), "{}")
		if code != 200 || time.Since(start) > 3*time.Second {
			t.Fatalf("%d after %v", code, time.Since(start))
		}
		if m["sync_pending"] != true {
			t.Fatalf("%v", m)
		}
		rows := ingressRows(t, m)
		if s := rowSync(rows["ingress-1"]); s["pending"] != false || s["state"] != "ok" {
			t.Fatalf("the healthy ingress: %v", s)
		}
		if s := rowSync(rows["ingress-2"]); s["pending"] != true || s["state"] != "unreachable" || s["since"] == nil {
			t.Fatalf("the lagging ingress: %v", s)
		}
		if agg := m["sync"].(map[string]any); agg["pending"] != true || agg["state"] != "unreachable" {
			t.Fatalf("aggregate %v", agg)
		}
		// the healthy ingress really got the revocation
		if set := pushedSet(t, f.a1); len(set) != 0 {
			t.Fatalf("ingress-1 still holds %d credentials", len(set))
		}
	})
	t.Run("ingress that hangs", func(t *testing.T) {
		old := putTimeout
		putTimeout = 5 * time.Second
		defer func() { putTimeout = old }()
		release := make(chan struct{})
		var once sync.Once
		hang := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "PUT" {
				<-release
			}
			w.WriteHeader(204)
		}))
		defer hang.Close()
		defer once.Do(func() { close(release) })
		f := newMultiFx(t)
		f.cfg = multiCfgJSON(hang.URL)
		f.register()
		dev, _, err := f.mod.store.CreateDevice("n", "o", "sing-box", adminFuture)
		if err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		code, m := f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", dev.ID), "{}")
		if code != 200 || time.Since(start) > 3*time.Second {
			t.Fatalf("a hanging ingress blocked the revoke: %d after %v", code, time.Since(start))
		}
		if m["sync_pending"] != true {
			t.Fatalf("%v", m)
		}
		rows := ingressRows(t, m)
		if s := rowSync(rows["ingress-1"]); s["pending"] != false || s["state"] != "ok" {
			t.Fatalf("%v", s)
		}
		if s := rowSync(rows["ingress-2"]); s["pending"] != true {
			t.Fatalf("a push that has not been acknowledged must read as pending: %v", s)
		}
		once.Do(func() { close(release) })
	})
}

func TestLinkHostUsedInAdminLinks(t *testing.T) {
	f := newMultiFx(t)
	f.register()
	id, subURL := f.create()
	if !strings.HasPrefix(subURL, "https://links.example.com/sub/") {
		t.Fatal(subURL)
	}
	code, m := f.do("POST", fmt.Sprintf("/enroll/devices/%d/links", id), `{"ttl":3600,"max_uses":1}`)
	if code != 200 || !strings.HasPrefix(m["url"].(string), "https://links.example.com/e/") {
		t.Fatalf("%d %v", code, m)
	}
	code, m = f.do("POST", fmt.Sprintf("/enroll/devices/%d/rotate-secret", id), "{}")
	if code != 200 || !strings.HasPrefix(m["subscription_url"].(string), "https://links.example.com/sub/") {
		t.Fatalf("%d %v", code, m)
	}
}

func opaque(uuid string) string {
	sum := sha256.Sum256([]byte(uuid))
	return hex.EncodeToString(sum[:])[:8]
}

func TestStatsMergedAndPartialFlag(t *testing.T) {
	f := newMultiFx(t)
	f.register()
	id, _ := f.create()
	dev, _ := f.mod.store.GetDevice(id)
	auto := dev.UUIDs["auto"]
	f.a1.stat = fmt.Sprintf(`{%q:{"up":1,"down":2}}`, opaque(auto))
	f.a2.stat = fmt.Sprintf(`{%q:{"up":10,"down":20}}`, opaque(DeriveUUID([]byte(moduleKey), "ingress-2", "auto", auto)))

	code, m := f.do("GET", "/enroll/devices", "")
	if code != 200 {
		t.Fatal(code)
	}
	tr := m["devices"].([]any)[0].(map[string]any)["traffic"].(map[string]any)
	if tr["up"] != float64(11) || tr["down"] != float64(22) || m["stats_partial"] != false {
		t.Fatalf("%v partial=%v", tr, m["stats_partial"])
	}

	// ingress-2 goes down: its figures are missing and the response says so
	f.a2.srv.Close()
	f.do("POST", fmt.Sprintf("/enroll/devices/%d/extend", id), fmt.Sprintf(`{"expires_at":%d}`, adminFuture+10))
	_, m = f.do("GET", "/enroll/devices", "")
	tr = m["devices"].([]any)[0].(map[string]any)["traffic"].(map[string]any)
	if tr["up"] != float64(1) || tr["down"] != float64(2) || m["stats_partial"] != true {
		t.Fatalf("%v partial=%v", tr, m["stats_partial"])
	}
}

func TestOnlineFlagInTheDeviceList(t *testing.T) {
	f := newMultiFx(t)
	f.register()
	f.nodes.set(ingressNode("ingress-1", true), ingressNode("ingress-2", false))
	_, m := f.do("GET", "/enroll/devices", "")
	rows := ingressRows(t, m)
	if rows["ingress-1"]["online"] != true || rows["ingress-2"]["online"] != false {
		t.Fatalf("%v", rows)
	}
	f.nodes.set()
	_, m = f.do("GET", "/enroll/devices", "")
	if ingressRows(t, m)["ingress-1"]["online"] != nil {
		t.Fatal("no node: online must be null")
	}
}

// --- discovery through the admin API and the public profile ---

func (f *multiFx) withDiscovered(t *testing.T) {
	t.Helper()
	f.a3 = newInfoAgent(t, "ingress-3")
	usePort(t, f.a3)
	f.env["AGENT_TOKEN_INGRESS_3"] = discToken
	f.nodes.set(ingressNode("ingress-3", true))
}

func (f *multiFx) fingerprintOf(name string) string {
	f.t.Helper()
	_, m := f.do("GET", "/enroll/ingresses", "")
	for _, r := range m["ingresses"].([]any) {
		row := r.(map[string]any)
		if row["name"] == name {
			fp, _ := row["fingerprint"].(string)
			return fp
		}
	}
	f.t.Fatalf("no row for %s in %v", name, m)
	return ""
}

func TestIngressListEndpoint(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	f.create()
	code, m := f.do("GET", "/enroll/ingresses", "")
	if code != 200 {
		t.Fatal(code)
	}
	rows := ingressRows(t, m)
	if len(rows) != 3 || rows["ingress-1"]["approval"] != "configured" || rows["ingress-3"]["approval"] != "pending" || rows["ingress-3"]["active"] != false {
		t.Fatalf("%v", rows)
	}
	if rows["ingress-3"]["sync"] != nil || rows["ingress-1"]["sync"] == nil {
		t.Fatal("only active ingresses have a sync state")
	}
	if d := m["discovery"].(map[string]any); d["enabled"] != true {
		t.Fatalf("%v", d)
	}
	_, dl := f.do("GET", "/enroll/devices", "")
	if p := dl["pending_ingresses"].([]any); len(p) != 1 || p[0] != "ingress-3" {
		t.Fatalf("%v", dl["pending_ingresses"])
	}
	if len(dl["ingresses"].([]any)) != 2 {
		t.Fatal("the device list carries the active ingresses only")
	}
}

func TestPendingIngressNotInProfileOrSyncUntilApproved(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	id, subURL := f.create()
	sub := subURL[strings.Index(subURL, "/sub/"):]
	_, body := f.pub(sub)
	if strings.Contains(body, "ingress-3.example.com") || strings.Contains(body, discKey) {
		t.Fatal("a pending ingress is in the profile")
	}
	if f.a3.putCount() != 0 {
		t.Fatal("a pending ingress received credentials")
	}

	fp := f.fingerprintOf("ingress-3")
	code, m := f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, fp))
	if code != 200 {
		t.Fatalf("%d %v", code, m)
	}
	// effective at once: the next profile has it, and the approval triggered a sync of the full set
	_, body = f.pub(sub)
	if !strings.Contains(body, "ingress-3.example.com") || !strings.Contains(body, discKey) {
		t.Fatal("an approved ingress is not in the profile")
	}
	if f.a3.putCount() != 1 || len(f.a3.lastPut()) != 3 {
		t.Fatalf("pushes %d", f.a3.putCount())
	}
	dev, _ := f.mod.store.GetDevice(id)
	if f.a3.lastPut()[0].UUID != DeriveUUID([]byte(moduleKey), "ingress-3", f.a3.lastPut()[0].Mode, dev.UUIDs[f.a3.lastPut()[0].Mode]) {
		t.Fatal("the discovered ingress must get derived credentials")
	}
	if rows := ingressRows(t, m); rowSync(rows["ingress-3"])["state"] != "ok" {
		t.Fatalf("the approval response lists the new ingress as synced: %v", rows["ingress-3"])
	}
	if _, l := f.do("GET", "/enroll/ingresses", ""); ingressRows(t, l)["ingress-3"]["approval"] != "approved" || ingressRows(t, l)["ingress-3"]["approved"] != true {
		t.Fatalf("%v", l)
	}

	// removing the approval takes it out of the profile and stops syncing to it
	n := f.a3.putCount()
	code, _ = f.do("POST", "/enroll/ingresses/ingress-3/unapprove", "{}")
	if code != 200 {
		t.Fatal(code)
	}
	_, body = f.pub(sub)
	if strings.Contains(body, "ingress-3.example.com") {
		t.Fatal("an un-approved ingress is still in the profile")
	}
	f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), "{}")
	if f.a3.putCount() != n {
		t.Fatal("an un-approved ingress is still synced")
	}
}

func TestApproveEndpointRules(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	fp := f.fingerprintOf("ingress-3")
	cases := []struct {
		name, path, ctype, body string
		want                    int
	}{
		{"mismatching fingerprint", "/enroll/ingresses/ingress-3/approve", "application/json", `{"fingerprint":"` + strings.Repeat("0", 64) + `"}`, 409},
		{"unknown ingress", "/enroll/ingresses/ingress-9/approve", "application/json", `{"fingerprint":"` + fp + `"}`, 404},
		{"configured ingress", "/enroll/ingresses/ingress-1/approve", "application/json", `{"fingerprint":"x"}`, 409},
		{"missing fingerprint", "/enroll/ingresses/ingress-3/approve", "application/json", `{}`, 422},
		{"not JSON content type", "/enroll/ingresses/ingress-3/approve", "text/plain", `{"fingerprint":"` + fp + `"}`, 422},
		{"unapprove needs JSON too", "/enroll/ingresses/ingress-3/unapprove", "text/plain", `{}`, 422},
		{"unapprove unknown", "/enroll/ingresses/ingress-9/unapprove", "application/json", `{}`, 404},
		{"unapprove configured", "/enroll/ingresses/ingress-1/unapprove", "application/json", `{}`, 409},
		{"invalid name", "/enroll/ingresses/Ingress_3/approve", "application/json", `{"fingerprint":"` + fp + `"}`, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", c.ctype)
			rec := httptest.NewRecorder()
			f.r.Admin.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
		})
	}
	if got, _ := f.mod.store.Approvals(); len(got) != 0 {
		t.Fatalf("nothing above may approve: %v", got)
	}
	if code, _ := f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, fp)); code != 200 {
		t.Fatal(code)
	}
}

func TestPendingListLeaksNoFullKeyMaterial(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	f.create()
	for _, p := range []string{"/enroll/ingresses", "/enroll/devices"} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		f.r.Admin.ServeHTTP(rec, req)
		body := rec.Body.String()
		for _, s := range []string{discKey, discSID, discToken, token1, token2} {
			if strings.Contains(body, s) {
				t.Fatalf("%s leaks %q", p, s)
			}
		}
		if p == "/enroll/ingresses" && (!strings.Contains(body, discKey[:6]) || !strings.Contains(body, discSID[:6])) {
			t.Fatalf("the prefixes are missing: %s", body)
		}
	}
}

func TestApprovalsPersistAcrossRestart(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	fp := f.fingerprintOf("ingress-3")
	if code, _ := f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, fp)); code != 200 {
		t.Fatal(code)
	}
	f.mod.Stop(context.Background())

	f.register() // the same database, a fresh process
	if _, m := f.do("GET", "/enroll/ingresses", ""); ingressRows(t, m)["ingress-3"]["approval"] != "approved" {
		t.Fatalf("%v", m)
	}
	// and it is used straight away: part of the next profile
	_, subURL := f.create()
	if _, body := f.pub(subURL[strings.Index(subURL, "/sub/"):]); !strings.Contains(body, "ingress-3.example.com") {
		t.Fatal("approved ingress not in the profile after a restart")
	}
}

func TestChangedParametersLeaveProfileThroughTheModule(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	_, subURL := f.create()
	sub := subURL[strings.Index(subURL, "/sub/"):]
	f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, f.fingerprintOf("ingress-3")))
	f.a3.set("public_key", "CHANGED-KEY-0123456789")
	f.clk.advance(infoTTL + time.Second)
	if _, body := f.pub(sub); strings.Contains(body, "ingress-3.example.com") || strings.Contains(body, "CHANGED-KEY") {
		t.Fatal("a changed ingress stays in the profile")
	}
	_, m := f.do("GET", "/enroll/ingresses", "")
	if ingressRows(t, m)["ingress-3"]["approval"] != "changed" {
		t.Fatalf("%v", m)
	}
	n := f.a3.putCount()
	f.create()
	if f.a3.putCount() != n {
		t.Fatal("a changed ingress still receives credentials")
	}
}

func TestDiscoveryOffWithoutLinkHostThroughTheModule(t *testing.T) {
	agent := newFakeIngressAgent(t)
	e := newModEnv(t, agent.srv.URL) // the old single-host file: no link_host key
	a3 := newInfoAgent(t, "ingress-3")
	usePort(t, a3)
	e.m["AGENT_TOKEN_INGRESS_3"] = discToken
	nodes := &nodeList{}
	nodes.set(ingressNode("ingress-3", true))
	env := e.env()
	r := routesFor(env)
	mod := New(env, WithNodeSource(nodes)).(*Module)
	if err := mod.Register(r); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop(context.Background())
	rec := httptest.NewRecorder()
	r.Admin.ServeHTTP(rec, httptest.NewRequest("GET", "/enroll/ingresses", nil))
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	d := m["discovery"].(map[string]any)
	if d["enabled"] != false || !strings.Contains(d["reason"].(string), "link_host") || len(m["ingresses"].([]any)) != 1 || a3.hits() != 0 {
		t.Fatalf("%v hits=%d", m, a3.hits())
	}
}

func TestHangingInfoDoesNotDelayARevoke(t *testing.T) {
	oldWait := syncWait
	syncWait = 300 * time.Millisecond
	defer func() { syncWait = oldWait }()
	oldTO := infoTimeout
	infoTimeout = 5 * time.Second
	defer func() { infoTimeout = oldTO }()
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	id, _ := f.create()
	f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, f.fingerprintOf("ingress-3")))
	release := make(chan struct{})
	f.a3.mu.Lock()
	f.a3.handler = func(w http.ResponseWriter, r *http.Request) { <-release }
	f.a3.mu.Unlock()
	t.Cleanup(func() { close(release) })
	f.clk.advance(infoTTL + time.Second) // the next reading is due, and it hangs
	start := time.Now()
	code, m := f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), "{}")
	if code != 200 || time.Since(start) > 2*time.Second || m["sync_pending"] == nil {
		t.Fatalf("a hanging /v1/info delayed the revoke: %d after %v", code, time.Since(start))
	}
}

func TestRevokeReachesApprovedIngressRightAfterARestart(t *testing.T) {
	f := newMultiFx(t)
	f.withDiscovered(t)
	f.register()
	id, _ := f.create()
	f.do("POST", "/enroll/ingresses/ingress-3/approve", fmt.Sprintf(`{"fingerprint":%q}`, f.fingerprintOf("ingress-3")))
	f.mod.Stop(context.Background())

	f.register() // a fresh process: no reading of /v1/info has happened yet
	before := f.a3.putCount()
	code, _ := f.do("POST", fmt.Sprintf("/enroll/devices/%d/revoke", id), "{}")
	if code != 200 || f.a3.putCount() != before+1 || len(f.a3.lastPut()) != 0 {
		t.Fatalf("%d: the revocation did not reach the approved ingress (pushes %d -> %d)", code, before, f.a3.putCount())
	}
}
