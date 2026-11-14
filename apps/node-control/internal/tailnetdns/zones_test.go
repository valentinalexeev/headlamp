package tailnetdns

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/ui"
)

// ask sends one query of type typ from the client at src and gives the rcode and the answers as
// "<TYPE> <data>".
func (r *rig) ask(name string, typ dnsmessage.Type, src string) (dnsmessage.RCode, []string) {
	r.t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 7})
	_ = b.StartQuestions()
	_ = b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name + "."), Type: typ, Class: dnsmessage.ClassINET})
	q, _ := b.Finish()
	var from netip.Addr
	if src != "" {
		from = netip.MustParseAddr(src)
	}
	var p dnsmessage.Parser
	h, err := p.Start(r.dns.AnswerFrom(q, from))
	if err != nil {
		r.t.Fatal(err)
	}
	_ = p.SkipAllQuestions()
	var out []string
	for {
		rh, err := p.AnswerHeader()
		if err != nil {
			break
		}
		switch rh.Type {
		case dnsmessage.TypeA:
			a, _ := p.AResource()
			out = append(out, "A "+netip.AddrFrom4(a.A).String())
		case dnsmessage.TypeAAAA:
			a, _ := p.AAAAResource()
			out = append(out, "AAAA "+netip.AddrFrom16(a.AAAA).String())
		case dnsmessage.TypeCNAME:
			c, _ := p.CNAMEResource()
			out = append(out, "CNAME "+c.CNAME.String())
		case dnsmessage.TypeTXT:
			c, _ := p.TXTResource()
			out = append(out, "TXT "+strings.Join(c.TXT, "|"))
		case dnsmessage.TypeSRV:
			c, _ := p.SRVResource()
			out = append(out, "SRV "+c.Target.String())
		default:
			_ = p.SkipAnswer()
		}
	}
	return h.RCode, out
}

func (r *rig) mustCreate(body map[string]any) {
	r.t.Helper()
	if c, b := r.do("POST", "/dns/records", ipOwner, body); c != http.StatusCreated {
		r.t.Fatalf("create %v: %d %s", body, c, b)
	}
}

func TestRecordTypes(t *testing.T) {
	for name, be := range backends {
		t.Run(name, func(t *testing.T) {
			r := newRigWith(t, be)
			r.mustCreate(map[string]any{"name": "app.example.com", "node": "nas"})
			r.mustCreate(map[string]any{"name": "alias.example.com", "type": "cname", "target": "App.Example.com."})
			r.mustCreate(map[string]any{"name": "info.example.com", "type": "TXT", "text": []string{"v=1", "owner=ops"}})
			r.mustCreate(map[string]any{"name": "_http._tcp.example.com", "type": "SRV", "target": "app.example.com",
				"port": 8080, "priority": 10, "weight": 5})
			r.mustCreate(map[string]any{"name": "v6.example.com", "type": "AAAA", "addresses": []string{"fd7a:115c:a1e0::5"}})
			cases := []struct {
				name string
				typ  dnsmessage.Type
				want []string
			}{
				{"app.example.com", dnsmessage.TypeA, []string{"A 100.64.0.30"}},
				{"app.example.com", dnsmessage.TypeAAAA, []string{"AAAA fd7a:115c:a1e0::30"}},
				{"alias.example.com", dnsmessage.TypeA, []string{"CNAME app.example.com.", "A 100.64.0.30"}},
				{"alias.example.com", dnsmessage.TypeCNAME, []string{"CNAME app.example.com."}},
				{"info.example.com", dnsmessage.TypeTXT, []string{"TXT v=1|owner=ops"}},
				{"info.example.com", dnsmessage.TypeA, nil},
				{"_http._tcp.example.com", dnsmessage.TypeSRV, []string{"SRV app.example.com."}},
				{"v6.example.com", dnsmessage.TypeAAAA, []string{"AAAA fd7a:115c:a1e0::5"}},
			}
			for _, c := range cases {
				rc, got := r.ask(c.name, c.typ, "")
				if rc != dnsmessage.RCodeSuccess || !slices.Equal(got, c.want) {
					t.Errorf("%s %v: %v %v, want %v", c.name, c.typ, rc, got, c.want)
				}
			}
			// the type survives a reload from the store
			recs, err := r.mod.st.Records()
			if err != nil {
				t.Fatal(err)
			}
			types := map[string]string{}
			for _, x := range recs {
				types[x.Name] = x.Type
			}
			if types["alias.example.com"] != "CNAME" || types["v6.example.com"] != "A" || types["_http._tcp.example.com"] != "SRV" {
				t.Errorf("types from the store: %v", types)
			}
		})
	}
}

