package funnel

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"example.com/node-control/internal/authz"
)

func testSettings() *Settings {
	return &Settings{DBPath: "x", PublicDomain: "example.com", FunnelTag: "tag:funnel", NSPrefix: "tag:ns-", IngressTag: "tag:funnel-ingress",
		TagOwners: []string{"group:admins"}, KeyUser: "_svcs"}
}

func openTestStore(t *testing.T) Store {
	t.Helper()
	st, err := OpenSQLite(filepath.Join(t.TempDir(), "funnel.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

type outcomes map[int64]string

func (o outcomes) fn() OutcomeFunc {
	return func(id int64) string {
		if v, ok := o[id]; ok {
			return v
		}
		return "unknown"
	}
}

func mustNS(t *testing.T, st Store, name string, ports []Range, change int64) {
	t.Helper()
	if err := st.CreateNamespace(name, ports, "", change); err != nil {
		t.Fatal(err)
	}
}

func decodeCfg(t *testing.T, b []byte) ConfigDoc {
	t.Helper()
	var d ConfigDoc
	if err := json.Unmarshal(b, &d); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	return d
}

func TestConfigOnlyActiveNamespaces(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	mustNS(t, st, "wait", []Range{{9000, 9000}}, 2)
	mustNS(t, st, "bad", []Range{{7000, 7000}}, 3)
	doc, err := BuildConfig(st, testSettings(), outcomes{1: "confirmed", 2: "pending", 3: "reverted"}.fn())
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Namespaces) != 1 || doc.Namespaces["pdl"].Ports[0].Lo != 8080 {
		t.Fatalf("namespaces = %+v", doc.Namespaces)
	}
	if doc.PublicDomain != "example.com" || doc.Version == "" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestPendingBecomesActiveWhenOutcomeConfirmed(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	if _, err := BuildConfig(st, testSettings(), outcomes{1: "confirmed"}.fn()); err != nil {
		t.Fatal(err)
	}
	nss, _ := st.Namespaces()
	if nss[0].State != StateActive {
		t.Fatalf("state = %s", nss[0].State)
	}
	// The confirmed state sticks, even if the outcome function later says something else.
	doc, _ := BuildConfig(st, testSettings(), outcomes{}.fn())
	if _, ok := doc.Namespaces["pdl"]; !ok {
		t.Fatal("an active namespace disappeared")
	}
}

func TestPendingBecomesFailedWhenOutcomeReverted(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	doc, err := BuildConfig(st, testSettings(), outcomes{1: "reverted"}.fn())
	if err != nil || len(doc.Namespaces) != 0 {
		t.Fatalf("%+v %v", doc, err)
	}
	if nss, _ := st.Namespaces(); nss[0].State != StateFailed {
		t.Fatalf("state = %s", nss[0].State)
	}
}

func TestUnknownOutcomeLeavesPending(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	for _, o := range []string{"unknown", "pending", ""} {
		doc, _ := BuildConfig(st, testSettings(), outcomes{1: o}.fn())
		if len(doc.Namespaces) != 0 {
			t.Fatalf("outcome %q served the namespace", o)
		}
	}
	if nss, _ := st.Namespaces(); nss[0].State != StatePending {
		t.Fatalf("state = %s", nss[0].State)
	}
}

func TestPortChangeServedOnlyAfterConfirmation(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	BuildConfig(st, testSettings(), outcomes{1: "confirmed"}.fn())
	if err := st.SetPendingPorts("pdl", []Range{{9000, 9010}}, 2); err != nil {
		t.Fatal(err)
	}
	doc, _ := BuildConfig(st, testSettings(), outcomes{2: "pending"}.fn())
	if p := doc.Namespaces["pdl"].Ports; len(p) != 1 || p[0].Lo != 8080 {
		t.Fatalf("while pending the old ports must stay: %+v", p)
	}
	doc, _ = BuildConfig(st, testSettings(), outcomes{2: "confirmed"}.fn())
	if p := doc.Namespaces["pdl"].Ports; len(p) != 1 || p[0].Lo != 9000 || p[0].Hi != 9010 {
		t.Fatalf("after confirmation the new ports apply: %+v", p)
	}
	// A reverted port change keeps the old ports.
	st.SetPendingPorts("pdl", []Range{{1, 2}}, 3)
	doc, _ = BuildConfig(st, testSettings(), outcomes{3: "reverted"}.fn())
	if p := doc.Namespaces["pdl"].Ports; p[0].Lo != 9000 {
		t.Fatalf("reverted change altered the ports: %+v", p)
	}
}

func TestManualRoutesOfInactiveNamespacesExcluded(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	mustNS(t, st, "wait", []Range{{9000, 9000}}, 2)
	if err := st.PutManual(ManualRoute{NS: "pdl", Service: "docs", Target: "docs-host", Port: 8080}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutManual(ManualRoute{NS: "wait", Service: "x", Target: "100.64.0.9", Port: 9000, Scheme: "https", InsecureSkipVerify: true}); err != nil {
		t.Fatal(err)
	}
	doc, _ := BuildConfig(st, testSettings(), outcomes{1: "confirmed", 2: "pending"}.fn())
	if len(doc.Routes) != 1 || doc.Routes[0].Service != "docs" || doc.Routes[0].Scheme != "http" {
		t.Fatalf("routes = %+v", doc.Routes)
	}
}

func TestBlocksIncluded(t *testing.T) {
	st := openTestStore(t)
	if err := st.Block("pdl", "admin", "because", "alice"); err != nil {
		t.Fatal(err)
	}
	doc, _ := BuildConfig(st, testSettings(), outcomes{}.fn())
	if len(doc.Blocked) != 1 || doc.Blocked[0] != (docBlock{"pdl", "admin"}) {
		t.Fatalf("blocked = %+v", doc.Blocked)
	}
	b, _ := json.Marshal(doc)
	if strings.Contains(string(b), "because") || strings.Contains(string(b), "alice") {
		t.Fatal("block reason or actor leaked into the config")
	}
}

func TestVersionStableAndChangesWithContent(t *testing.T) {
	st := openTestStore(t)
	mustNS(t, st, "pdl", []Range{{8080, 8080}}, 1)
	o := outcomes{1: "confirmed"}.fn()
	a, _ := BuildConfig(st, testSettings(), o)
	b, _ := BuildConfig(st, testSettings(), o)
	if a.Version != b.Version || len(a.Version) != 64 {
		t.Fatalf("versions %q %q", a.Version, b.Version)
	}
	st.Block("pdl", "x", "r", "a")
	c, _ := BuildConfig(st, testSettings(), o)
	if c.Version == a.Version {
		t.Fatal("version did not change with the content")
	}
	other := testSettings()
	other.PublicDomain = "example.org"
	d, _ := BuildConfig(st, other, o)
	if d.Version == c.Version {
		t.Fatal("version did not change with the domain")
	}
}

// --- HTTP ---

const (
	ingressIP = "100.64.0.10"
	humanIP   = "100.64.0.1"
	otherIP   = "100.64.0.11"
	errIP     = "100.64.0.12"
)

type ingressRig struct {
	h   http.Handler
	st  Store
	out outcomes
	now time.Time
	log *bytes.Buffer
}

func newIngressRig(t *testing.T) *ingressRig {
	t.Helper()
	st := openTestStore(t)
	tn := authz.NewFakeTailnet(map[string]authz.Who{
		ingressIP: {Login: "tagged-devices", Tags: []string{"tag:funnel-ingress"}},
		humanIP:   {Login: "alice@"},
		otherIP:   {Login: "tagged-devices", Tags: []string{"tag:funnel"}},
		errIP:     {Err: http.ErrAbortHandler},
	})
	logBuf := &bytes.Buffer{}
	log := slog.New(slog.NewTextHandler(logBuf, nil))
	rig := &ingressRig{st: st, out: outcomes{}, now: time.Unix(1_700_000_000, 0), log: logBuf}
	g := &ingress{st: st, s: testSettings(), outcome: func(id int64) string { return rig.out.fn()(id) }, now: func() time.Time { return rig.now }, log: log}
	mux := http.NewServeMux()
	g.register(mux, authz.TagGate(tn, "tag:funnel-ingress", log))
	rig.h = mux
	return rig
}

func (r *ingressRig) do(method, path, from, ctype string, body []byte, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = from + ":4000"
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.h.ServeHTTP(w, req)
	return w
}

func TestETag304(t *testing.T) {
	rig := newIngressRig(t)
	mustNS(t, rig.st, "pdl", []Range{{8080, 8080}}, 1)
	rig.out[1] = "confirmed"
	w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, nil)
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	doc := decodeCfg(t, w.Body.Bytes())
	etag := w.Header().Get("ETag")
	if etag != `"`+doc.Version+`"` {
		t.Fatalf("ETag %q, version %q", etag, doc.Version)
	}
	for _, inm := range []string{etag, doc.Version, `W/` + etag, `"other", ` + etag} {
		if w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, map[string]string{"If-None-Match": inm}); w.Code != 304 || w.Body.Len() != 0 {
			t.Errorf("If-None-Match %q: %d", inm, w.Code)
		}
	}
	if w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, map[string]string{"If-None-Match": `"stale"`}); w.Code != 200 {
		t.Errorf("stale etag: %d", w.Code)
	}
	rig.st.Block("pdl", "x", "r", "a")
	if w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, map[string]string{"If-None-Match": etag}); w.Code != 200 {
		t.Errorf("changed content must not answer 304: %d", w.Code)
	}
}

