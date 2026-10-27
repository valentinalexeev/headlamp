package funnel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/policy"
)

const (
	adminIP    = "100.64.0.1"
	bobIP      = "100.64.0.2"
	taggedIP   = "100.64.0.3"
	nobodyIP   = "100.64.0.4"
	whoisErrIP = "100.64.0.5"
)

const startPolicy = `{
  "tagOwners": {"tag:funnel": ["group:admins"], "tag:funnel-ingress": ["group:admins"]},
  "acls": [{"action": "accept", "src": ["tag:funnel"], "dst": ["tag:funnel-ingress:9100"]}]
}
`

// fakePolicy is a policy service that keeps the text in memory and allows one pending change.
type fakePolicy struct {
	mu       sync.Mutex
	text     string
	nextID   int64
	pending  bool
	outcomes map[int64]policy.Outcome
	changes  []string
	failWith error
}

func newFakePolicy(text string) *fakePolicy {
	return &fakePolicy{text: text, outcomes: map[int64]policy.Outcome{}}
}

func (f *fakePolicy) Current(context.Context) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.text, "stamp", nil
}

func (f *fakePolicy) Change(_ context.Context, actor string, transform func(string) (string, error), reason string) (int64, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failWith != nil {
		return 0, time.Time{}, f.failWith
	}
	if f.pending {
		return 0, time.Time{}, policy.ErrBusy
	}
	next, err := transform(f.text)
	if err != nil {
		return 0, time.Time{}, err
	}
	if next == f.text {
		return 0, time.Time{}, policy.ErrNoChange
	}
	f.text = next
	f.nextID++
	f.pending = true
	f.outcomes[f.nextID] = policy.OutcomePending
	f.changes = append(f.changes, actor+": "+reason)
	return f.nextID, time.Now().Add(2 * time.Minute), nil
}

func (f *fakePolicy) Outcome(id int64) policy.Outcome {
	f.mu.Lock()
	defer f.mu.Unlock()
	if o, ok := f.outcomes[id]; ok {
		return o
	}
	return policy.OutcomeUnknown
}

// end settles the pending change: confirmed keeps the text, reverted restores `before`.
func (f *fakePolicy) end(id int64, o policy.Outcome, before string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outcomes[id] = o
	f.pending = false
	if o == policy.OutcomeReverted {
		f.text = before
	}
}

type fakeHS struct {
	mu        sync.Mutex
	users     []headscale.User
	nodes     []headscale.Node
	keys      []headscale.PreAuthKey
	created   []createCall
	expired   []uint64
	listErr   error
	nextKeyID uint64
}

type createCall struct {
	user      uint64
	tags      []string
	ttl       time.Duration
	ephemeral bool
	reusable  bool
}

func (h *fakeHS) ListNodes(context.Context) ([]headscale.Node, error) { return h.nodes, h.listErr }
func (h *fakeHS) ListUsers(context.Context) ([]headscale.User, error) { return h.users, h.listErr }
func (h *fakeHS) CreatePreAuthKey(_ context.Context, user uint64, tags []string, ttl time.Duration, eph, reuse bool) (headscale.PreAuthKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextKeyID++
	h.created = append(h.created, createCall{user, append([]string(nil), tags...), ttl, eph, reuse})
	k := headscale.PreAuthKey{ID: 100 + h.nextKeyID, Key: fmt.Sprintf("tskey-auth-SECRET%d", h.nextKeyID), UserID: user, Ephemeral: eph, Reusable: reuse,
		Expiration: time.Now().Add(ttl), ACLTags: tags}
	stored := k
	stored.Key = ""
	h.keys = append(h.keys, stored)
	return k, nil
}
func (h *fakeHS) ListPreAuthKeys(context.Context) ([]headscale.PreAuthKey, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]headscale.PreAuthKey(nil), h.keys...), h.listErr
}
func (h *fakeHS) ExpirePreAuthKey(_ context.Context, id uint64) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.expired = append(h.expired, id)
	return nil
}

type rig struct {
	t      *testing.T
	h      http.Handler
	mod    *Module
	st     Store
	pol    *fakePolicy
	hs     *fakeHS
	logBuf *bytes.Buffer
}