func TestTypedRecordValidation(t *testing.T) {
	r := newRig(t)
	for _, body := range []map[string]any{
		{"name": "x.example.com", "type": "MX", "target": "a.example.com"},
		{"name": "x.example.com", "type": "CNAME", "addresses": []string{"100.64.0.5"}},
		{"name": "x.example.com", "type": "CNAME", "target": "x.example.com"},
		{"name": "x.example.com", "type": "CNAME", "target": "nodots"},
		{"name": "x.example.com", "type": "TXT"},
		{"name": "x.example.com", "type": "TXT", "text": []string{strings.Repeat("a", 256)}},
		{"name": "x.example.com", "type": "TXT", "text": []string{"bad\x01"}},
		{"name": "x.example.com", "type": "SRV", "target": "a.example.com"},
		{"name": "x.example.com", "type": "A", "target": "a.example.com"},
		{"name": "_x.example.com", "addresses": []string{"100.64.0.5"}},
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{{"addresses": []string{"100.64.0.6"}}}},
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{{"tags": []string{"dev"}}}},
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{{"users": []string{"a b"}}}},
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{{"tags": []string{"tag:a"}, "type": "TXT", "text": []string{"x"}}}},
		{"name": "x.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{{"tags": []string{"tag:a"}, "addresses": []string{"192.0.2.1"}}}},
		{"name": "x.example.com"}, // no data and no rules
	} {
		if c, b := r.do("POST", "/dns/records", ipOwner, body); c != http.StatusUnprocessableEntity {
			t.Errorf("%v: %d %s", body, c, b)
		}
	}
}

// The first rule that matches the client answers; a client that no rule matches gets the default.
func TestRulesPerUserAndTag(t *testing.T) {
	for name, be := range backends {
		t.Run(name, func(t *testing.T) {
			r := newRigWith(t, be)
			r.mustCreate(map[string]any{"name": "web.example.com", "addresses": []string{"100.64.0.5"}, "rules": []map[string]any{
				{"users": []string{"Alice@example.com"}, "addresses": []string{"100.64.0.6"}},
				{"tags": []string{"tag:dev-a"}, "node": "nas"},
				{"tags": []string{"tag:dev-a", "tag:dev-b"}, "tag": "tag:llm-proxy"},
			}})
			for src, want := range map[string][]string{
				"":       {"A 100.64.0.5"},
				ipViewer: {"A 100.64.0.5"},
				ipAlice:  {"A 100.64.0.6"},
				ipDevA:   {"A 100.64.0.30"}, // the first matching rule wins
				ipDevB:   {"A 100.64.0.41"}, // the online nodes of the tag
			} {
				if rc, got := r.ask("web.example.com", dnsmessage.TypeA, src); rc != dnsmessage.RCodeSuccess || !slices.Equal(got, want) {
					t.Errorf("from %q: %v %v, want %v", src, rc, got, want)
				}
			}
			// a name only some clients see: no default, and a rule with no data
			r.mustCreate(map[string]any{"name": "dev.example.com", "type": "TXT", "rules": []map[string]any{
				{"tags": []string{"tag:dev-b"}},
				{"tags": []string{"tag:dev-a", "tag:dev-b"}, "text": []string{"dev"}},
			}})
			for src, want := range map[string][]string{ipDevA: {"TXT dev"}, ipDevB: nil, ipViewer: nil} {
				if rc, got := r.ask("dev.example.com", dnsmessage.TypeTXT, src); rc != dnsmessage.RCodeSuccess || !slices.Equal(got, want) {
					t.Errorf("TXT from %s: %v %v, want %v", src, rc, got, want)
				}
			}
			// the rules survive the store
			recs, _ := r.mod.st.Records()
			if len(recs) != 2 || len(recs[1].Rules) != 3 || recs[1].Rules[0].Type != "A" || recs[1].Rules[1].Node != "nas" {
				t.Errorf("rules from the store: %+v", recs)
			}
		})
	}
}

