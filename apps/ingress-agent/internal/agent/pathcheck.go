package agent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Pathcheck is the continuous test of the path from this bridge (the edge) to the central
// control host: headscale, its DERP server and STUN. It runs from the network of the edge
// host, not through the tunnel, and it records which part of the path fails. Its results
// decide whether the edge-to-central traffic must be wrapped (see the README, "Path check").
//
// It is off unless INGRESS_PATHCHECK_TARGET is set. It opens no port and changes no
// access rule. It sends a few small requests to hosts the edge already talks to, plus one
// optional reference host, and an optional transfer within a daily byte budget.
type Pathcheck struct {
	Host       string   // the central host name (TLS server name of headscale)
	Port       int      // its HTTPS port
	ExpectIPs  []net.IP // optional: the addresses the name must resolve to
	HealthPath string   // headscale health path; "" skips the HTTP probe
	DERPPath   string   // DERP upgrade path; "" skips the DERP probe
	STUN       string   // host:port of STUN; "" skips the UDP probe
	BlobURL    string   // https URL of a file on the central side; "" skips the transfer
	BlobBytes  int64    // bytes to read from BlobURL (and RefURL) in one transfer
	RefURL     string   // https URL of a neutral reference host; "" skips the reference
	Tailscale  []string // command that prints `tailscale status --json`; nil skips it
	Interval   time.Duration
	BlobEvery  int   // run the transfer on every n-th check
	DailyBytes int64 // transfer budget per UTC day
	StatePath  string

	// For tests. nil: the system resolver, net.Dialer, the system roots, time.Now.
	Lookup func(ctx context.Context, host string) ([]net.IP, error)
	Dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	TLS    *tls.Config
	Now    func() time.Time
	// Exec runs the Tailscale command and returns its stdout (tests). nil: os/exec.
	Exec func(ctx context.Context, argv []string) ([]byte, error)
	// StallAfter is the transfer stall limit (tests). 0: the default.
	StallAfter time.Duration

	mu    sync.Mutex
	st    pathState
	runs  int
	ready bool
}

// pathState is what the check keeps between runs and restarts (pathcheck.json).
type pathState struct {
	Last      *PathResult  `json:"last,omitempty"`
	History   []PathSample `json:"history"`
	Since     int64        `json:"since,omitempty"`
	Streak    int          `json:"streak"`
	BytesDay  string       `json:"bytes_day,omitempty"`
	BytesUsed int64        `json:"bytes_used"`
}

// PathSample is one past run, kept short for the history.
type PathSample struct {
	T     int64  `json:"t"`
	Class string `json:"c"`
	TCPMs int64  `json:"tcp_ms,omitempty"`
	Kbps  int64  `json:"kbps,omitempty"`
}

// PathcheckView is the answer of GET /v1/pathcheck.
type PathcheckView struct {
	Disabled   bool           `json:"disabled,omitempty"`
	IntervalS  int64          `json:"interval_s,omitempty"`
	Class      string         `json:"class,omitempty"`
	Since      int64          `json:"since,omitempty"`
	Streak     int            `json:"streak,omitempty"`
	Checked    int64          `json:"checked,omitempty"`
	Counts     map[string]int `json:"counts,omitempty"`
	BytesToday int64          `json:"bytes_today,omitempty"`
	Last       *PathResult    `json:"last,omitempty"`
	History    []PathSample   `json:"history,omitempty"`
}

const (
	defaultPathInterval = 15 * time.Minute
	minPathInterval     = time.Minute
	defaultBlobEvery    = 4
	defaultBlobBytes    = 1 << 20
	maxBlobBytes        = 16 << 20
	defaultDailyBytes   = 50 << 20
	defaultHealthPath   = "/health"
	defaultDERPPath     = "/derp"
	defaultSTUNPort     = "3478"
	pathHistoryLen      = 96 // 24 hours at the default interval
	pathJitterRange     = 0.4
	pathJitterBase      = 0.8
	pathStateMode       = 0o600
	pathOff             = "-"
	pathEnvTarget       = "INGRESS_PATHCHECK_TARGET"
	pathClassOK         = "ok"
)

// LoadPathcheck reads the INGRESS_PATHCHECK_* variables. An empty target turns the check off.
// A bad value names the variable, never the value.
func LoadPathcheck(getenv func(string) string, stateDir string) (*Pathcheck, error) {
	target := getenv(pathEnvTarget)
	if target == "" {
		return nil, nil
	}
	p := &Pathcheck{StatePath: filepath.Join(stateDir, "pathcheck.json")}
	if err := p.loadTarget(target); err != nil {
		return nil, err
	}
	if err := p.loadProbes(getenv); err != nil {
		return nil, err
	}
	if err := p.loadSchedule(getenv); err != nil {
		return nil, err
	}
	if f := getenv("INGRESS_PATHCHECK_CA"); f != "" { // tests only: production uses system roots
		pem, err := os.ReadFile(f) //nolint:gosec // a path the operator sets in the env file
		pool := x509.NewCertPool()
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			return nil, &InfoConfigError{Var: "INGRESS_PATHCHECK_CA"}
		}
		p.TLS = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool}
	}
	return p, nil
}

