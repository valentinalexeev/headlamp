package agent

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// ErrApply means the new config did not validate or could not be installed; nothing changed.
var ErrApply = errors.New("apply failed")

// Applier owns the credential set and the live xray config.
type Applier struct {
	StatePath, BasePath, ConfigPath string
	Runner                          *Runner
	Clock                           func() int64
	DirectGeoIP, DirectGeoSite      []string
	// ExitDNS are the resolvers reached through via-exit; empty: DefaultExitDNS.
	ExitDNS []string
	Log     *slog.Logger
	mu      sync.Mutex
	// rendered is the list of grant group outbounds of the base at the last render (#139). When a
	// playbook run adds one, the next push renders again even if the credentials did not change.
	// nil: not rendered yet.
	rendered *string
}

func (a *Applier) now() int64 {
	if a.Clock != nil {
		return a.Clock()
	}
	return time.Now().Unix()
}

// Current is the persisted set.
func (a *Applier) Current() ([]Cred, error) { return Load(a.StatePath) }

// baseGroups is the grant group outbounds of the base config, as one string.
func (a *Applier) baseGroups() string {
	base, err := os.ReadFile(a.BasePath)
	if err != nil {
		return ""
	}
	return strings.Join(GroupOutbounds(base), ",")
}

func hasGroupGrant(creds []Cred) bool {
	for _, c := range creds {
		if groupIDRe.MatchString(c.Tailnet) {
			return true
		}
	}
	return false
}

func (a *Applier) render(creds []Cred) ([]byte, error) {
	base, err := os.ReadFile(a.BasePath)
	if err != nil {
		return nil, err
	}
	return Render(base, creds, a.DirectGeoIP, a.DirectGeoSite, a.ExitDNS)
}

// install writes the config next to the live one, has xray validate it, then replaces the live file. The
// temporary name ends in .json: xray picks the config format from the extension.
func (a *Applier) install(ctx context.Context, cfg []byte) error {
	dir := filepath.Dir(a.ConfigPath)
	f, err := os.CreateTemp(dir, "."+filepath.Base(a.ConfigPath)+".*.json")
	if err != nil {
		return fmt.Errorf("%w: %v", ErrApply, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	_, werr := f.Write(cfg)
	cerr := f.Close()
	if werr != nil || cerr != nil {
		return fmt.Errorf("%w: write", ErrApply)
	}
	mode := os.FileMode(0o640)
	if st, err := os.Stat(a.BasePath); err == nil {
		mode = st.Mode().Perm()
	}
	_ = os.Chmod(tmp, mode)
	if !a.Runner.TestConfig(ctx, tmp) {
		return fmt.Errorf("%w: xray rejected the generated config", ErrApply)
	}
	if err := os.Rename(tmp, a.ConfigPath); err != nil {
		return fmt.Errorf("%w: %v", ErrApply, err)
	}
	return nil
}

// Apply makes the set desired: it returns true when something changed. The set is saved only after xray restarted.
func (a *Applier) Apply(ctx context.Context, desired []Cred) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.applyLocked(ctx, desired)
}

func (a *Applier) applyLocked(ctx context.Context, desired []Cred) (bool, error) {
	desired = Sweep(desired, a.now())
	cur, err := a.Current()
	if err != nil {
		return false, fmt.Errorf("%w: state unreadable", ErrApply)
	}
	added, removed := Diff(cur, desired)
	groups := a.baseGroups()
	sameGroups := !hasGroupGrant(desired) || (a.rendered != nil && *a.rendered == groups)
	if len(added) == 0 && len(removed) == 0 && sameGroups {
		return false, nil
	}
	cfg, err := a.render(desired)
	if err != nil {
		return false, fmt.Errorf("%w: render", ErrApply)
	}
	if err := a.install(ctx, cfg); err != nil {
		return false, err
	}
	if err := a.Runner.Restart(ctx); err != nil {
		return false, fmt.Errorf("%w: restart", ErrApply)
	}
	if err := Save(a.StatePath, desired); err != nil {
		return false, fmt.Errorf("%w: save", ErrApply)
	}
	a.rendered = &groups
	a.Log.Info(fmt.Sprintf("applied: +%d -%d", len(added), len(removed)))
	return true, nil
}

// Expire drops expired credentials, as one step with the apply, so a concurrent PUT is never reverted.
func (a *Applier) Expire(ctx context.Context) (bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, err := a.Current()
	if err != nil {
		return false, err
	}
	alive := Sweep(cur, a.now())
	if len(alive) == len(cur) {
		return false, nil
	}
	return a.applyLocked(ctx, alive)
}

// Restore re-renders the persisted set after a reboot and restarts xray only if the file on disk differs.
func (a *Applier) Restore(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	cur, err := a.Current()
	if err != nil {
		return err
	}
	creds := Sweep(cur, a.now())
	cfg, err := a.render(creds)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(a.ConfigPath); err == nil && sameJSON(old, cfg) {
		return nil
	}
	if err := a.install(ctx, cfg); err != nil {
		return err
	}
	if err := a.Runner.Restart(ctx); err != nil {
		return err
	}
	if err := Save(a.StatePath, creds); err != nil {
		return err
	}
	a.Log.Info(fmt.Sprintf("restored: %d", len(creds)))
	return nil
}

func sameJSON(a, b []byte) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return bytes.Equal(xa, ya)
}

