package tasks

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"example.com/node-control/internal/authz"
)

const (
	maxConfigBody  = 1 << 20
	maxSecretsBody = 64 << 10
	maxScriptBytes = 256 << 10
	maxSecretValue = 4096
	maxSecretCount = 64
	maxRunsListed  = 50
)

var (
	scriptNameRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	secretNameRe = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)
	// Task ids that would be shadowed by the fixed /tasks/... routes.
	reservedTaskIDs = map[string]bool{"config": true, "secrets": true}
)

// placeholderPage stands in until the tasks page exists.
const placeholderPage = `<!doctype html><html><head><meta charset="utf-8"><title>Tasks</title></head>` +
	`<body><h1>Tasks</h1></body></html>`

// api holds the HTTP handlers. Every handler reads the store and engine once
// through Module.current and answers 503 while they are not open.
type api struct{ m *Module }

func (a *api) register(mux *http.ServeMux, gate func(http.Handler) http.Handler) {
	plain := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(h)) }
	body := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, gate(authz.RequireJSON(h))) }
	plain("GET /tasks", a.list)
	plain("GET /tasks/{id}", a.task)
	body("POST /tasks/{id}/run", a.trigger)
	plain("GET /tasks/{id}/runs", a.runs)
	plain("GET /runs/{id}", a.run)
	plain("DELETE /runs/{id}", a.cancel)
	plain("GET /tasks/config", a.getConfig)
	body("PUT /tasks/config", a.putConfig)
	plain("GET /tasks/secrets", a.listSecrets)
	body("PUT /tasks/secrets", a.putSecrets)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
}

// fail answers {"error": msg}; msg must never carry a secret value.
func fail(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// open returns the store and engine, or answers 503 and false.
func (a *api) open(w http.ResponseWriter) (*Store, *Engine, bool) {
	s, e := a.m.current()
	if s == nil || e == nil {
		fail(w, http.StatusServiceUnavailable, "degraded")
		return nil, nil, false
	}
	return s, e, true
}

// readBody decodes a size-limited JSON body into dst; it answers the error
// itself (without echoing the body) and reports success.
func readBody(w http.ResponseWriter, r *http.Request, limit int64, dst any, strict bool) bool {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	dec := json.NewDecoder(r.Body)
	if strict {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(dst); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			fail(w, http.StatusRequestEntityTooLarge, "request body too large")
		} else {
			fail(w, http.StatusUnprocessableEntity, "the body must be a JSON object with the expected fields")
		}
		return false
	}
	return true
}

func wantsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}

// --- tasks and runs ----------------------------------------------------------

func (a *api) view(s *Store, e *Engine, id string, t Task) taskView {
	var active *Run
	if r, ok := e.Running(id); ok {
		active = &r
	}
	return buildTaskView(a.m.now(), id, t, s.State(id), s.Runs(id), active)
}

// list serves the task list as JSON, or the page to a browser navigation.
func (a *api) list(w http.ResponseWriter, r *http.Request) {
	if wantsHTML(r) {
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
		w.Write([]byte(placeholderPage))
		return
	}
	s, e, ok := a.open(w)
	if !ok {
		return
	}
	out := []taskView{}
	if cfg := s.Config(); cfg != nil {
		for _, id := range sortedTasks(cfg) {
			out = append(out, a.view(s, e, id, cfg.Tasks[id]))
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) task(w http.ResponseWriter, r *http.Request) {
	s, e, ok := a.open(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	cfg := s.Config()
	if cfg == nil {
		fail(w, http.StatusNotFound, "unknown task")
		return
	}
	t, found := cfg.Tasks[id]
	if !found {
		fail(w, http.StatusNotFound, "unknown task")
		return
	}
	writeJSON(w, http.StatusOK, a.view(s, e, id, t))
}

func (a *api) trigger(w http.ResponseWriter, r *http.Request) {
	_, e, ok := a.open(w)
	if !ok {
		return
	}
	id, err := e.Trigger(r.PathValue("id"))
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"run_id": id})
	case errors.Is(err, ErrAlreadyRunning):
		fail(w, http.StatusConflict, "task is already running")
	case errors.Is(err, ErrUnknownTask):
		fail(w, http.StatusNotFound, "unknown task")
	case errors.Is(err, ErrStopped):
		fail(w, http.StatusServiceUnavailable, "stopped")
	default:
		fail(w, http.StatusInternalServerError, "trigger failed")
	}
}

