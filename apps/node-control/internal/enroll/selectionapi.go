package enroll

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// The bridge selection console (#314). It is off by default: ENROLL_SELECTION_UI=1 turns it on.
// Off, the page and API are not there and a stored override is not used, so every profile is the
// one of enroll.json. Reads need enroll view, changes need enroll admin (authz/routes.go).

// SelectionUIEnabled reports whether the bridge selection console is on.
func SelectionUIEnabled(env core.Env) bool { return env.Getenv("ENROLL_SELECTION_UI") == "1" }

// Audit actions of the console.
const (
	ActionSelectionPreview = "selection.preview"
	ActionSelectionApply   = "selection.apply"
	ActionSelectionRevert  = "selection.revert"
)

// selectionMu serialises the read-change-write of the override document.
var selectionMu sync.Mutex

// Selector gives the override to apply to profiles. A nil Selector applies nothing.
type Selector struct {
	store Backend
	// singBox is the path of the sing-box binary for the check; "" skips the check.
	singBox string
}

// NewSelector returns the override source over store. It looks for sing-box on PATH once.
func NewSelector(store Backend) *Selector {
	bin, _ := exec.LookPath("sing-box")
	return &Selector{store: store, singBox: bin}
}

// Apply returns cfg with the stored override. A store error gives cfg as it is: a profile is never
// refused because of the console.
func (s *Selector) Apply(cfg IngressConfig) IngressConfig {
	if s == nil {
		return cfg
	}
	d, err := s.store.SelectionDoc()
	if err != nil {
		slog.Warn("bridge selection override not read", "error_type", errType(err))
		return cfg
	}
	return ApplySelection(cfg, d.Current)
}

// previewDevice is the fixed fake device of a preview: no real UUID or name is ever in it.
func previewDevice() Device {
	return Device{ID: 0, Name: "preview", Client: "sing-box", UUIDs: map[string]string{
		"auto":   "00000000-0000-4000-8000-000000000001",
		"direct": "00000000-0000-4000-8000-000000000002",
		"exit":   "00000000-0000-4000-8000-000000000003",
	}}
}

// Limits of the sing-box check: the mode of the temporary profile, the time it may take and the
// length of its error in an answer.
const (
	checkFileMode = 0o600
	checkTimeout  = 10 * time.Second
	checkErrMax   = 200
)

// check runs `sing-box check` on the profile of cfg. "skipped" when there is no binary.
func (s *Selector) check(ctx context.Context, cfg IngressConfig) (string, error) {
	if s == nil || s.singBox == "" {
		return "skipped", nil
	}
	body, err := RenderProfile(cfg, previewDevice())
	if err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp("", "selection-check")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "profile.json")
	if err := os.WriteFile(p, body, checkFileMode); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	// The binary is from exec.LookPath at start, the argument a file of our own temp directory.
	cmd := exec.CommandContext(ctx, s.singBox, "check", "-c", p) //nolint:gosec // see above
	out, err := cmd.CombinedOutput()
	if err != nil {
		first, _, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
		return "", fmt.Errorf("sing-box check failed: %s", cleanAudit(first, checkErrMax))
	}
	return "ok", nil
}

// selectionView is the console state.
type selectionView struct {
	Effective SelectionEffective `json:"effective"`
	Override  SelectionOverride  `json:"override"`
	CanRevert bool               `json:"can_revert"`
	By        string             `json:"by"`
	At        int64              `json:"at"`
}

// selectionPreview is the answer of a preview: the values, the changes, the groups of the
// profile and the result of the check.
type selectionPreview struct {
	Effective SelectionEffective `json:"effective"`
	Changes   []string           `json:"changes"`
	SingBox   []any              `json:"singbox"`
	Clash     []any              `json:"clash"`
	Check     string             `json:"check"`
}