// A record with rules, or of another type, stays out of the headscale file: headscale would send it
// to every device.
func TestFileHoldsOnlyAddressRecordsWithoutRules(t *testing.T) {
	r, p := rigWithFile(t)
	r.mustCreate(map[string]any{"name": "plain.example.com", "addresses": []string{"100.64.0.5"}})
	r.mustCreate(map[string]any{"name": "ruled.example.com", "addresses": []string{"100.64.0.5"},
		"rules": []map[string]any{{"tags": []string{"tag:dev-a"}, "addresses": []string{"100.64.0.6"}}}})
	r.mustCreate(map[string]any{"name": "alias.example.com", "type": "CNAME", "target": "plain.example.com"})
	var names []string
	for _, e := range readEntries(t, p) {
		names = append(names, e.Name)
	}
	if !slices.Equal(names, []string{"plain.example.com"}) {
		t.Errorf("file names: %v", names)
	}
	var st stateResp
	_, b := r.do("GET", "/dns/state", ipViewer, nil)
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	in := map[string]bool{}
	for _, v := range st.Records {
		in[v.Name] = v.InFile
	}
	if !in["plain.example.com"] || in["ruled.example.com"] || in["alias.example.com"] {
		t.Errorf("in_file: %v", in)
	}
	// only the names the file does not hold need a split line
	if _, ok := st.Split["plain.example.com"]; ok || len(st.Split["ruled.example.com"]) != 1 || len(st.Split["alias.example.com"]) != 1 {
		t.Errorf("split: %v", st.Split)
	}
}

func TestZones(t *testing.T) {
	for name, be := range backends {
		t.Run(name, func(t *testing.T) {
			r := newRigWith(t, be)
			testZoneAnswers(t, r)
			testZoneStateAndDelete(t, r)
		})
	}
}

// testZoneAnswers adds a zone and a record in it: an unknown name in the zone does not exist.
func testZoneAnswers(t *testing.T, r *rig) {
	if rc, _ := r.ask("none.corp.example", dnsmessage.TypeA, ""); rc != dnsmessage.RCodeRefused {
		t.Errorf("before the zone: %v", rc)
	}
	if c, _ := r.do("POST", "/dns/zones", ipViewer, map[string]any{"name": "corp.example"}); c != http.StatusForbidden {
		t.Errorf("viewer zone create: %d", c)
	}
	if c, b := r.do("POST", "/dns/zones", ipOwner, map[string]any{"name": "Corp.Example."}); c != http.StatusCreated {
		t.Fatalf("zone create: %d %s", c, b)
	}
	if c, _ := r.do("POST", "/dns/zones", ipOwner, map[string]any{"name": "corp.example"}); c != http.StatusConflict {
		t.Errorf("duplicate zone: %d", c)
	}
	for _, bad := range []string{base, "x." + base, zone, "example.com", "nodots", "bad_zone.example"} {
		if c, _ := r.do("POST", "/dns/zones", ipOwner, map[string]any{"name": bad}); c != http.StatusUnprocessableEntity {
			t.Errorf("zone %q: %d", bad, c)
		}
	}
	r.mustCreate(map[string]any{"name": "app.corp.example", "addresses": []string{"100.64.0.5"}})
	if rc, got := r.ask("app.corp.example", dnsmessage.TypeA, ""); rc != dnsmessage.RCodeSuccess || len(got) != 1 {
		t.Errorf("record in zone: %v %v", rc, got)
	}
	if rc, _ := r.ask("none.corp.example", dnsmessage.TypeA, ""); rc != dnsmessage.RCodeNameError {
		t.Errorf("unknown name in zone: %v", rc)
	}
}

// testZoneStateAndDelete checks the state, the bridge and the activity of a zone, then deletes it.
func testZoneStateAndDelete(t *testing.T, r *rig) {
	var st stateResp
	_, b := r.do("GET", "/dns/state", ipViewer, nil)
	_ = json.Unmarshal(b, &st)
	if len(st.Zones) != 1 || st.Zones[0].Name != "corp.example" || len(st.Split["corp.example"]) != 1 {
		t.Errorf("state zones %+v split %v", st.Zones, st.Split)
	}
	if _, ok := st.Split["app.corp.example"]; ok || st.Records[0].Zone != "corp.example" {
		t.Errorf("a record in a zone needs no own split line: %v %+v", st.Split, st.Records)
	}
	r.mod.cur.Lock()
	r.mod.cur.listening = true
	r.mod.cur.Unlock()
	if b, ok := r.mod.BridgeDNS(); !ok || !slices.Contains(b.Domains, "domain:corp.example") {
		t.Errorf("bridge domains: %v", b.Domains)
	}
	if c, _ := r.do("DELETE", "/dns/zones/corp.example", ipOwner, nil); c != http.StatusNoContent {
		t.Errorf("zone delete: %d", c)
	}
	if c, _ := r.do("DELETE", "/dns/zones/corp.example", ipOwner, nil); c != http.StatusNotFound {
		t.Errorf("zone delete again: %d", c)
	}
	if rc, _ := r.ask("none.corp.example", dnsmessage.TypeA, ""); rc != dnsmessage.RCodeRefused {
		t.Errorf("after the zone: %v", rc)
	}
	es, _ := r.mod.Activity(context.Background(), "viewer@", 10)
	var actions []string
	for _, e := range es {
		actions = append(actions, e.Action)
	}
	if !slices.Contains(actions, "zone.create") || !slices.Contains(actions, "zone.delete") {
		t.Errorf("activity: %v", actions)
	}
}