func (p *Pathcheck) loadTarget(target string) error {
	host, raw := target, strconv.Itoa(httpsPort)
	if h, pt, err := net.SplitHostPort(target); err == nil {
		host, raw = h, pt
	}
	n, err := port(raw, pathEnvTarget)
	if err != nil {
		return err
	}
	if !dnsName(host) && net.ParseIP(host) == nil {
		return &InfoConfigError{Var: pathEnvTarget}
	}
	p.Host, p.Port = host, n
	return nil
}

func (p *Pathcheck) loadProbes(getenv func(string) string) error {
	for _, s := range ParseList(getenv("INGRESS_PATHCHECK_ADDRS")) {
		ip := net.ParseIP(s)
		if ip == nil {
			return &InfoConfigError{Var: "INGRESS_PATHCHECK_ADDRS"}
		}
		p.ExpectIPs = append(p.ExpectIPs, ip)
	}
	var err error
	if p.HealthPath, err = pathVar(getenv, "INGRESS_PATHCHECK_HEALTH_PATH",
		defaultHealthPath); err != nil {
		return err
	}
	if p.DERPPath, err = pathVar(getenv, "INGRESS_PATHCHECK_DERP_PATH",
		defaultDERPPath); err != nil {
		return err
	}
	switch s := getenv("INGRESS_PATHCHECK_STUN"); s {
	case pathOff:
	case "":
		p.STUN = net.JoinHostPort(p.Host, defaultSTUNPort)
	default:
		if _, _, err := net.SplitHostPort(s); err != nil {
			return &InfoConfigError{Var: "INGRESS_PATHCHECK_STUN"}
		}
		p.STUN = s
	}
	if p.BlobURL, err = httpsVar(getenv, "INGRESS_PATHCHECK_BLOB_URL"); err != nil {
		return err
	}
	if p.RefURL, err = httpsVar(getenv, "INGRESS_PATHCHECK_REF_URL"); err != nil {
		return err
	}
	if ts := getenv("INGRESS_PATHCHECK_TAILSCALE"); ts != "" {
		if !strings.HasPrefix(ts, "/") || strings.ContainsAny(ts, " \t") {
			return &InfoConfigError{Var: "INGRESS_PATHCHECK_TAILSCALE"}
		}
		p.Tailscale = []string{ts, "status", "--json"}
		if sock := getenv("INGRESS_PATHCHECK_TAILSCALE_SOCKET"); sock != "" {
			p.Tailscale = []string{ts, "--socket=" + sock, "status", "--json"}
		}
	}
	return nil
}

func (p *Pathcheck) loadSchedule(getenv func(string) string) error {
	p.Interval = defaultPathInterval
	if s := getenv("INGRESS_PATHCHECK_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < minPathInterval {
			return &InfoConfigError{Var: "INGRESS_PATHCHECK_INTERVAL"}
		}
		p.Interval = d
	}
	var err error
	if p.BlobEvery, err = intVar(getenv, "INGRESS_PATHCHECK_BLOB_EVERY", defaultBlobEvery, 1,
		pathHistoryLen); err != nil {
		return err
	}
	blob, err := intVar(getenv, "INGRESS_PATHCHECK_BLOB_BYTES", defaultBlobBytes, 1, maxBlobBytes)
	if err != nil {
		return err
	}
	p.BlobBytes = int64(blob)
	daily, err := intVar(getenv, "INGRESS_PATHCHECK_DAILY_BYTES", defaultDailyBytes, 0,
		1<<40) //nolint:mnd // 1 TiB: a sanity cap only
	p.DailyBytes = int64(daily)
	return err
}

func pathVar(getenv func(string) string, name, def string) (string, error) {
	switch s := getenv(name); {
	case s == pathOff:
		return "", nil
	case s == "":
		return def, nil
	case !strings.HasPrefix(s, "/") || strings.ContainsAny(s, " \t\r\n?#"):
		return "", &InfoConfigError{Var: name}
	default:
		return s, nil
	}
}

func httpsVar(getenv func(string) string, name string) (string, error) {
	s := getenv(name)
	if s == "" {
		return "", nil
	}
	if u, err := url.Parse(s); err != nil || u.Scheme != "https" || u.Hostname() == "" ||
		u.User != nil {
		return "", &InfoConfigError{Var: name}
	}
	return s, nil
}