func (a *Admin) registerSelection(mux *core.Mux) {
	mux.HandleFunc("GET /enroll/selection", asset("text/html; charset=utf-8", selectionHTML))
	mux.HandleFunc(
		"GET /enroll/static/selection.js",
		asset("text/javascript; charset=utf-8", selectionJS),
	)
	mux.HandleFunc("GET /enroll/selection/state", a.getSelection)
	mux.HandleFunc("GET /enroll/selection/export", a.exportSelection)
	mux.HandleFunc("POST /enroll/selection/preview", a.jsonOnly(a.previewSelection))
	mux.HandleFunc("POST /enroll/selection/apply", a.jsonOnly(a.applySelection))
	mux.HandleFunc("POST /enroll/selection/revert", a.jsonOnly(a.revertSelection))
}

// selectionNav is the console entry of the top bar.
func selectionNav() core.NavItem {
	return core.NavItem{
		Label: "Bridge selection",
		Path:  "/enroll/selection",
		Order: navOrderSelection,
		Group: "configure",
		Icon:  "nodes",
		Allow: authz.NavAllow(authz.P(authz.FeatureEnroll, authz.LevelView)),
	}
}

// fileCfg is the configuration of the moment without the override.
func (a *Admin) fileCfg(
	r *http.Request,
) IngressConfig {
	return a.reg.Current(r.Context(), false).Cfg
}

func (a *Admin) getSelection(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.SelectionDoc()
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(
		w,
		http.StatusOK,
		selectionView{Effective: Effective(a.fileCfg(r), d.Current), Override: d.Current,
			CanRevert: d.Previous != nil, By: d.By, At: d.At},
	)
}

func (a *Admin) exportSelection(w http.ResponseWriter, r *http.Request) {
	d, err := a.store.SelectionDoc()
	if err != nil {
		a.internal(w, err)
		return
	}
	writeJSON(w, http.StatusOK, Export(a.fileCfg(r), d.Current))
}