func funnelEnv(dir string, extra map[string]string) core.Env {
	m := map[string]string{
		"FUNNEL_DB": filepath.Join(dir, "funnel.db"), "FUNNEL_PUBLIC_DOMAIN": "example.com", "FUNNEL_INGRESS_HOSTNAME": "my-ingress.fly.dev",
		"FUNNEL_KEY_USER": "_svcs", "FUNNEL_ADMIN_USERS": "alice", "FUNNEL_TAG_OWNERS": "group:admins",
		"ENROLL_ALLOWED_HOSTS": "node-control.example.com", "HEADSCALE_LOGIN_SERVER": "https://hs.example.com", "HEADSCALE_API_KEY": "k",
	}
	for k, v := range extra {
		m[k] = v
	}
	return core.MapEnv(m)
}

func newRig(t *testing.T, extra map[string]string) *rig {
	t.Helper()
	pol := newFakePolicy(startPolicy)
	hs := &fakeHS{
		users: []headscale.User{{ID: 7, Name: "_svcs"}, {ID: 8, Name: "other"}},
		nodes: []headscale.Node{
			{ID: 1, Name: "pdl-box", GivenName: "pdl-box", Tags: []string{"tag:funnel", "tag:ns-pdl"}, IPAddresses: []string{"100.64.1.1"}},
			{ID: 2, Name: "plain", GivenName: "plain", Tags: []string{"tag:other"}, IPAddresses: []string{"100.64.1.2"}},
		},
	}
	tn := authz.NewFakeTailnet(map[string]authz.Who{
		adminIP:    {Login: "alice@"},
		bobIP:      {Login: "bob@"},
		taggedIP:   {Login: "tagged-devices", Tags: []string{"tag:funnel-ingress"}},
		nobodyIP:   {Login: ""},
		whoisErrIP: {Err: errors.New("boom")},
	})
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	mod := New(funnelEnv(t.TempDir(), extra), hs, func() PolicyService { return pol }).(*Module)
	mux := http.NewServeMux()
	if err := mod.Register(&core.Routes{Admin: mux, Env: funnelEnv(t.TempDir(), extra), Tailnet: tn, Log: log}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { mod.Stop(context.Background()) })
	return &rig{t: t, h: mux, mod: mod, st: mod.st, pol: pol, hs: hs, logBuf: logBuf}
}

type resp struct {
	code int
	body string
	hdr  http.Header
}

func (r resp) json(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.body), &m); err != nil {
		t.Fatalf("not JSON (%d): %q", r.code, r.body)
	}
	return m
}

func (r *rig) do(method, path, from string, body any) resp {
	var rd *bytes.Reader
	if body == nil {
		rd = bytes.NewReader(nil)
	} else if s, ok := body.(string); ok {
		rd = bytes.NewReader([]byte(s))
	} else {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rd)
	req.RemoteAddr = from + ":5000"
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return resp{w.Code, w.Body.String(), w.Result().Header}
}

func (r *rig) admin(method, path string, body any) resp { return r.do(method, path, adminIP, body) }

func (r *rig) createNS(name string, ports []rangeJSON) resp {
	return r.admin("POST", "/funnel/namespaces", map[string]any{"name": name, "ports": ports, "note": "n"})
}

// activate creates a namespace and confirms its ACL change.
func (r *rig) activate(name string, ports []rangeJSON) {
	r.t.Helper()
	before := r.pol.text
	res := r.createNS(name, ports)
	if res.code != 202 {
		r.t.Fatalf("create %s: %d %s", name, res.code, res.body)
	}
	id := int64(res.json(r.t)["change_id"].(float64))
	r.pol.end(id, policy.OutcomeConfirmed, before)
	if res := r.admin("GET", "/funnel/state", nil); res.code != 200 { // settles
		r.t.Fatal(res.code)
	}
}

var p9000 = []rangeJSON{{9000, 9010}}

func TestCreateNamespaceSubmitsComposedACLChangeAndStoresPending(t *testing.T) {
	r := newRig(t, nil)
	res := r.createNS("pdl", p9000)
	if res.code != 202 {
		t.Fatalf("%d %s", res.code, res.body)
	}
	m := res.json(t)
	if m["change_id"].(float64) != 1 || m["deadline"] == "" {
		t.Fatalf("%v", m)
	}
	want, err := Compose(startPolicy, map[string][]Range{"pdl": {{9000, 9010}}}, r.mod.s)
	if err != nil || r.pol.text != want {
		t.Fatalf("the submitted policy is not Compose of the current text:\n%s\nwant\n%s", r.pol.text, want)
	}
	nss, _ := r.st.Namespaces()
	if len(nss) != 1 || nss[0].State != StatePending || nss[0].ChangeID != 1 {
		t.Fatalf("namespaces = %+v", nss)
	}
	if len(r.pol.changes) != 1 || !strings.HasPrefix(r.pol.changes[0], "alice: ") {
		t.Fatalf("changes = %v", r.pol.changes)
	}
	// Not served until confirmed.
	doc, _ := BuildConfig(r.st, r.mod.s, r.mod.outcome)
	if len(doc.Namespaces) != 0 {
		t.Fatal("a pending namespace was served")
	}
}

