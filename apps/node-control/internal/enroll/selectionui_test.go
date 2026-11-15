package enroll

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

func intp(n int) *int { return &n }

// With no override, every profile is byte for byte the one of enroll.json (the existing goldens).
func TestSelectionEmptyOverrideKeepsGoldens(t *testing.T) {
	for _, cfg := range []IngressConfig{tunedCfg(), groupedCfg(), multiCfg} {
		if !bytes.Equal(mustRenderBoth(t, cfg), mustRenderBoth(t, ApplySelection(cfg, SelectionOverride{}))) {
			t.Fatalf("an empty override changed the profile")
		}
	}
	for _, f := range []func(IngressConfig, Device) ([]byte, error){RenderProfile, RenderClashProfile, RenderURIList} {
		a := mustRender(t, f, multiCfg)
		b := mustRender(t, f, (*Selector)(nil).Apply(multiCfg))
		if !bytes.Equal(a, b) {
			t.Fatal("a nil selector changed the profile")
		}
	}
	// and the goldens themselves still match through an empty stored override
	s := &Selector{store: newStore(t, adminNow)}
	got, err := RenderProfile(s.Apply(groupedCfg()), multiDevice())
	if err != nil || !bytes.Equal(got, golden(t, "profile_grouped_singbox.golden")) {
		t.Fatalf("grouped golden differs through an empty store: %v", err)
	}
	got, err = RenderClashProfile(s.Apply(tunedCfg()), multiDevice())
	if err != nil || !bytes.Equal(got, golden(t, "profile_tuned_clash.golden")) {
		t.Fatalf("tuned clash golden differs through an empty store: %v", err)
	}
}

func mustRenderBoth(t *testing.T, cfg IngressConfig) []byte {
	t.Helper()
	a := mustRender(t, RenderProfile, cfg)
	return append(a, mustRender(t, RenderClashProfile, cfg)...)
}

// Precedence: console, then enroll.json, then the built-in default.
func TestSelectionPrecedence(t *testing.T) {
	file := groupedCfg() // tolerance 120, interval 90s, url from file, no strategy
	e := Effective(file, SelectionOverride{})
	if e.Tolerance.Value != 120 || e.Tolerance.Source != SourceFile || e.Strategy.Source != SourceDefault ||
		e.Interval.Value != "90s" || e.URL.Source != SourceFile || e.OrderSource != SourceFile {
		t.Fatalf("file values: %+v", e)
	}
	e = Effective(multiCfg, SelectionOverride{})
	if e.Tolerance.Value != probeTolerance || e.Tolerance.Source != SourceDefault || e.Interval.Value != probeInterval ||
		e.URL.Value != probeURL || e.Strategy.Value != strategyURLTest {
		t.Fatalf("defaults: %+v", e)
	}
	o := SelectionOverride{Tolerance: intp(0), Interval: "5m", Strategy: strategyRoundRobin,
		Groups: map[string]string{"ingress-4": "group-b"}, Order: []string{"ingress-4"}}
	e = Effective(file, o)
	if e.Tolerance.Value != 0 || e.Tolerance.Source != SourceUI || e.Interval.Value != "5m" ||
		e.URL.Source != SourceFile || e.Strategy.Value != strategyRoundRobin || e.OrderSource != SourceUI {
		t.Fatalf("override: %+v", e)
	}
	if e.Bridges[0].Name != "ingress-4" || e.Bridges[0].Group != "group-b" || e.Bridges[0].GroupSource != SourceUI ||
		e.Bridges[0].FileGroup != "" || e.Bridges[1].GroupSource != SourceFile {
		t.Fatalf("bridges: %+v", e.Bridges)
	}
	cfg := ApplySelection(file, o)
	if *cfg.Selection.Tolerance != 0 || cfg.Selection.Interval != 5*time.Minute ||
		cfg.Selection.URL != "https://probe.example.com/204" || cfg.Ingresses[0].Name != "ingress-4" {
		t.Fatalf("applied: %+v", cfg.Selection)
	}
	if *file.Selection.Tolerance != 120 || file.Ingresses[0].Name != "ingress-1" || file.Ingresses[3].Group != "" {
		t.Fatal("ApplySelection changed its input")
	}
	// "url-test" removes a strategy of the file
	if c := ApplySelection(tunedCfg(), SelectionOverride{Strategy: strategyURLTest}); c.Selection.Strategy != "" {
		t.Fatal("url-test did not remove the file strategy")
	}
	// an empty group removes the group of the file
	c := ApplySelection(file, SelectionOverride{Groups: map[string]string{"ingress-1": ""}})
	if c.Ingresses[0].Group != "" {
		t.Fatal("empty group override")
	}
	// the profile follows the override
	prof := string(mustRender(t, RenderProfile, ApplySelection(file, SelectionOverride{Tolerance: intp(333)})))
	if !strings.Contains(prof, `"tolerance":333`) {
		t.Fatal("tolerance override not in profile")
	}
}