// readOverride reads {"override": {...}} strictly. ok=false means a response was written.
func readOverride(w http.ResponseWriter, r *http.Request) (SelectionOverride, bool) {
	obj, ok := readObject(w, r)
	if !ok {
		return SelectionOverride{}, false
	}
	raw, present := obj["override"]
	if !present || len(obj) != 1 {
		invalid(
			w,
			fieldError{
				"missing",
				[]any{"body", "override"},
				"the body must be {\"override\": {...}}",
			},
		)
		return SelectionOverride{}, false
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var o SelectionOverride
	if err := dec.Decode(&o); err != nil {
		invalid(
			w,
			fieldError{
				"value_error",
				[]any{"body", "override"},
				"unknown key or wrong type in override",
			},
		)
		return SelectionOverride{}, false
	}
	return o, true
}

// validated checks o and runs sing-box check. ok=false means a response was written.
func (a *Admin) validated(
	w http.ResponseWriter,
	r *http.Request,
	file IngressConfig,
	o SelectionOverride,
) (string, bool) {
	if err := o.Validate(file); err != nil {
		invalid(w, fieldError{"value_error", []any{"body", "override"}, err.Error()})
		return "", false
	}
	res, err := a.sel.check(r.Context(), ApplySelection(file, o))
	if err != nil {
		invalid(w, fieldError{"value_error", []any{"body", "override"}, err.Error()})
		return "", false
	}
	return res, true
}

func (a *Admin) previewSelection(w http.ResponseWriter, r *http.Request) {
	o, ok := readOverride(w, r)
	if !ok {
		return
	}
	file := a.fileCfg(r)
	res, ok := a.validated(w, r, file, o)
	if !ok {
		return
	}
	d, err := a.store.SelectionDoc()
	if err != nil {
		a.internal(w, err)
		return
	}
	changes := selectionChanges(Effective(file, d.Current), Effective(file, o))
	a.audit(r, ActionSelectionPreview, auditChanges(changes))
	eff := ApplySelection(file, o)
	writeJSON(w, http.StatusOK, selectionPreview{Effective: Effective(file, o), Changes: changes,
		SingBox: previewGroups(
			eff,
			singBoxGroups,
		), Clash: previewGroups(eff, clashGroups), Check: res})
}

// previewGroups are the selection groups of the profile. A profile with one bridge and no
// Hysteria 2 has none: the app uses the one bridge.
func previewGroups(cfg IngressConfig, f func(IngressConfig) []any) []any {
	if !cfg.grouped() {
		return []any{}
	}
	return f(cfg)
}

func (a *Admin) applySelection(w http.ResponseWriter, r *http.Request) {
	o, ok := readOverride(w, r)
	if !ok {
		return
	}
	selectionMu.Lock()
	defer selectionMu.Unlock()
	file := a.fileCfg(r)
	if _, ok := a.validated(w, r, file, o); !ok {
		return
	}
	d, err := a.store.SelectionDoc()
	if err != nil {
		a.internal(w, err)
		return
	}
	changes := selectionChanges(Effective(file, d.Current), Effective(file, o))
	prev := d.Current
	nd := SelectionDoc{Current: o, Previous: &prev, By: a.actor(r), At: a.store.Clock()().Unix()}
	if err := a.store.PutSelectionDoc(nd); err != nil {
		a.internal(w, err)
		return
	}
	a.audit(r, ActionSelectionApply, auditChanges(changes))
	a.getSelection(w, r)
}

func (a *Admin) revertSelection(w http.ResponseWriter, r *http.Request) {
	if _, ok := readObject(w, r); !ok {
		return
	}
	selectionMu.Lock()
	defer selectionMu.Unlock()
	d, err := a.store.SelectionDoc()
	if err != nil {
		a.internal(w, err)
		return
	}
	if d.Previous == nil {
		detail(w, http.StatusConflict, "there is no earlier value to go back to")
		return
	}
	file := a.fileCfg(r)
	changes := selectionChanges(Effective(file, d.Current), Effective(file, *d.Previous))
	// one step only: after a revert, the next revert has nothing to go back to
	nd := SelectionDoc{Current: *d.Previous, By: a.actor(r), At: a.store.Clock()().Unix()}
	if err := a.store.PutSelectionDoc(nd); err != nil {
		a.internal(w, err)
		return
	}
	a.audit(r, ActionSelectionRevert, auditChanges(changes))
	a.getSelection(w, r)
}

// selectionChanges lists each value that differs, as "name: old (source) -> new (source)".
func selectionChanges(old, cur SelectionEffective) []string {
	var out []string
	val := func(name string, o, n SelectionValue) {
		if fmt.Sprint(o.Value) != fmt.Sprint(n.Value) || o.Source != n.Source {
			out = append(
				out,
				fmt.Sprintf("%s: %v (%s) -> %v (%s)", name, o.Value, o.Source, n.Value, n.Source),
			)
		}
	}
	val("tolerance", old.Tolerance, cur.Tolerance)
	val("interval", old.Interval, cur.Interval)
	val("url", old.URL, cur.URL)
	val("strategy", old.Strategy, cur.Strategy)
	names := func(e SelectionEffective) []string {
		var n []string
		for _, b := range e.Bridges {
			n = append(n, b.Name)
		}
		return n
	}
	if on, nn := names(old), names(cur); !slices.Equal(on, nn) {
		out = append(
			out,
			fmt.Sprintf("order: %s -> %s", strings.Join(on, ","), strings.Join(nn, ",")),
		)
	}
	ob := map[string]SelectionBridge{}
	for _, b := range old.Bridges {
		ob[b.Name] = b
	}
	for _, b := range cur.Bridges {
		p := ob[b.Name]
		if p.Group != b.Group || p.GroupSource != b.GroupSource {
			out = append(
				out,
				fmt.Sprintf(
					"group %s: %q (%s) -> %q (%s)",
					b.Name,
					p.Group,
					p.GroupSource,
					b.Group,
					b.GroupSource,
				),
			)
		}
		if p.Drained != b.Drained {
			out = append(out, fmt.Sprintf("drain %s: %t -> %t", b.Name, p.Drained, b.Drained))
		}
	}
	if out == nil {
		out = []string{}
	}
	return out
}

// auditChanges is the audit detail: the changes, or "no change".
func auditChanges(c []string) string {
	if len(c) == 0 {
		return "no change"
	}
	return strings.Join(c, "; ")
}
