package controlsync

import (
	"context"
	"math/rand"
	"strings"
	"time"
)

const (
	fetchTimeout  = 30 * time.Second
	reportTimeout = 15 * time.Second
	maxBackoff    = 5 * time.Minute
	maxErrors     = 10
)

// Run keeps the configuration in sync until ctx is done. It applies the cached configuration before the
// first fetch, then every interval (with +/-20 % jitter, and exponential backoff while the source fails)
// pulls the configuration, applies a new version, caches it, and reports state when rep is non-nil.
//
// Fail static: while the source is unreachable, slow or answering garbage, or a new version is rejected,
// the last good configuration stays in force, including after a restart (the cache). The state store is
// written only when the version changes.
func Run(ctx context.Context, src Source, rep Reporter, app *Applier, cache *Cache, state func() State, interval time.Duration, log func(string, ...any)) {
	r := &runner{
		src: src, rep: rep, app: app, cache: cache, state: state, interval: interval, log: log,
		now: time.Now, rnd: rand.Float64,
		wait: func(ctx context.Context, d time.Duration) bool {
			t := time.NewTimer(d)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return false
			case <-t.C:
				return true
			}
		},
	}
	r.run(ctx)
}

type runner struct {
	src      Source
	rep      Reporter
	app      *Applier
	cache    *Cache
	state    func() State
	interval time.Duration
	log      func(string, ...any)

	now  func() time.Time
	rnd  func() float64
	wait func(ctx context.Context, d time.Duration) bool

	applied    string
	lastOK     time.Time
	errs       []string
	lastLogged string
	lastRepErr string
}

func (r *runner) logf(format string, args ...any) {
	if r.log != nil {
		r.log(format, args...)
	}
}

func (r *runner) run(ctx context.Context) {
	r.lastOK = r.now()
	r.loadCache()
	failures := 0
	for {
		if r.cycle(ctx) {
			failures = 0
		} else {
			failures++
		}
		if !r.wait(ctx, r.delay(failures)) {
			return
		}
	}
}

// loadCache applies the cached configuration, if any, so the proxy serves before node-control answers. A
// corrupt cache is logged and ignored; it is replaced when a good configuration arrives, never before.
func (r *runner) loadCache() {
	cfg, ok, err := r.cache.Load()
	if err != nil {
		r.addErr("cached configuration: " + err.Error())
		r.logf("controlsync: %v; starting without a configuration", err)
		return
	}
	if !ok {
		return
	}
	if _, err := r.app.Apply(cfg); err != nil {
		r.addErr(err.Error())
		r.logf("controlsync: the cached configuration was not applied: %v", err)
		return
	}
	r.applied = cfg.Version
	r.logf("controlsync: applied cached configuration version %s", cfg.Version)
}

func (r *runner) delay(failures int) time.Duration {
	d := r.interval
	for i := 0; i < failures && d < maxBackoff; i++ {
		d *= 2
	}
	if d > maxBackoff {
		d = maxBackoff
	}
	return time.Duration(float64(d) * (0.8 + 0.4*r.rnd()))
}

// cycle runs one fetch/apply/report round and reports whether the source and the configuration were fine.
func (r *runner) cycle(ctx context.Context) bool {
	ok := true
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	cfg, changed, err := r.src.Fetch(fctx, r.applied)
	cancel()
	if err != nil {
		ok = false
		r.addErr(err.Error())
		r.logOnce("controlsync: " + err.Error())
	} else {
		r.lastOK = r.now()
		r.errs = nil
		r.lastLogged = ""
		if changed && cfg.Version != r.applied {
			if _, aerr := r.app.Apply(cfg); aerr != nil {
				ok = false
				r.addErr(aerr.Error())
				r.logOnce("controlsync: " + aerr.Error())
			} else {
				r.applied = cfg.Version
				r.logf("controlsync: applied configuration version %s", cfg.Version)
				if serr := r.cache.Save(cfg); serr != nil {
					r.addErr("could not cache the configuration")
					r.logf("controlsync: could not cache the configuration: %v", serr)
				}
			}
		}
	}
	r.report(ctx)
	return ok
}

func (r *runner) report(ctx context.Context) {
	if r.rep == nil || r.state == nil {
		return
	}
	s := r.state()
	s.AppliedVersion = r.applied
	s.SentAt = r.now()
	s.ConfigAge = s.SentAt.Sub(r.lastOK)
	s.Errors = append([]string(nil), r.errs...)
	rctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	if err := r.rep.Report(rctx, s); err != nil {
		if msg := err.Error(); msg != r.lastRepErr {
			r.lastRepErr = msg
			r.logf("controlsync: %v", err)
		}
		return
	}
	r.lastRepErr = ""
}

func (r *runner) addErr(msg string) {
	if len(msg) > 300 {
		msg = msg[:300]
	}
	msg = strings.ReplaceAll(msg, "\n", " ")
	for _, e := range r.errs {
		if e == msg {
			return
		}
	}
	r.errs = append(r.errs, msg)
	if len(r.errs) > maxErrors {
		r.errs = r.errs[len(r.errs)-maxErrors:]
	}
}

// logOnce logs a failure the first time it appears, not on every retry.
func (r *runner) logOnce(msg string) {
	if msg == r.lastLogged {
		return
	}
	r.lastLogged = msg
	r.logf("%s", msg)
}