func (a *api) runs(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.open(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	cfg := s.Config()
	if cfg == nil {
		fail(w, http.StatusNotFound, "unknown task")
		return
	}
	if _, found := cfg.Tasks[id]; !found {
		fail(w, http.StatusNotFound, "unknown task")
		return
	}
	rs := s.Runs(id)
	if len(rs) > maxRunsListed {
		rs = rs[:maxRunsListed]
	}
	out := make([]runView, 0, len(rs))
	for _, x := range rs {
		out = append(out, toRunView(x))
	}
	writeJSON(w, http.StatusOK, out)
}

// findRun looks for a run among the executing attempts, then in the history
// of every configured task.
func findRun(s *Store, e *Engine, id string) (Run, bool) {
	cfg := s.Config()
	if cfg == nil {
		return Run{}, false
	}
	tasks := sortedTasks(cfg)
	for _, t := range tasks {
		if r, ok := e.Running(t); ok && r.ID == id {
			return r, true
		}
	}
	for _, t := range tasks {
		for _, r := range s.Runs(t) {
			if r.ID == id {
				return r, true
			}
		}
	}
	return Run{}, false
}

func (a *api) run(w http.ResponseWriter, r *http.Request) {
	s, e, ok := a.open(w)
	if !ok {
		return
	}
	run, found := findRun(s, e, r.PathValue("id"))
	if !found {
		fail(w, http.StatusNotFound, "unknown run")
		return
	}
	writeJSON(w, http.StatusOK, toRunView(run))
}

// cancel: 204 when the executing attempt was cancelled; 409 for a run that is
// recorded as running or queued but is not executing in this process (it
// cannot be cancelled from here); 404 for an unknown or finished run.
func (a *api) cancel(w http.ResponseWriter, r *http.Request) {
	s, e, ok := a.open(w)
	if !ok {
		return
	}
	id := r.PathValue("id")
	err := e.Cancel(id)
	if err == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if run, found := findRun(s, e, id); found && (run.State == RunRunning || run.State == RunQueued) {
		fail(w, http.StatusConflict, "run is not executing in this process")
		return
	}
	fail(w, http.StatusNotFound, "unknown or finished run")
}

// --- config ------------------------------------------------------------------

// referencedScripts lists the scripts the config's tasks use, sorted.
func referencedScripts(cfg *Config) []string {
	if cfg == nil {
		return nil
	}
	set := map[string]bool{}
	for _, t := range cfg.Tasks {
		set[t.Script] = true
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// secretRefs lists, per task, the secret names it needs: its secrets, its
// healthcheck and its notification topics.
func secretRefs(t Task) []string {
	out := append([]string(nil), t.Secrets...)
	if t.Healthcheck != "" {
		out = append(out, t.Healthcheck)
	}
	if t.Alert != nil && t.Alert.Notify.Topic != "" {
		out = append(out, t.Alert.Notify.Topic)
	}
	if t.OnFailure != nil && t.OnFailure.Notify.Topic != "" {
		out = append(out, t.OnFailure.Notify.Topic)
	}
	return out
}

func (a *api) getConfig(w http.ResponseWriter, _ *http.Request) {
	s, _, ok := a.open(w)
	if !ok {
		return
	}
	cfg := s.Config()
	if cfg == nil {
		fail(w, http.StatusNotFound, "no config")
		return
	}
	scripts := map[string]string{}
	for _, n := range referencedScripts(cfg) {
		if text, found := s.Script(n); found {
			scripts[n] = text
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": cfg, "scripts": scripts})
}

type changeSet struct {
	Added   []string `json:"added"`
	Changed []string `json:"changed"`
	Removed []string `json:"removed"`
}

// diff compares two name -> content maps.
func diff(old, cur map[string]string) changeSet {
	cs := changeSet{Added: []string{}, Changed: []string{}, Removed: []string{}}
	for n, v := range cur {
		switch o, found := old[n]; {
		case !found:
			cs.Added = append(cs.Added, n)
		case o != v:
			cs.Changed = append(cs.Changed, n)
		}
	}
	for n := range old {
		if _, found := cur[n]; !found {
			cs.Removed = append(cs.Removed, n)
		}
	}
	sort.Strings(cs.Added)
	sort.Strings(cs.Changed)
	sort.Strings(cs.Removed)
	return cs
}

// normalisedTasks is the normalised form of each task (defaults applied, keys sorted)
// that decides whether a task changed.
func normalisedTasks(cfg *Config) map[string]string {
	out := map[string]string{}
	if cfg == nil {
		return out
	}
	for id, t := range cfg.Tasks {
		b, _ := json.Marshal(t)
		out[id] = string(b)
	}
	return out
}

func problemsOf(err error) []string {
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []string
		for _, e := range j.Unwrap() {
			out = append(out, problemsOf(e)...)
		}
		return out
	}
	return []string{err.Error()}
}

func (a *api) putConfig(w http.ResponseWriter, r *http.Request) {
	s, e, ok := a.open(w)
	if !ok {
		return
	}
	var in struct {
		Config  json.RawMessage   `json:"config"`
		Scripts map[string]string `json:"scripts"`
	}
	if !readBody(w, r, maxConfigBody, &in, true) {
		return
	}
	if len(bytes.TrimSpace(in.Config)) == 0 || string(bytes.TrimSpace(in.Config)) == "null" {
		fail(w, http.StatusUnprocessableEntity, "config is required")
		return
	}
	for n, text := range in.Scripts {
		if !scriptNameRe.MatchString(n) {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("script name %q must match %s", n, scriptNameRe))
			return
		}
		if len(text) > maxScriptBytes {
			fail(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("script %s is larger than 256 KiB", n))
			return
		}
	}
	cfg, err := ParseConfig(in.Config)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid task config", "problems": problemsOf(err)})
		return
	}

	// Checks against what is stored. Secrets must be pushed first.
	known := map[string]bool{}
	for _, n := range s.SecretNames() {
		known[n] = true
	}
	var problems []string
	for _, id := range sortedTasks(cfg) {
		t := cfg.Tasks[id]
		if reservedTaskIDs[id] {
			problems = append(problems, fmt.Sprintf("task %s: id: reserved, choose another name", id))
		}
		for _, n := range secretRefs(t) {
			if !known[n] {
				problems = append(problems, fmt.Sprintf("task %s: secret %s is not stored (push secrets first)", id, n))
			}
		}
	}
	newScripts := map[string]string{}
	toStore := map[string]string{}
	for _, n := range referencedScripts(cfg) {
		stored, haveStored := s.Script(n)
		text, inBody := in.Scripts[n]
		switch {
		case inBody:
			newScripts[n] = text
			if !haveStored || stored != text {
				toStore[n] = text
			}
		case haveStored:
			newScripts[n] = stored
		default:
			problems = append(problems, fmt.Sprintf("script %s is referenced but neither in the request nor stored", n))
		}
	}
	if len(problems) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "invalid task config", "problems": problems})
		return
	}

	old := s.Config()
	oldScripts := map[string]string{}
	for _, n := range referencedScripts(old) {
		if text, found := s.Script(n); found {
			oldScripts[n] = text
		}
	}
	taskDiff := diff(normalisedTasks(old), normalisedTasks(cfg))
	scriptDiff := diff(oldScripts, newScripts)
	dryRun := r.URL.Query().Get("dry_run")
	dry := dryRun == "1" || dryRun == "true"
	out := map[string]any{"tasks": taskDiff, "scripts": scriptDiff, "dry_run": dry}
	if !dry {
		unchanged := len(taskDiff.Added)+len(taskDiff.Changed)+len(taskDiff.Removed) == 0 && len(toStore) == 0
		if !unchanged {
			// A failed write keeps the new config in memory (it is live) and
			// the store retries it; the answer says so.
			if err := s.PutConfig(cfg, toStore); err != nil {
				a.m.log.Warn("task config not yet written to Redis", "err", err)
				out["pending_write"] = true
			}
			e.Reload()
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// --- secrets -----------------------------------------------------------------

func (a *api) listSecrets(w http.ResponseWriter, _ *http.Request) {
	s, _, ok := a.open(w)
	if !ok {
		return
	}
	type entry struct {
		Name string `json:"name"`
		Set  bool   `json:"set"`
	}
	out := []entry{}
	for _, n := range s.SecretNames() {
		out = append(out, entry{Name: n, Set: true})
	}
	writeJSON(w, http.StatusOK, out)
}

// putSecrets stores named values. No answer, error or log line carries a
// value, and an invalid name is not echoed either (it may be a pasted value).
func (a *api) putSecrets(w http.ResponseWriter, r *http.Request) {
	s, _, ok := a.open(w)
	if !ok {
		return
	}
	var in map[string]string
	if !readBody(w, r, maxSecretsBody, &in, false) {
		return
	}
	switch {
	case len(in) == 0:
		fail(w, http.StatusUnprocessableEntity, "no secrets given")
		return
	case len(in) > maxSecretCount:
		fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("at most %d secrets per request", maxSecretCount))
		return
	}
	names := make([]string, 0, len(in))
	for n, v := range in {
		if !secretNameRe.MatchString(n) {
			fail(w, http.StatusUnprocessableEntity, "a secret name is invalid: use A-Z, a-z, 0-9 and _, at most 64")
			return
		}
		if len(v) == 0 || len(v) > maxSecretValue {
			fail(w, http.StatusUnprocessableEntity, fmt.Sprintf("secret %s: the value must be 1 to %d bytes", n, maxSecretValue))
			return
		}
		names = append(names, n)
	}
	sort.Strings(names)
	pending := false
	for _, n := range names {
		if cur, err := s.Secrets([]string{n}); err == nil && cur[n] == in[n] {
			continue // unchanged: no write
		}
		if err := s.PutSecret(n, in[n]); err != nil {
			a.m.log.Warn("task secret not yet written to Redis", "name", n)
			pending = true
		}
	}
	out := map[string]any{"stored": names}
	if pending {
		out["pending_write"] = true
	}
	writeJSON(w, http.StatusOK, out)
}