// SweepLoop expires credentials every interval until ctx ends. A failure is logged by type, and the loop goes on.
func SweepLoop(ctx context.Context, a *Applier, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := a.Expire(ctx); err != nil {
				a.Log.Error("expiry sweep failed")
			}
		}
	}
}

// --- HTTP ---

const maxBody = 4 << 20

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{4}-?[0-9a-fA-F]{12}$`)

// canonUUID gives the lower-case, hyphenated form of a uuid (as Python's uuid.UUID does), or "".
func canonUUID(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(strings.TrimPrefix(s, "urn:uuid:"), "{")
	s = strings.TrimSuffix(s, "}")
	if !uuidRE.MatchString(s) {
		return ""
	}
	h := strings.ToLower(strings.ReplaceAll(s, "-", ""))
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// parseCreds reads and validates the PUT body: an object with "credentials", each a uuid, a known mode and an
// integer not_after; uuids are unique. Any problem is false.
func parseCreds(body []byte) ([]Cred, bool) {
	var raw struct {
		Credentials *[]map[string]json.RawMessage `json:"credentials"`
	}
	if json.Unmarshal(body, &raw) != nil || raw.Credentials == nil {
		return nil, false
	}
	out := make([]Cred, 0, len(*raw.Credentials))
	seen := map[string]bool{}
	for _, item := range *raw.Credentials {
		var u, m, tn string
		if json.Unmarshal(item["uuid"], &u) != nil || json.Unmarshal(item["mode"], &m) != nil {
			return nil, false
		}
		raw, ok := item["tailnet"]
		if ok && (json.Unmarshal(raw, &tn) != nil || !ValidTailnet(tn)) {
			return nil, false
		}
		u = canonUUID(u)
		if u == "" || !slices.Contains(Modes, m) {
			return nil, false
		}
		na := bytes.TrimSpace(item["not_after"])
		if len(na) == 0 || bytes.ContainsAny(na, ".eE\"") {
			return nil, false // a string, a float or an exponent: an integer is required
		}
		var n int64
		if json.Unmarshal(na, &n) != nil {
			return nil, false
		}
		if seen[u] {
			return nil, false
		}
		seen[u] = true
		out = append(out, Cred{UUID: u, Mode: m, NotAfter: n, Tailnet: tn})
	}
	return out, true
}

// Server is the agent's HTTP API.
type Server struct {
	A     *Applier
	Token string
	Info  *Info
	// Egress is the bridge path self-test (#196); nil: GET /v1/egress answers {"disabled": true}.
	Egress *Egress
	Clock  func() int64
}

func (s *Server) now() int64 {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now().Unix()
}

// Handler serves PUT /v1/credentials, GET /v1/stats, GET /v1/info and GET /v1/egress, each behind
// the bearer token. Every error
// has an empty body. Nothing else exists.
func (s *Server) Handler() http.Handler {
	want := []byte("Bearer " + s.Token)
	authed := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			got := []byte(r.Header.Get("Authorization"))
			if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			h(w, r)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("PUT /v1/credentials", authed(s.putCredentials))
	mux.HandleFunc("GET /v1/stats", authed(s.stats))
	mux.HandleFunc("GET /v1/info", authed(s.info))
	mux.HandleFunc("GET /v1/egress", authed(s.egress))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, pattern := mux.Handler(r)
		if pattern == "" {
			// no body, and a wrong method on a known path is still answered after the token check
			switch r.URL.Path {
			case "/v1/credentials", "/v1/stats", "/v1/info", "/v1/egress":
				authed(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusMethodNotAllowed) })(w, r)
				return
			}
			w.WriteHeader(http.StatusNotFound)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) putCredentials(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil || len(body) > maxBody {
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		return
	}
	creds, ok := parseCreds(body)
	if !ok {
		w.WriteHeader(http.StatusUnprocessableEntity)
		return
	}
	creds = Sweep(creds, s.now())
	if len(creds) == 0 && r.URL.Query().Get("confirm_empty") != "true" {
		w.WriteHeader(http.StatusConflict)
		return
	}
	if _, err := s.A.Apply(r.Context(), creds); err != nil {
		s.A.Log.Error("apply failed: " + strings.TrimPrefix(err.Error(), ErrApply.Error()+": "))
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// tailnet_groups tells node-control which grant groups this bridge can serve (#139): a group
	// without its outbound shows "needs provisioning" in the console. Only opaque group ids.
	groups := []string{}
	if base, err := os.ReadFile(s.A.BasePath); err == nil {
		groups = GroupOutbounds(base)
	}
	writeJSON(w, map[string]any{"count": len(creds), "tailnet_groups": groups})
}

func (s *Server) stats(w http.ResponseWriter, r *http.Request) {
	by := s.A.Runner.Stats(r.Context())
	out := map[string]Traffic{}
	cur, _ := s.A.Current()
	for _, c := range cur {
		if t, ok := by[EmailFor(c.UUID)]; ok {
			out[OpaqueID(c.UUID)] = t
		}
	}
	writeJSON(w, out)
}

// egress gives the last bridge path self-test (#196). It never probes on request: the loop does.
func (s *Server) egress(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, s.Egress.Result())
}

func (s *Server) info(w http.ResponseWriter, _ *http.Request) {
	if s.Info == nil {
		w.WriteHeader(http.StatusServiceUnavailable) // this deployment does not configure it
		return
	}
	writeJSON(w, s.Info)
}
