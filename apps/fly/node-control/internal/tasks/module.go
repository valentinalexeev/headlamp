package tasks

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"tailscale.com/ipn"

	"example.com/node-control/internal/authz"
	"example.com/node-control/internal/core"
	"example.com/node-control/internal/headscale"
	"example.com/node-control/internal/scrub"
	"example.com/node-control/internal/statestore"
)

const (
	// redisHashKey is the Redis hash holding the task data; it is also the
	// encryption scope, so a value copied elsewhere does not decrypt.
	redisHashKey = "node-control:tasks"

	defaultRunnerPort  = 9100
	defaultNtfyURL     = "https://ntfy.sh"
	defaultPollSeconds = 5
	notifyHTTPTimeout  = 15 * time.Second
)

// openRetryWaits is the backoff between attempts to open the store; the last
// value repeats. Nothing else polls Redis.
var openRetryWaits = []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute}

// Module is the scheduled-task module: the admin API over the task store and
// the engine. It is active when REDIS_URL, NODE_CONTROL_STATE_KEY and
// TASKS_RUNNER_TOKEN are all set. Redis being down never stops node-control:
// the routes answer 503 until the store opens, which Start retries in the
// background.
type Module struct {
	env core.Env
	src headscale.NodeSource

	// Set by Register.
	on       bool
	redisURL string
	stateKey string
	token    string
	tag      string // TASKS_RUNNER_TAG
	// sessionsTag is SESSIONS_TAG; "" means no browser-session page or API.
	sessionsTag string
	port        int
	ntfyURL     string
	tn          core.Tailnet
	log         *slog.Logger

	metricsAddr     string // TASKS_METRICS_ADDR; "" means no metrics listener
	heartbeatSecret string // TASKS_HEARTBEAT_SECRET; "" means no heartbeat
	metrics         *Metrics

	// Collaborators, replaced by tests.
	pollEvery    time.Duration
	openKV       func(url, key string) (ipn.StateStore, func(), error)
	newRunner    func() Runner
	newSessions  func() SessionClient
	newNotifier  func(*Store) Notifier
	onOpenFail   func()
	now          func() time.Time
	serveMetrics func(ctx context.Context, addr string, m *Metrics) error

	cancel   context.CancelFunc
	loopDone chan struct{}
	bg       sync.WaitGroup // metrics listener and heartbeat

	// mu guards the fields below only; it is never held across a store or
	// engine call.
	mu         sync.Mutex
	store      *Store
	engine     *Engine
	closeKV    func()
	engineDone chan struct{}
}

// New returns the tasks module configured from env. src is the shared, cached
// headscale node source used to pick nodes.
func New(env core.Env, src headscale.NodeSource) core.Module {
	return &Module{env: env, src: src, openKV: openRedisKV, now: time.Now, serveMetrics: ServeMetrics}
}

func (m *Module) Name() string { return "tasks" }

// Nav lists the page in the admin top bar; an inactive module lists nothing.
func (m *Module) Nav() []core.NavItem {
	if !m.on {
		return nil
	}
	items := []core.NavItem{{Label: "Tasks", Path: "/tasks", Order: 40}}
	if m.sessionsTag != "" {
		items = append(items, core.NavItem{Label: "Browser", Path: "/sessions", Order: 45})
	}
	return items
}

// intEnv reads an integer variable with a default and an inclusive range.
func intEnv(env core.Env, key string, def, lo, hi int) (int, error) {
	v := env.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("%s must be a whole number from %d to %d", key, lo, hi)
	}
	return n, nil
}