func TestCreateNamespaceWhenPolicyNotPlainJSONIs422NothingStored(t *testing.T) {
	r := newRig(t, nil)
	r.pol.text = "{\n// hand-written comment\n\"acls\": []}"
	res := r.createNS("pdl", p9000)
	if res.code != 422 || !strings.Contains(res.json(t)["detail"].(string), "plain JSON") {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if nss, _ := r.st.Namespaces(); len(nss) != 0 {
		t.Fatal("a namespace was stored although the ACL change failed")
	}
	if r.pol.nextID != 0 {
		t.Fatal("a policy change was submitted")
	}
}

func TestCreateNamespaceWhileAnotherPolicyChangePendingIs409(t *testing.T) {
	r := newRig(t, nil)
	r.pol.pending = true
	res := r.createNS("pdl", p9000)
	if res.code != 409 {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if nss, _ := r.st.Namespaces(); len(nss) != 0 {
		t.Fatal("stored despite the busy policy")
	}
}

func TestCreateNamespaceValidation(t *testing.T) {
	r := newRig(t, nil)
	for name, body := range map[string]any{
		"empty name":      map[string]any{"name": "", "ports": p9000},
		"upper":           map[string]any{"name": "PDL", "ports": p9000},
		"no ports":        map[string]any{"name": "pdl", "ports": []rangeJSON{}},
		"bad range":       map[string]any{"name": "pdl", "ports": []rangeJSON{{10, 5}}},
		"zero":            map[string]any{"name": "pdl", "ports": []rangeJSON{{0, 5}}},
		"reserved proxy":  map[string]any{"name": "pdl", "ports": []rangeJSON{{8000, 8100}}},
		"reserved api":    map[string]any{"name": "pdl", "ports": []rangeJSON{{9100, 9100}}},
		"long note":       map[string]any{"name": "pdl", "ports": p9000, "note": strings.Repeat("x", 201)},
		"not an object":   `[1]`,
		"wrong port type": `{"name":"pdl","ports":"80"}`,
	} {
		if res := r.admin("POST", "/funnel/namespaces", body); res.code != 422 {
			t.Errorf("%s: %d %s", name, res.code, res.body)
		}
	}
	if r.pol.nextID != 0 {
		t.Fatal("an invalid request reached the policy")
	}
	if res := r.createNS("pdl", p9000); res.code != 202 {
		t.Fatal(res.code)
	}
	if res := r.createNS("pdl", p9000); res.code != 409 {
		t.Errorf("duplicate: %d", res.code)
	}
}

func TestFailedNamespaceCanBeCreatedAgain(t *testing.T) {
	r := newRig(t, nil)
	before := r.pol.text
	res := r.createNS("pdl", p9000)
	r.pol.end(int64(res.json(t)["change_id"].(float64)), policy.OutcomeReverted, before)
	r.admin("GET", "/funnel/state", nil)
	if nss, _ := r.st.Namespaces(); nss[0].State != StateFailed {
		t.Fatalf("state = %s", nss[0].State)
	}
	if res := r.createNS("pdl", p9000); res.code != 202 {
		t.Fatalf("retry: %d %s", res.code, res.body)
	}
}

func TestCreateNamespaceWhenACLAlreadyThereIsActiveAtOnce(t *testing.T) {
	r := newRig(t, nil)
	r.pol.text, _ = Compose(startPolicy, map[string][]Range{"pdl": {{9000, 9010}}}, r.mod.s)
	res := r.createNS("pdl", p9000)
	if res.code != 200 || res.json(t)["state"] != StateActive {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if r.pol.nextID != 0 {
		t.Fatal("a no-op policy change was submitted")
	}
}

func TestUncertainPolicyChangeIsStoredPending(t *testing.T) {
	r := newRig(t, nil)
	r.pol.failWith = fmt.Errorf("%w: %w", policy.ErrUncertain, errors.New("timeout"))
	res := r.createNS("pdl", p9000)
	if res.code != 202 || res.json(t)["uncertain"] != true {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if nss, _ := r.st.Namespaces(); len(nss) != 1 || nss[0].State != StatePending {
		t.Fatalf("%+v", nss)
	}
}

func TestPatchPortsFlow(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	before := r.pol.text
	res := r.admin("PATCH", "/funnel/namespaces/pdl", map[string]any{"ports": []rangeJSON{{9500, 9510}}})
	if res.code != 202 {
		t.Fatalf("%d %s", res.code, res.body)
	}
	id := int64(res.json(t)["change_id"].(float64))
	if !strings.Contains(r.pol.text, "tag:ns-pdl:9500-9510") {
		t.Fatalf("ACL not updated:\n%s", r.pol.text)
	}
	// While pending the old ports stay served and a second change is refused.
	doc, _ := BuildConfig(r.st, r.mod.s, r.mod.outcome)
	if p := doc.Namespaces["pdl"].Ports; p[0].Lo != 9000 {
		t.Fatalf("served ports during the change: %+v", p)
	}
	if res := r.admin("PATCH", "/funnel/namespaces/pdl", map[string]any{"ports": p9000}); res.code != 409 {
		t.Errorf("second change: %d", res.code)
	}
	r.pol.end(id, policy.OutcomeConfirmed, before)
	doc, _ = BuildConfig(r.st, r.mod.s, r.mod.outcome)
	if p := doc.Namespaces["pdl"].Ports; p[0].Lo != 9500 {
		t.Fatalf("after confirmation: %+v", p)
	}
	for name, c := range map[string]struct {
		path string
		body any
		code int
	}{
		"unknown": {"/funnel/namespaces/nope", map[string]any{"ports": p9000}, 404},
		"empty":   {"/funnel/namespaces/pdl", map[string]any{"ports": []rangeJSON{}}, 422},
		"reserve": {"/funnel/namespaces/pdl", map[string]any{"ports": []rangeJSON{{9100, 9100}}}, 422},
	} {
		if res := r.admin("PATCH", c.path, c.body); res.code != c.code {
			t.Errorf("%s: %d", name, res.code)
		}
	}
}

func TestPatchOfPendingNamespaceIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.createNS("pdl", p9000)
	if res := r.admin("PATCH", "/funnel/namespaces/pdl", map[string]any{"ports": p9000}); res.code != 409 {
		t.Fatalf("%d", res.code)
	}
}

func TestDeleteNamespaceRefusedWithManualRoutesOrReportedDynamicRoutes(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	if res := r.admin("POST", "/funnel/routes", map[string]any{"ns": "pdl", "service": "docs", "target": "pdl-box", "port": 9001}); res.code != 204 {
		t.Fatalf("route: %d %s", res.code, res.body)
	}
	if res := r.admin("DELETE", "/funnel/namespaces/pdl", nil); res.code != 409 {
		t.Fatalf("with a manual route: %d", res.code)
	}
	r.admin("DELETE", "/funnel/routes/pdl/docs", nil)
	report := `{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"errors":[],
		"routes":[{"ns":"pdl","service":"admin","owner_node_id":"n1"},{"ns":"pdl","service":"static","owner_node_id":""}]}`
	r.st.SaveState("ingress", []byte(report), time.Now())
	res := r.admin("DELETE", "/funnel/namespaces/pdl", nil)
	if res.code != 409 || !strings.Contains(res.body, "1 registered route") {
		t.Fatalf("with a reported dynamic route: %d %s", res.code, res.body)
	}
	if nss, _ := r.st.Namespaces(); len(nss) != 1 {
		t.Fatal("namespace deleted despite the refusal")
	}
}

func TestDeleteNamespaceRemovesACLEntriesViaChange(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	r.activate("docs", []rangeJSON{{3000, 3000}})
	// An unexpired publisher key for the namespace is reported back.
	r.hs.keys = append(r.hs.keys, headscale.PreAuthKey{ID: 5, ACLTags: []string{"tag:funnel", "tag:ns-pdl"}, Expiration: time.Now().Add(time.Hour)})
	res := r.admin("DELETE", "/funnel/namespaces/pdl", nil)
	if res.code != 202 || res.json(t)["unexpired_keys"] != float64(1) {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if strings.Contains(r.pol.text, "tag:ns-pdl") || !strings.Contains(r.pol.text, "tag:ns-docs") {
		t.Fatalf("ACL after delete:\n%s", r.pol.text)
	}
	nss, _ := r.st.Namespaces()
	if len(nss) != 1 || nss[0].Name != "docs" {
		t.Fatalf("namespaces = %+v", nss)
	}
	if res := r.admin("DELETE", "/funnel/namespaces/ghost", nil); res.code != 404 {
		t.Errorf("unknown: %d", res.code)
	}
}

func TestMintKeyForcesTagsAndIgnoresExtraFields(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	res := r.admin("POST", "/funnel/namespaces/pdl/keys", `{"tags":["tag:node-control"],"user":"other","aclTags":["tag:x"],"ephemeral":true,"reusable":true,"ttl_hours":48}`)
	if res.code != 200 {
		t.Fatalf("%d %s", res.code, res.body)
	}
	if len(r.hs.created) != 1 {
		t.Fatalf("created = %v", r.hs.created)
	}
	c := r.hs.created[0]
	if c.user != 7 || strings.Join(c.tags, ",") != "tag:funnel,tag:ns-pdl" || !c.ephemeral || !c.reusable || c.ttl != 48*time.Hour {
		t.Fatalf("call = %+v", c)
	}
	m := res.json(t)
	if m["key"] != "tskey-auth-SECRET1" || m["id"] != float64(101) {
		t.Fatalf("response = %v", m)
	}
}

func TestMintKeyDefaultsAndBounds(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	r.admin("POST", "/funnel/namespaces/pdl/keys", `{"reusable":true}`)
	r.admin("POST", "/funnel/namespaces/pdl/keys", `{"ephemeral":true}`)
	if r.hs.created[0].ttl != 90*24*time.Hour || r.hs.created[1].ttl != 24*time.Hour {
		t.Fatalf("default ttl: %v %v", r.hs.created[0].ttl, r.hs.created[1].ttl)
	}
	for _, body := range []string{`{"ttl_hours":0}`, `{"ttl_hours":-5}`, `{"ttl_hours":8761}`} {
		if res := r.admin("POST", "/funnel/namespaces/pdl/keys", body); res.code != 422 {
			t.Errorf("%s: %d", body, res.code)
		}
	}
	if len(r.hs.created) != 2 {
		t.Fatal("an out-of-range request minted a key")
	}
}

func TestMintKeyOnlyForActiveNamespaceAndExistingUser(t *testing.T) {
	r := newRig(t, nil)
	if res := r.admin("POST", "/funnel/namespaces/ghost/keys", `{}`); res.code != 404 {
		t.Errorf("unknown: %d", res.code)
	}
	r.createNS("pdl", p9000)
	if res := r.admin("POST", "/funnel/namespaces/pdl/keys", `{}`); res.code != 409 {
		t.Errorf("pending: %d", res.code)
	}
	r.pol.end(1, policy.OutcomeConfirmed, "")
	r.hs.users = []headscale.User{{ID: 8, Name: "other"}}
	if res := r.admin("POST", "/funnel/namespaces/pdl/keys", `{}`); res.code != 422 {
		t.Errorf("missing key user: %d", res.code)
	}
	if len(r.hs.created) != 0 {
		t.Fatal("a key was minted")
	}
}

func TestMintKeySecretReturnedOnceAndAbsentFromAuditLogsAndList(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	res := r.admin("POST", "/funnel/namespaces/pdl/keys", `{"reusable":true}`)
	secret := res.json(t)["key"].(string)
	if !strings.Contains(secret, "SECRET") {
		t.Fatal("no secret returned")
	}
	for _, path := range []string{"/funnel/namespaces/pdl/keys", "/funnel/audit", "/funnel/state"} {
		if got := r.admin("GET", path, nil); strings.Contains(got.body, secret) || strings.Contains(got.body, "tskey-") {
			t.Errorf("%s shows the secret: %s", path, got.body)
		}
	}
	if strings.Contains(r.logBuf.String(), secret) {
		t.Error("the secret reached the log")
	}
	es, _ := r.st.AuditLog(50)
	for _, e := range es {
		if strings.Contains(e.Detail, secret) || strings.Contains(e.Action, secret) {
			t.Errorf("audit holds the secret: %+v", e)
		}
	}
}

func TestListKeysFiltersByNamespaceTag(t *testing.T) {
	r := newRig(t, nil)
	r.hs.keys = []headscale.PreAuthKey{
		{ID: 1, ACLTags: []string{"tag:funnel", "tag:ns-pdl"}, Expiration: time.Now().Add(time.Hour), Reusable: true},
		{ID: 2, ACLTags: []string{"tag:funnel", "tag:ns-other"}, Expiration: time.Now().Add(time.Hour)},
		{ID: 3, ACLTags: []string{"tag:node-control"}, Expiration: time.Now().Add(time.Hour)},
		{ID: 4, ACLTags: []string{"tag:ns-pdl"}, Expiration: time.Now().Add(time.Hour)}, // no funnel tag
		{ID: 5, ACLTags: []string{"tag:funnel", "tag:ns-pdl"}, Expiration: time.Now().Add(-time.Hour)},
	}
	m := r.admin("GET", "/funnel/namespaces/pdl/keys", nil).json(t)
	keys := m["keys"].([]any)
	if len(keys) != 2 || keys[0].(map[string]any)["id"] != float64(1) || keys[1].(map[string]any)["expired"] != true {
		t.Fatalf("keys = %v", keys)
	}
}

func TestExpireKeyOnlyPublisherKeys(t *testing.T) {
	r := newRig(t, nil)
	r.hs.keys = []headscale.PreAuthKey{
		{ID: 1, ACLTags: []string{"tag:funnel", "tag:ns-pdl"}},
		{ID: 2, ACLTags: []string{"tag:node-control"}},
	}
	if res := r.admin("DELETE", "/funnel/keys/1", nil); res.code != 204 {
		t.Fatalf("publisher key: %d", res.code)
	}
	for _, id := range []string{"2", "99", "abc"} {
		if res := r.admin("DELETE", "/funnel/keys/"+id, nil); res.code != 404 && res.code != 422 {
			t.Errorf("key %s: %d", id, res.code)
		}
	}
	if len(r.hs.expired) != 1 || r.hs.expired[0] != 1 {
		t.Fatalf("expired = %v", r.hs.expired)
	}
}

func TestManualRouteRequiresActiveNamespaceKnownNodeWithNSTagAndAllowedPort(t *testing.T) {
	r := newRig(t, nil)
	route := func(m map[string]any) resp { return r.admin("POST", "/funnel/routes", m) }
	ok := map[string]any{"ns": "pdl", "service": "docs", "target": "pdl-box", "port": 9001}
	if res := route(ok); res.code != 404 {
		t.Errorf("unknown namespace: %d", res.code)
	}
	r.createNS("pdl", p9000)
	if res := route(ok); res.code != 409 {
		t.Errorf("pending namespace: %d", res.code)
	}
	r.pol.end(1, policy.OutcomeConfirmed, "")
	r.admin("GET", "/funnel/state", nil)
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for kk, vv := range ok {
			m[kk] = vv
		}
		m[k] = v
		return m
	}
	for name, body := range map[string]map[string]any{
		"port outside":    with("port", 8081),
		"unknown node":    with("target", "ghost"),
		"node without ns": with("target", "plain"),
		"ip of that node": with("target", "100.64.1.2"),
		"bad service":     with("service", "Bad Name"),
		"bad scheme":      with("scheme", "ftp"),
	} {
		if res := route(body); res.code != 422 {
			t.Errorf("%s: %d %s", name, res.code, res.body)
		}
	}
	if res := route(with("target", "100.64.1.1")); res.code != 204 {
		t.Errorf("by tailnet IP: %d %s", res.code, res.body)
	}
	if res := route(ok); res.code != 204 {
		t.Errorf("by name: %d %s", res.code, res.body)
	}
	rs, _ := r.st.ManualRoutes()
	if len(rs) != 1 || rs[0].Target != "pdl-box" {
		t.Fatalf("routes = %+v", rs)
	}
	doc, _ := BuildConfig(r.st, r.mod.s, r.mod.outcome)
	if len(doc.Routes) != 1 {
		t.Fatalf("config routes = %+v", doc.Routes)
	}
	if res := r.admin("DELETE", "/funnel/routes/pdl/docs", nil); res.code != 204 {
		t.Errorf("delete: %d", res.code)
	}
	if res := r.admin("DELETE", "/funnel/routes/pdl/docs", nil); res.code != 404 {
		t.Errorf("delete again: %d", res.code)
	}
}

func TestBlockUnblockAndAudit(t *testing.T) {
	r := newRig(t, nil)
	if res := r.admin("POST", "/funnel/blocks", map[string]any{"ns": "pdl", "service": "admin", "reason": "noisy"}); res.code != 204 {
		t.Fatalf("%d %s", res.code, res.body)
	}
	doc, _ := BuildConfig(r.st, r.mod.s, r.mod.outcome)
	if len(doc.Blocked) != 1 {
		t.Fatalf("blocked = %+v", doc.Blocked)
	}
	if res := r.admin("POST", "/funnel/blocks", map[string]any{"ns": "PDL", "service": "x"}); res.code != 422 {
		t.Errorf("bad name: %d", res.code)
	}
	if res := r.admin("DELETE", "/funnel/blocks/pdl/admin", nil); res.code != 204 {
		t.Fatalf("unblock: %d", res.code)
	}
	if res := r.admin("DELETE", "/funnel/blocks/pdl/admin", nil); res.code != 404 {
		t.Errorf("unblock again: %d", res.code)
	}
	a := r.admin("GET", "/funnel/audit", nil).json(t)["audit"].([]any)
	var actions []string
	for _, e := range a {
		m := e.(map[string]any)
		if m["actor"] != "alice@" && m["actor"] != "alice" {
			t.Errorf("actor = %v", m["actor"])
		}
		actions = append(actions, m["action"].(string))
	}
	if strings.Join(actions, ",") != "block.delete,block.put" {
		t.Fatalf("audit actions = %v", actions)
	}
}

func TestStateViewMarksStaleReport(t *testing.T) {
	r := newRig(t, nil)
	if got := r.admin("GET", "/funnel/state", nil).json(t); got["ingress"] != nil {
		t.Fatalf("ingress before any report: %v", got["ingress"])
	}
	report := `{"applied_version":"v","sent_at":"2000-01-01T00:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":["x"]}`
	now := time.Unix(1_700_000_000, 0)
	r.mod.now = func() time.Time { return now.Add(30 * time.Second) }
	r.st.SaveState("ingress", []byte(report), now)
	ing := r.admin("GET", "/funnel/state", nil).json(t)["ingress"].(map[string]any)
	if ing["stale"] != false || ing["age_seconds"] != float64(30) {
		t.Fatalf("fresh: %v", ing)
	}
	r.mod.now = func() time.Time { return now.Add(5 * time.Minute) }
	ing = r.admin("GET", "/funnel/state", nil).json(t)["ingress"].(map[string]any)
	if ing["stale"] != true || ing["age_seconds"] != float64(300) {
		t.Fatalf("stale: %v", ing)
	}
	if ing["report"].(map[string]any)["applied_version"] != "v" {
		t.Fatal("report not passed through")
	}
	// A report that is not JSON must not break the page.
	r.st.SaveState("ingress", []byte("garbage"), now)
	if res := r.admin("GET", "/funnel/state", nil); res.code != 200 || res.json(t)["ingress"] != nil {
		t.Fatalf("garbage report: %d %s", res.code, res.body)
	}
}

func TestChecklistContainsWildcardCNAMEAndAcmeHint(t *testing.T) {
	r := newRig(t, nil)
	r.createNS("pdl", p9000)
	nss := r.admin("GET", "/funnel/state", nil).json(t)["namespaces"].([]any)
	cl := nss[0].(map[string]any)["checklist"].(map[string]any)
	b, _ := json.Marshal(cl)
	for _, want := range []string{`*.pdl.example.com.`, `my-ingress.fly.dev.`, `_acme-challenge.pdl.example.com.`, `fly certs add \"*.pdl.example.com\" -a my-ingress`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("checklist lacks %s: %s", want, b)
		}
	}
}

func TestEveryRoute403ForNonAdminTaggedAndWhoIsError(t *testing.T) {
	r := newRig(t, nil)
	routes := []struct{ method, path string }{
		{"GET", "/funnel"}, {"GET", "/funnel/static/funnel.js"}, {"GET", "/funnel/static/funnel.css"}, {"GET", "/funnel/state"}, {"GET", "/funnel/audit"},
		{"POST", "/funnel/namespaces"}, {"PATCH", "/funnel/namespaces/pdl"}, {"DELETE", "/funnel/namespaces/pdl"},
		{"POST", "/funnel/namespaces/pdl/keys"}, {"GET", "/funnel/namespaces/pdl/keys"}, {"DELETE", "/funnel/keys/1"},
		{"POST", "/funnel/routes"}, {"DELETE", "/funnel/routes/pdl/x"}, {"POST", "/funnel/blocks"}, {"DELETE", "/funnel/blocks/pdl/x"},
	}
	for name, ip := range map[string]string{"other user": bobIP, "ingress node": taggedIP, "no login": nobodyIP, "whois error": whoisErrIP, "unknown": "100.64.9.9"} {
		for _, rt := range routes {
			if res := r.do(rt.method, rt.path, ip, `{}`); res.code != 403 || res.body != "" {
				t.Errorf("%s %s %s: %d %q", name, rt.method, rt.path, res.code, res.body)
			}
		}
	}
	if r.pol.nextID != 0 || len(r.hs.created) != 0 {
		t.Fatal("a refused caller changed something")
	}
}

func TestPostRequiresJSON(t *testing.T) {
	r := newRig(t, nil)
	for _, path := range []string{"/funnel/namespaces", "/funnel/routes", "/funnel/blocks", "/funnel/namespaces/pdl/keys"} {
		req := httptest.NewRequest("POST", path, strings.NewReader("name=pdl"))
		req.RemoteAddr = adminIP + ":1"
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		r.h.ServeHTTP(w, req)
		if w.Code != 422 {
			t.Errorf("%s: %d", path, w.Code)
		}
	}
	if nss, _ := r.st.Namespaces(); len(nss) != 0 {
		t.Fatal("a form post created a namespace")
	}
}

func TestIngressEndpointsStayReachableOnlyForTheIngress(t *testing.T) {
	r := newRig(t, nil)
	if res := r.do("GET", "/ingress/v1/config", taggedIP, nil); res.code != 200 {
		t.Fatalf("ingress: %d", res.code)
	}
	if res := r.do("GET", "/ingress/v1/config", adminIP, nil); res.code != 403 {
		t.Fatalf("a human on a machine endpoint: %d", res.code)
	}
	if res := r.do("GET", "/funnel/state", taggedIP, nil); res.code != 403 {
		t.Fatalf("the ingress on a human endpoint: %d", res.code)
	}
}

func TestHeadscaleFailureIs502(t *testing.T) {
	r := newRig(t, nil)
	r.activate("pdl", p9000)
	r.hs.listErr = errors.New("connection refused to 10.0.0.1")
	res := r.admin("POST", "/funnel/namespaces/pdl/keys", `{}`)
	if res.code != 502 || strings.Contains(res.body, "10.0.0.1") {
		t.Fatalf("%d %s", res.code, res.body)
	}
}

func TestRegisterRefusesIncompleteSetups(t *testing.T) {
	good := func() (*fakePolicy, *fakeHS) { return newFakePolicy(startPolicy), &fakeHS{} }
	cases := map[string]struct {
		extra map[string]string
		pol   bool
		tn    bool
	}{
		"no policy console":   {nil, false, true},
		"no tailnet":          {nil, true, false},
		"wildcard host":       {map[string]string{"ENROLL_ALLOWED_HOSTS": "*"}, true, true},
		"no hosts":            {map[string]string{"ENROLL_ALLOWED_HOSTS": ""}, true, true},
		"no headscale server": {map[string]string{"HEADSCALE_LOGIN_SERVER": ""}, true, true},
		"no headscale key":    {map[string]string{"HEADSCALE_API_KEY": ""}, true, true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			pol, hs := good()
			env := funnelEnv(t.TempDir(), c.extra)
			m := New(env, hs, func() PolicyService {
				if c.pol {
					return pol
				}
				return nil
			}).(*Module)
			routes := &core.Routes{Admin: http.NewServeMux(), Env: env, Log: slog.Default()}
			if c.tn {
				routes.Tailnet = authz.NewFakeTailnet(nil)
			}
			if err := m.Register(routes); err == nil {
				t.Fatal("registered")
			}
			if m.Nav() != nil {
				t.Fatal("an inactive module advertises a page")
			}
		})
	}
}

func TestNavOnlyWhenActive(t *testing.T) {
	r := newRig(t, nil)
	if nav := r.mod.Nav(); len(nav) != 1 || nav[0].Path != "/funnel" {
		t.Fatalf("nav = %v", nav)
	}
}