func TestSelectionValidate(t *testing.T) {
	file := groupedCfg()
	ok := []SelectionOverride{{}, {Tolerance: intp(0)}, {Tolerance: intp(1000)}, {Interval: "30s"}, {Interval: "24h"},
		{URL: "https://probe.example.com/x"}, {Strategy: strategyURLTest}, {Strategy: strategyConsistentHash},
		{Groups: map[string]string{"ingress-1": "", "ingress-2": "group-c"}},
		{Order: []string{"ingress-3", "ingress-1"}}, {Drain: []string{"ingress-1", "ingress-2", "ingress-3"}}}
	for _, o := range ok {
		if err := o.Validate(file); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	bad := map[string]SelectionOverride{
		"tolerance":   {Tolerance: intp(1001)},
		"negative":    {Tolerance: intp(-1)},
		"interval":    {Interval: "10s"},
		"interval2":   {Interval: "1500ms"},
		"url":         {URL: "ftp://x.example.com"},
		"url-quote":   {URL: "https://x.example.com/\"a"},
		"strategy":    {Strategy: "random"},
		"groupname":   {Groups: map[string]string{"ingress-1": "Bad Name"}},
		"groupbridge": {Groups: map[string]string{"nope": "a"}},
		"order":       {Order: []string{"nope"}},
		"orderdup":    {Order: []string{"ingress-1", "ingress-1"}},
		"drainall":    {Drain: []string{"ingress-1", "ingress-2", "ingress-3", "ingress-4"}},
		"drainname":   {Drain: []string{"<x>"}},
	}
	for name, o := range bad {
		err := o.Validate(file)
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), "<x>") || strings.Contains(err.Error(), "ENROLL_CONFIG") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A drained bridge is not in new profiles; the last bridge is never drained.
func TestSelectionDrain(t *testing.T) {
	file := groupedCfg()
	c := ApplySelection(file, SelectionOverride{Drain: []string{"ingress-2"}})
	if len(c.Ingresses) != 3 {
		t.Fatalf("%d bridges", len(c.Ingresses))
	}
	for _, f := range []func(IngressConfig, Device) ([]byte, error){RenderProfile, RenderClashProfile, RenderURIList} {
		p := string(mustRender(t, f, c))
		if d, err := base64.StdEncoding.DecodeString(p); err == nil {
			p = string(d)
		}
		if strings.Contains(p, "ingress-2") || !strings.Contains(p, "ingress-3") {
			t.Errorf("drained bridge in the profile:\n%s", p)
		}
	}
	// group-b is empty now: no empty group in the profile
	if p := string(mustRender(t, RenderProfile, c)); strings.Contains(p, "@group-b") {
		t.Error("an empty group is in the profile")
	}
	// a stale drain list that names every bridge keeps the profile whole
	c = ApplySelection(file, SelectionOverride{Drain: []string{"ingress-1", "ingress-2", "ingress-3", "ingress-4"}})
	if len(c.Ingresses) != 4 {
		t.Fatal("all bridges drained")
	}
	if e := Effective(file, SelectionOverride{Drain: []string{"ingress-2"}}); !e.Bridges[1].Drained || len(e.Bridges) != 4 {
		t.Fatalf("view %+v", e.Bridges)
	}
}

func TestSelectionStoreBothBackends(t *testing.T) {
	for _, f := range backendFactories {
		t.Run(f.name, func(t *testing.T) {
			b := f.open(t).b
			d, err := b.SelectionDoc()
			if err != nil || !d.Current.Empty() || d.Previous != nil {
				t.Fatalf("empty store: %+v %v", d, err)
			}
			want := SelectionDoc{Current: SelectionOverride{Tolerance: intp(0), Drain: []string{"a"}},
				Previous: &SelectionOverride{URL: "https://x.example.com"}, By: "admin@", At: 7}
			if err := b.PutSelectionDoc(want); err != nil {
				t.Fatal(err)
			}
			got, err := b.SelectionDoc()
			gb, _ := json.Marshal(got)
			wb, _ := json.Marshal(want)
			if err != nil || !bytes.Equal(gb, wb) {
				t.Fatalf("got %s want %s (%v)", gb, wb, err)
			}
		})
	}
}

// selectionFx is the admin API with the console on, over a grouped configuration.
func selectionFx(t *testing.T) *adminFx {
	t.Helper()
	store := newStore(t, adminNow)
	a := NewAdmin(store, groupedCfg(), &stubAgent{})
	a.sel = &Selector{store: store}
	mux := core.NewMux()
	a.Register(mux)
	return &adminFx{t: t, store: store, admin: a, mux: mux}
}

func auditActions(t *testing.T, b Backend) []string {
	t.Helper()
	log, err := b.AuditLog(100)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range log {
		out = append(out, e.Action+" "+e.Detail)
	}
	return out
}

// The acceptance path: change a tuning value, preview, apply, revert; the audit log has each step.
func TestSelectionPreviewApplyRevertAudit(t *testing.T) {
	f := selectionFx(t)
	st := decode(t, f.do("GET", "/enroll/selection/state", "", ""))
	if st["can_revert"] != false {
		t.Fatalf("state %v", st)
	}
	body := map[string]any{"override": map[string]any{"tolerance": 300}}
	rec := f.post("/enroll/selection/preview", body)
	if rec.Code != 200 {
		t.Fatalf("preview %d %s", rec.Code, rec.Body)
	}
	p := decode(t, rec)
	if ch := p["changes"].([]any); len(ch) != 1 || ch[0] != "tolerance: 120 (file) -> 300 (ui)" {
		t.Fatalf("changes %v", p["changes"])
	}
	if p["check"] == "" || len(p["singbox"].([]any)) == 0 || len(p["clash"].([]any)) == 0 {
		t.Fatalf("preview %v", p)
	}
	if strings.Contains(rec.Body.String(), "11111111") || strings.Contains(rec.Body.String(), "KEY-ONE") {
		t.Fatal("the preview carries device or key material")
	}
	// a preview stores nothing
	if d, _ := f.store.SelectionDoc(); !d.Current.Empty() {
		t.Fatal("preview stored")
	}
	rec = f.post("/enroll/selection/apply", body)
	if rec.Code != 200 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body)
	}
	st = decode(t, rec)
	eff := st["effective"].(map[string]any)["tolerance"].(map[string]any)
	if eff["value"] != float64(300) || eff["source"] != "ui" || st["can_revert"] != true {
		t.Fatalf("after apply %v", st)
	}
	// the public profile now carries the value
	s := &Selector{store: f.store}
	if p := string(mustRender(t, RenderProfile, s.Apply(groupedCfg()))); !strings.Contains(p, `"tolerance":300`) {
		t.Fatal("applied value not in the profile")
	}
	rec = f.post("/enroll/selection/revert", map[string]any{})
	if rec.Code != 200 {
		t.Fatalf("revert %d %s", rec.Code, rec.Body)
	}
	st = decode(t, rec)
	eff = st["effective"].(map[string]any)["tolerance"].(map[string]any)
	if eff["value"] != float64(120) || eff["source"] != "file" || st["can_revert"] != false {
		t.Fatalf("after revert %v", st)
	}
	if !bytes.Equal(mustRender(t, RenderProfile, s.Apply(groupedCfg())), golden(t, "profile_grouped_singbox.golden")) {
		t.Fatal("after the revert the profile is not the one of enroll.json")
	}
	// one step only
	if rec := f.post("/enroll/selection/revert", map[string]any{}); rec.Code != http.StatusConflict {
		t.Fatalf("second revert %d", rec.Code)
	}
	got := auditActions(t, f.store)
	want := []string{
		"selection.revert tolerance: 300 (ui) -> 120 (file)",
		"selection.apply tolerance: 120 (file) -> 300 (ui)",
		"selection.preview tolerance: 120 (file) -> 300 (ui)",
	}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("audit\n got %q\nwant %q", got, want)
	}
	log, _ := f.store.AuditLog(1)
	if log[0].Actor == "" || log[0].At.IsZero() {
		t.Fatalf("audit who/when: %+v", log[0])
	}
}