// firstLogin returns the first non-blank item of a csv list.
func firstLogin(csv string) string {
	for _, p := range strings.Split(csv, ",") {
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	return ""
}

func (m *Module) Register(r *core.Routes) error {
	m.log = r.Log
	if m.log == nil {
		m.log = slog.Default()
	}
	var missing []string
	for _, k := range []string{"REDIS_URL", "NODE_CONTROL_STATE_KEY", "TASKS_RUNNER_TOKEN"} {
		if m.env.Getenv(k) == "" {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		m.log.Info("tasks module inactive: not set: " + strings.Join(missing, ", "))
		return nil
	}
	const enabled = "tasks module enabled (REDIS_URL, NODE_CONTROL_STATE_KEY and TASKS_RUNNER_TOKEN set)"
	users := m.env.Getenv("TASKS_ADMIN_USERS")
	allowed := authz.ParseAllowed(users)
	if !allowed.Has(firstLogin(users)) {
		return errors.New(enabled + " but TASKS_ADMIN_USERS names no login")
	}
	m.stateKey = m.env.Getenv("NODE_CONTROL_STATE_KEY")
	if err := statestore.ValidateStateKey(m.stateKey); err != nil {
		return err
	}
	port, err := intEnv(m.env, "TASKS_RUNNER_PORT", defaultRunnerPort, 1, 65535)
	if err != nil {
		return err
	}
	poll, err := intEnv(m.env, "TASKS_POLL_SECONDS", defaultPollSeconds, 1, 60)
	if err != nil {
		return err
	}
	// The pages and API act on the operator's behalf: like the policy
	// console, they need the core's Host guard against DNS rebinding.
	if hosts := m.env.List("ENROLL_ALLOWED_HOSTS"); len(hosts) == 0 || slices.Contains(hosts, "*") {
		return errors.New(enabled + " but ENROLL_ALLOWED_HOSTS is empty or contains \"*\"; " +
			"list the host names the admin listener answers to (the core's Host guard protects the tasks page and API from DNS rebinding)")
	}
	tag := strings.TrimSpace(m.env.Default("TASKS_RUNNER_TAG", DefaultRunnerTag))
	if !runnerTagRe.MatchString(tag) {
		return fmt.Errorf("TASKS_RUNNER_TAG must look like tag:task-runner (match %s)", runnerTagRe)
	}
	sessTag := strings.TrimSpace(m.env.Getenv("SESSIONS_TAG"))
	if sessTag != "" && !runnerTagRe.MatchString(sessTag) {
		return fmt.Errorf("SESSIONS_TAG must look like tag:browser-session (match %s)", runnerTagRe)
	}
	if r.Tailnet == nil {
		return errors.New(enabled + " but the core has no tailnet node to identify users")
	}
	metricsAddr, err := metricsAddrEnv(m.env)
	if err != nil {
		return err
	}
	hb := strings.TrimSpace(m.env.Getenv("TASKS_HEARTBEAT_SECRET"))
	if hb != "" && !secretNameRe.MatchString(hb) {
		return errors.New("TASKS_HEARTBEAT_SECRET must be a secret name (letters, digits, underscore)")
	}
	m.on = true
	m.redisURL = m.env.Getenv("REDIS_URL")
	m.token = m.env.Getenv("TASKS_RUNNER_TOKEN")
	m.tag = tag
	m.port = port
	m.ntfyURL = m.env.Default("TASKS_NTFY_URL", defaultNtfyURL)
	m.tn = r.Tailnet
	m.metricsAddr, m.heartbeatSecret = metricsAddr, hb
	m.metrics = NewMetrics()
	if m.pollEvery == 0 {
		m.pollEvery = time.Duration(poll) * time.Second
	}
	gate := authz.UserGate(r.Tailnet, allowed, r.Log)
	(&api{m: m}).register(r.Admin, gate)
	if sessTag != "" {
		m.sessionsTag = sessTag
		var sc SessionClient
		if m.newSessions != nil {
			sc = m.newSessions()
		} else {
			sc = NewSessionClient(m.tn, m.port, m.token)
		}
		(&sessionAPI{m: m, c: sc}).register(r.Admin, gate)
		m.log.Info("browser sessions enabled", "tag", sessTag)
	}
	return nil
}

// metricsAddrEnv reads TASKS_METRICS_ADDR: unset is "", anything else must be
// host:port with a numeric port (the host may be empty for all interfaces).
func metricsAddrEnv(env core.Env) (string, error) {
	v := strings.TrimSpace(env.Getenv("TASKS_METRICS_ADDR"))
	if v == "" {
		return "", nil
	}
	bad := errors.New("TASKS_METRICS_ADDR must be host:port, for example :9091")
	_, port, err := net.SplitHostPort(v)
	if err != nil {
		return "", bad
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return "", bad
	}
	return v, nil
}

// openRedisKV opens the encrypted Redis hash. Errors carry neither the URL
// nor the key.
func openRedisKV(url, key string) (ipn.StateStore, func(), error) {
	rs, err := statestore.NewRedis(url, redisHashKey)
	if err != nil {
		return nil, nil, err
	}
	enc, err := statestore.NewEncrypted(rs, key, redisHashKey)
	if err != nil {
		rs.Close()
		return nil, nil, err
	}
	return enc, func() { rs.Close() }, nil
}

// Start opens the store in the background and returns at once.
func (m *Module) Start(ctx context.Context) error {
	if !m.on {
		return nil
	}
	ctx, m.cancel = context.WithCancel(ctx)
	m.loopDone = make(chan struct{})
	m.log.Info("tasks: only nodes with the runner tag are candidates; tag them as an admin and let only the admin/service identity own the tag in the policy's tagOwners",
		"tag", m.tag)
	if m.metricsAddr != "" {
		m.bg.Add(1)
		go func() {
			defer m.bg.Done()
			// A listen error is logged once; the module keeps working.
			if err := m.serveMetrics(ctx, m.metricsAddr, m.metrics); err != nil && ctx.Err() == nil {
				m.log.Warn("tasks metrics listener failed", "addr", m.metricsAddr, "err", err.Error())
			}
		}()
	}
	go func() {
		defer close(m.loopDone)
		m.openLoop(ctx)
	}()
	return nil
}

// openLoop retries opening the store with backoff until it succeeds or ctx
// ends.
func (m *Module) openLoop(ctx context.Context) {
	for i := 0; ; i++ {
		err := m.open(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		wait := openRetryWaits[min(i, len(openRetryWaits)-1)]
		m.log.Warn("tasks store unavailable, the tasks API answers 503 until it opens",
			"err", scrub.Scrub(err.Error(), []string{m.redisURL, m.stateKey, m.token}), "retry_in", wait.String())
		if m.onOpenFail != nil {
			m.onOpenFail()
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// open reads the store and starts the engine.
func (m *Module) open(ctx context.Context) error {
	kv, closeKV, err := m.openKV(m.redisURL, m.stateKey)
	if err != nil {
		return err
	}
	store, err := OpenStore(kv, m.log)
	if err != nil {
		closeKV()
		return err
	}
	if ctx.Err() != nil {
		closeKV()
		return nil
	}
	var notifier Notifier
	if m.newNotifier != nil {
		notifier = m.newNotifier(store)
	} else {
		notifier = NewNotifier(store, &http.Client{Timeout: notifyHTTPTimeout}, m.ntfyURL, m.log, m.now)
	}
	if o, ok := notifier.(interface {
		SetObserver(func(task, kind string, ok bool))
	}); ok {
		o.SetObserver(m.metrics.NotifySent)
	}
	var runner Runner
	if m.newRunner != nil {
		runner = m.newRunner()
	} else {
		runner = NewRunnerClient(m.tn, m.port, m.token, nil)
	}
	eng := NewEngine(Deps{Store: store, Nodes: m.src, Runner: runner, Notifier: notifier,
		PollEvery: m.pollEvery, Log: m.log, Metrics: m.metrics, RunnerTag: m.tag})
	done := make(chan struct{})
	m.mu.Lock()
	m.store, m.engine, m.closeKV, m.engineDone = store, eng, closeKV, done
	m.mu.Unlock()
	go func() {
		defer close(done)
		eng.Run(ctx)
	}()
	if p, ok := notifier.(heartbeatPinger); ok && m.heartbeatSecret != "" {
		hb := newHeartbeat(p, m.heartbeatSecret, m.log)
		m.bg.Add(1)
		go func() {
			defer m.bg.Done()
			hb.run(ctx)
		}()
	}
	m.log.Info("tasks store open")
	return nil
}

// current returns the open store and engine, or nils while degraded.
func (m *Module) current() (*Store, *Engine) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.store, m.engine
}

func (m *Module) engineFor() *Engine {
	_, e := m.current()
	return e
}

// Stop ends the open loop and the engine, then closes Redis. The core hands
// every module one shared, short Stop context (5 s) and this module stops
// last, so each wait is bounded by ctx: the engine's own shutdown wait is
// shorter than that, which lets Redis close only once the engine has stopped
// writing. Only an expired ctx closes Redis under a running engine (its later
// writes then fail and are logged). It is safe without Start and when the
// module is inactive.
func (m *Module) Stop(ctx context.Context) error {
	if m.cancel == nil {
		return nil
	}
	m.cancel()
	loopEnded := false
	select {
	case <-m.loopDone:
		loopEnded = true
	case <-ctx.Done():
	}
	m.mu.Lock()
	done, closeKV := m.engineDone, m.closeKV
	m.closeKV = nil
	m.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
		}
	}
	// The open loop adds the heartbeat to bg; wait only once it has ended.
	if loopEnded {
		bgDone := make(chan struct{})
		go func() { m.bg.Wait(); close(bgDone) }()
		select {
		case <-bgDone:
		case <-ctx.Done():
		}
	}
	if closeKV != nil {
		closeKV()
	}
	return nil
}