// A database of 0.18 (#286) has records without a type: they read as address records.
func TestOldRowsReadAsAddressRecords(t *testing.T) {
	p := filepath.Join(t.TempDir(), "dns.db")
	db, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE records (name TEXT PRIMARY KEY, target TEXT NOT NULL, note TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL, updated INTEGER NOT NULL);
		INSERT INTO records VALUES ('old.example.com', '{"tag":"tag:llm-proxy"}', '', 1, 1);`)
	_ = db.Close()
	if err != nil {
		t.Fatal(err)
	}
	st, err := OpenSQLite(p)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	recs, err := st.Records()
	if err != nil || len(recs) != 1 || recs[0].Type != TypeAddr || recs[0].Tag != "tag:llm-proxy" || !recs[0].InFile() {
		t.Fatalf("%+v %v", recs, err)
	}
	if zs, err := st.Zones(); err != nil || len(zs) != 0 {
		t.Errorf("zones on an old database: %v %v", zs, err)
	}
}

// The DNS page (#285): served with the console's security headers, to viewers only, and in the bar.
func TestPageServedAndInNav(t *testing.T) {
	r := newRig(t)
	for path, ctype := range map[string]string{"/dns": "text/html", "/dns/static/dns.js": "text/javascript"} {
		c, _ := r.do("GET", path, ipViewer, nil)
		if c != http.StatusOK {
			t.Fatalf("%s: %d", path, c)
		}
		rec := httpGet(r, path, ipViewer)
		h := rec.Header()
		if !strings.HasPrefix(h.Get("Content-Type"), ctype) || h.Get("X-Frame-Options") != "DENY" ||
			h.Get("Content-Security-Policy") != ui.PageCSP || h.Get("Cache-Control") != "no-store" {
			t.Errorf("%s headers: %v", path, h)
		}
		if c, _ := r.do("GET", path, "100.64.0.250", nil); c != http.StatusForbidden {
			t.Errorf("%s from a stranger: %d", path, c)
		}
	}
	nav := r.mod.Nav()
	if len(nav) != 1 || nav[0].Path != "/dns" || !nav[0].Allow("viewer@") || nav[0].Allow("nobody@") {
		t.Errorf("nav: %+v", nav)
	}
	if err := core.ValidateNavItem(nav[0]); err != nil {
		t.Error(err)
	}
	r.mustCreate(map[string]any{"name": "app.example.com", "addresses": []string{"100.64.0.5"}})
	items, err := r.mod.Search(context.Background(), "viewer@")
	if err != nil || len(items) != 1 || items[0].Label != "app.example.com" {
		t.Errorf("search: %v %v", items, err)
	}
	if _, err := r.mod.Search(context.Background(), "nobody@"); !errors.Is(err, core.ErrNotVisible) {
		t.Errorf("search for a stranger: %v", err)
	}
}

func TestPageHasNoInlineCodeOrExternalReferences(t *testing.T) {
	html := strings.ToLower(string(pageHTML))
	for _, m := range regexp.MustCompile(`<script\b[^>]*>`).FindAllString(html, -1) {
		if !strings.Contains(m, ` src="/`) || !strings.Contains(m, "defer") {
			t.Errorf("script tag %q must be a same-origin deferred file", m)
		}
	}
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick=", "<link"} {
		if strings.Contains(html, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "eval(", "new Function",
		"confirm(", "prompt(", "alert(", "XMLHttpRequest", "localStorage", "sessionStorage", "http://", "https://"} {
		if strings.Contains(string(pageJS), bad) {
			t.Errorf("script contains %q", bad)
		}
	}
}

func TestPageSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	if out, err := exec.Command(node, "testdata/dns_selftest.js", "../ui/static/hl.js", "static/dns.js").CombinedOutput(); err != nil {
		t.Fatalf("self-test failed: %v\n%s", err, out)
	}
}

func httpGet(r *rig, path, ip string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = ip + ":40000"
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	return rec
}