func TestGateMatrix(t *testing.T) {
	rig := newIngressRig(t)
	good := []byte(`{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":[]}`)
	for name, from := range map[string]string{"human": humanIP, "other tag": otherIP, "whois error": errIP, "unknown address": "100.64.9.9"} {
		for _, c := range []struct {
			method, path string
			body         []byte
		}{{"GET", "/ingress/v1/config", nil}, {"POST", "/ingress/v1/state", good}} {
			w := rig.do(c.method, c.path, from, "application/json", c.body, nil)
			if w.Code != 403 || w.Body.Len() != 0 {
				t.Errorf("%s %s %s: %d %q", name, c.method, c.path, w.Code, w.Body.String())
			}
		}
	}
	if w := rig.do("GET", "/ingress/v1/config", ingressIP, "", nil, nil); w.Code != 200 {
		t.Fatalf("ingress refused: %d", w.Code)
	}
	if _, _, ok, _ := rig.st.LatestState(); ok {
		t.Fatal("a refused caller stored a report")
	}
}

func TestStateAcceptsValidBodyKeepsLatest(t *testing.T) {
	rig := newIngressRig(t)
	post := func(v string) *httptest.ResponseRecorder {
		return rig.do("POST", "/ingress/v1/state", ingressIP, "application/json",
			[]byte(`{"applied_version":"`+v+`","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":2,"routes":[{"ns":"pdl","service":"a"}],"errors":["x"]}`), nil)
	}
	if w := post("v1"); w.Code != 204 || w.Body.Len() != 0 {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
	rig.now = rig.now.Add(time.Minute)
	if w := post("v2"); w.Code != 204 {
		t.Fatal(w.Code)
	}
	body, at, ok, err := rig.st.LatestState()
	if err != nil || !ok || !strings.Contains(string(body), `"v2"`) || !at.Equal(rig.now) {
		t.Fatalf("latest: %s %v %v %v", body, at, ok, err)
	}
}

func TestStateRejectsOversize413AndBadShape422(t *testing.T) {
	rig := newIngressRig(t)
	pad := strings.Repeat("x", maxIngressBody)
	big := []byte(`{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":["` + pad + `"]}`)
	if w := rig.do("POST", "/ingress/v1/state", ingressIP, "application/json", big, nil); w.Code != 413 {
		t.Errorf("oversize: %d", w.Code)
	}
	for name, body := range map[string]string{
		"not json":          `<html>`,
		"empty":             ``,
		"array":             `[]`,
		"missing version":   `{"sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":[]}`,
		"bad time":          `{"applied_version":"v","sent_at":"yesterday","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":[]}`,
		"negative peers":    `{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":-1,"routes":[],"errors":[]}`,
		"routes wrong type": `{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":"none","errors":[]}`,
		"errors wrong type": `{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":[1]}`,
	} {
		if w := rig.do("POST", "/ingress/v1/state", ingressIP, "application/json", []byte(body), nil); w.Code != 422 {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := rig.do("POST", "/ingress/v1/state", ingressIP, "text/plain", []byte(`{}`), nil); w.Code != 422 {
		t.Errorf("content type: %d", w.Code)
	}
	if _, _, ok, _ := rig.st.LatestState(); ok {
		t.Fatal("an invalid report was stored")
	}
}

func TestStateExtraFieldsIgnored(t *testing.T) {
	rig := newIngressRig(t)
	body := `{"applied_version":"v","sent_at":"2000-01-01T00:00:00Z","snapshot_loaded":false,"peer_count":0,"routes":[],"errors":[],"surprise":{"a":[1,2,3]},"public_domain":"evil.example"}`
	if w := rig.do("POST", "/ingress/v1/state", ingressIP, "application/json", []byte(body), nil); w.Code != 204 {
		t.Fatalf("%d", w.Code)
	}
	// An old timestamp is stored with the server's receive time, which is what the page shows as the age.
	_, at, _, _ := rig.st.LatestState()
	if !at.Equal(rig.now) {
		t.Fatalf("received time = %v", at)
	}
}

func TestStateNotEchoedInLogs(t *testing.T) {
	rig := newIngressRig(t)
	body := `{"applied_version":"v","sent_at":"2026-10-25T10:00:00Z","snapshot_loaded":true,"peer_count":1,"routes":[],"errors":["super-secret-marker"]}`
	rig.do("POST", "/ingress/v1/state", ingressIP, "application/json", []byte(body), nil)
	rig.do("POST", "/ingress/v1/state", humanIP, "application/json", []byte(body), nil)
	rig.do("POST", "/ingress/v1/state", ingressIP, "application/json", []byte(strings.Replace(body, `"peer_count":1`, `"peer_count":"x"`, 1)), nil)
	if strings.Contains(rig.log.String(), "super-secret-marker") {
		t.Fatalf("report content reached the log:\n%s", rig.log.String())
	}
}