func TestSelectionRefusesBadInput(t *testing.T) {
	f := selectionFx(t)
	for _, body := range []string{
		`{"override":{"tolerance":5000}}`, `{"override":{"host":"x.example.com"}}`, `{"override":{"public_key":"k"}}`,
		`{"override":{"port":1}}`, `{"override":{"drain":["ingress-1","ingress-2","ingress-3","ingress-4"]}}`,
		`{"tolerance":1}`, `{"override":{},"x":1}`, `[]`,
	} {
		for _, path := range []string{"/enroll/selection/preview", "/enroll/selection/apply"} {
			if rec := f.do("POST", path, "application/json", body); rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s %s: %d", path, body, rec.Code)
			}
		}
	}
	if rec := f.do("POST", "/enroll/selection/apply", "text/plain", `{"override":{}}`); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("content type: %d", rec.Code)
	}
	if d, _ := f.store.SelectionDoc(); !d.Current.Empty() || d.Previous != nil {
		t.Fatal("a refused change was stored")
	}
	if got := auditActions(t, f.store); len(got) != 0 {
		t.Fatalf("a refused change was logged: %v", got)
	}
}

func TestSelectionExport(t *testing.T) {
	f := selectionFx(t)
	body := map[string]any{"override": map[string]any{"interval": "5m", "strategy": "round-robin",
		"groups": map[string]any{"ingress-4": "group-b"}, "order": []any{"ingress-2"}, "drain": []any{"ingress-3"}}}
	if rec := f.post("/enroll/selection/apply", body); rec.Code != 200 {
		t.Fatalf("apply %d %s", rec.Code, rec.Body)
	}
	var ex SelectionExport
	if err := json.Unmarshal(f.do("GET", "/enroll/selection/export", "", "").Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.URLTest["interval"] != "5m" || ex.URLTest["strategy"] != "round-robin" || ex.URLTest["tolerance"] != float64(120) ||
		ex.URLTest["url"] != "https://probe.example.com/204" {
		t.Errorf("urltest %v", ex.URLTest)
	}
	if strings.Join(ex.Order, ",") != "ingress-2,ingress-1,ingress-3,ingress-4" || ex.Groups["ingress-4"] != "group-b" ||
		strings.Join(ex.Drained, ",") != "ingress-3" {
		t.Errorf("export %+v", ex)
	}
	// defaults are not exported
	if ex := Export(multiCfg, SelectionOverride{}); len(ex.URLTest) != 0 {
		t.Errorf("defaults exported: %v", ex.URLTest)
	}
}

// Off by default: no page, no API, no nav entry, and a stored override is not used.
func TestSelectionOffByDefault(t *testing.T) {
	f := newAdminFxCfg(t, groupedCfg())
	for _, p := range []string{"/enroll/selection", "/enroll/selection/state", "/enroll/static/selection.js"} {
		if rec := f.do("GET", p, "", ""); rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	if SelectionUIEnabled(core.MapEnv(map[string]string{})) || !SelectionUIEnabled(core.MapEnv(map[string]string{"ENROLL_SELECTION_UI": "1"})) {
		t.Error("flag")
	}
}

// The page script runs under node against a small DOM stand-in (skipped when node is missing).
func TestSelectionPageSelfTest(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "testdata/selection_selftest.js", "../ui/static/hl.js", "static/selection.js").CombinedOutput()
	if err != nil {
		t.Fatalf("selection selftest: %v\n%s", err, out)
	}
	if out, err := exec.Command(node, "--check", "static/selection.js").CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s", err, out)
	}
}

func TestSelectionPageAndScript(t *testing.T) {
	f := selectionFx(t)
	for _, p := range []string{"/enroll/selection", "/enroll/static/selection.js"} {
		rec := f.do("GET", p, "", "")
		if rec.Code != 200 || rec.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
	html := string(selectionHTML)
	for _, bad := range []string{"<style", " style=", "javascript:", "http://", "https://", " onclick="} {
		if strings.Contains(html, bad) {
			t.Errorf("page contains %q", bad)
		}
	}
	if strings.Contains(string(selectionJS), "innerHTML") {
		t.Error("script uses innerHTML")
	}
}