func intVar(getenv func(string) string, name string, def, lo, hi int) (int, error) {
	s := getenv(name)
	if s == "" {
		return def, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi {
		return 0, &InfoConfigError{Var: name}
	}
	return n, nil
}

func (p *Pathcheck) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Restore loads the state of an earlier run. A missing or unreadable file starts empty.
func (p *Pathcheck) Restore() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.ready = true
	b, err := os.ReadFile(p.StatePath)
	if err != nil {
		return
	}
	var st pathState
	if json.Unmarshal(b, &st) == nil {
		if len(st.History) > pathHistoryLen {
			st.History = st.History[len(st.History)-pathHistoryLen:]
		}
		p.st = st
	}
}

// Loop runs a check at once, then about every Interval (with jitter, so that the checks of
// many edges do not line up), until ctx ends.
func (p *Pathcheck) Loop(ctx context.Context) {
	for {
		p.Run(ctx)
		//nolint:gosec // jitter only, not security relevant
		f := pathJitterBase + rand.Float64()*pathJitterRange
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(float64(p.Interval) * f)):
		}
	}
}

// Run does one check, records it, and returns it.
func (p *Pathcheck) Run(ctx context.Context) PathResult {
	p.mu.Lock()
	if !p.ready {
		p.mu.Unlock()
		p.Restore()
		p.mu.Lock()
	}
	p.runs++
	withBlob := p.BlobURL != "" && (p.runs-1)%max(p.BlobEvery, 1) == 0 && p.budgetLeftLocked()
	p.mu.Unlock()

	start := p.now()
	r := p.probe(ctx, withBlob)
	r.Checked = p.now().Unix()
	r.Duration = p.now().Sub(start).Milliseconds()
	r.Class, r.Findings = Classify(&r)
	p.record(r)
	return r
}

func (p *Pathcheck) day() string { return p.now().UTC().Format(time.DateOnly) }

func (p *Pathcheck) budgetLeftLocked() bool {
	if p.st.BytesDay != p.day() {
		p.st.BytesDay, p.st.BytesUsed = p.day(), 0
	}
	need := p.BlobBytes
	if p.RefURL != "" {
		need *= 2 // the reference transfer runs right after it
	}
	return p.st.BytesUsed+need <= p.DailyBytes
}

func (p *Pathcheck) record(r PathResult) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.st.BytesDay != p.day() {
		p.st.BytesDay, p.st.BytesUsed = p.day(), 0
	}
	if r.Blob != nil {
		p.st.BytesUsed += r.Blob.Bytes
	}
	if r.RefBlob != nil {
		p.st.BytesUsed += r.RefBlob.Bytes
	}
	if p.st.Last == nil || p.st.Last.Class != r.Class {
		p.st.Since, p.st.Streak = r.Checked, 0
	}
	p.st.Streak++
	last := r
	p.st.Last = &last
	s := PathSample{T: r.Checked, Class: r.Class}
	if r.TCP != nil {
		s.TCPMs = r.TCP.Ms
	}
	if r.Blob != nil {
		s.Kbps = r.Blob.Kbps
	}
	p.st.History = append(p.st.History, s)
	if len(p.st.History) > pathHistoryLen {
		p.st.History = p.st.History[len(p.st.History)-pathHistoryLen:]
	}
	p.saveLocked()
}

// saveLocked writes the state atomically; a failure only loses history across a restart.
func (p *Pathcheck) saveLocked() {
	b, err := json.Marshal(p.st)
	if err != nil {
		return
	}
	f, err := os.CreateTemp(filepath.Dir(p.StatePath), ".pathcheck.*")
	if err != nil {
		return
	}
	tmp := f.Name()
	_, werr := f.Write(b)
	cerr := f.Close()
	if werr != nil || cerr != nil || os.Chmod(tmp, pathStateMode) != nil ||
		os.Rename(tmp, p.StatePath) != nil {
		_ = os.Remove(tmp)
	}
}

// View is the answer of GET /v1/pathcheck. A nil check answers {"disabled": true}.
func (p *Pathcheck) View() PathcheckView {
	if p == nil {
		return PathcheckView{Disabled: true}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	v := PathcheckView{IntervalS: int64(p.Interval / time.Second), Since: p.st.Since,
		Streak: p.st.Streak, Counts: map[string]int{},
		History: append([]PathSample{}, p.st.History...)}
	if p.st.BytesDay == p.day() {
		v.BytesToday = p.st.BytesUsed
	}
	if p.st.Last != nil {
		last := *p.st.Last
		v.Last, v.Class, v.Checked = &last, last.Class, last.Checked
	}
	for _, s := range p.st.History {
		v.Counts[s.Class]++
	}
	return v
}
