package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// attentionTimeout bounds each module's answer on all three endpoints; a var so tests can shorten it.
var attentionTimeout = 15 * time.Second

const (
	maxTitleRunes   = 200
	maxDetailRunes  = 500
	maxShortRunes   = 100 // labels, names, actors, actions, ids
	maxSummaryRunes = 300
	defaultActivity = 200
	maxActivity     = 500
	// A source's search entries are capped so one cannot crowd out the others, and the whole answer so the palette
	// never has to filter an unbounded list. Both are far above what a console has.
	maxSearchPerSource = 2000
	maxSearchItems     = 5000
)

// cleanText removes control and format characters (bidi overrides, zero-width marks), repairs
// invalid UTF-8 and cuts the result to max runes, ending in an ellipsis when it cut.
func cleanText(s string, max int) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, s)
	if utf8.RuneCountInString(s) > max {
		s = string([]rune(s)[:max-1]) + "…"
	}
	return s
}

// safeLocalPath reports whether p is an absolute local path with an optional query: the path part
// passes ValidateNavItem's check, the query holds no space, quote, angle bracket, backslash, fragment
// or control character.
func safeLocalPath(p string) bool {
	base, query, hasQuery := strings.Cut(p, "?")
	if ValidateNavItem(NavItem{Label: "x", Path: base}) != nil {
		return false
	}
	if hasQuery && (strings.ContainsAny(query, " #<>\"\\") || strings.IndexFunc(query, unicode.IsControl) >= 0) {
		return false
	}
	return true
}

func levelRank(l AttentionLevel) int {
	switch l {
	case LevelTimed:
		return 0
	case LevelCritical:
		return 1
	case LevelWaiting:
		return 2
	}
	return 3
}

func cleanItem(it AttentionItem, source string) AttentionItem {
	it.ID = cleanText(it.ID, maxShortRunes)
	it.Source = cleanText(it.Source, maxShortRunes)
	if it.Source == "" {
		it.Source = source
	}
	switch it.Level {
	case LevelTimed, LevelCritical, LevelWaiting, LevelInfo:
	default:
		it.Level = LevelInfo
	}
	it.Title = cleanText(it.Title, maxTitleRunes)
	if it.ID == "" { // from the title, so that it holds from one poll to the next
		sum := sha256.Sum256([]byte(it.Title))
		it.ID = cleanText(it.Source, maxShortRunes-9) + "-" + hex.EncodeToString(sum[:4])
	}
	it.Detail = cleanText(it.Detail, maxDetailRunes)
	it.ActionLabel = cleanText(it.ActionLabel, maxShortRunes)
	if !safeLocalPath(it.ActionPath) {
		it.ActionPath, it.ActionLabel = "", ""
	}
	it.AckToken, it.Acknowledged, it.module = "", nil, "" // the core's own, whatever a source put there
	return it
}

func cleanLine(l SystemLine) SystemLine {
	l.Name = cleanText(l.Name, maxShortRunes)
	l.Summary = cleanText(l.Summary, maxSummaryRunes)
	if !safeLocalPath(l.Path) {
		l.Path = ""
	}
	switch l.State {
	case "ok", "waiting", "critical", "unknown", "off":
	default:
		l.State = "unknown"
	}
	return l
}

// callSource runs fn with a timeout and turns a panic or a source that ignores its context into an
// error. The goroutine of a source that never returns is abandoned; its result channel is buffered.
func callSource[T any](parent context.Context, fn func(ctx context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(parent, attentionTimeout)
	defer cancel()
	type res struct {
		v   T
		err error
	}
	ch := make(chan res, 1)
	go func() {
		defer func() {
			if p := recover(); p != nil {
				ch <- res{err: fmt.Errorf("source panicked: %v", p)}
			}
		}()
		v, err := fn(ctx)
		ch <- res{v, err}
	}()
	select {
	case r := <-ch:
		return r.v, r.err
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

// fanOut calls fn for every module in parallel and returns the results in module order.
func fanOut[S any, T any](ctx context.Context, srcs []S, fn func(ctx context.Context, s S) (T, error)) ([]T, []error) {
	vals := make([]T, len(srcs))
	errs := make([]error, len(srcs))
	done := make(chan struct{}, len(srcs))
	for i, s := range srcs {
		go func() {
			vals[i], errs[i] = callSource(ctx, func(c context.Context) (T, error) { return fn(c, s) })
			done <- struct{}{}
		}()
	}
	for range srcs {
		<-done
	}
	return vals, errs
}

type modOf[S any] struct {
	name string
	src  S
}

func sourcesOf[S any](mods []Module) []modOf[S] {
	var out []modOf[S]
	for _, m := range mods {
		if s, ok := m.(S); ok {
			out = append(out, modOf[S]{m.Name(), s})
		}
	}
	return out
}

// attentionView is what one caller sees: the merged items and system lines, sorted.
type attentionView struct {
	items   []AttentionItem
	systems []SystemLine
	hidden  bool
}

type attOut struct {
	items []AttentionItem
	lines []SystemLine
}

// readAttention asks every source for this caller and applies the acknowledgements, ending those that no longer hold.
func readAttention(ctx context.Context, att []modOf[AttentionSource], navPath func(string) string, acks *ackKeeper, login string) attentionView {
	// Read before the sources are asked: what they say is then never older than the acknowledgements it is compared
	// with, so one made a moment ago is not ended by an answer from before its item appeared.
	held := acks.load()
	results, errs := fanOut(ctx, att, func(c context.Context, s modOf[AttentionSource]) (attOut, error) {
		i, l, err := s.src.Attention(c, login)
		return attOut{i, l}, err
	})
	v := attentionView{items: []AttentionItem{}, systems: []SystemLine{}}
	for i, s := range att {
		switch err := errs[i]; {
		case errors.Is(err, ErrNotVisible):
			v.hidden = true
		case err != nil:
			// The error text is never shown: it may carry upstream detail.
			name := cleanText(s.name, maxShortRunes)
			path := navPath(s.name)
			it := AttentionItem{ID: "unknown-" + name, Source: s.name, Level: LevelCritical,
				Title: name + " could not be checked", Detail: "Its status is unknown until it answers again."}
			if path != "" {
				it.ActionLabel, it.ActionPath = "Open", path
			}
			v.items = append(v.items, cleanItem(it, s.name))
			v.systems = append(v.systems, cleanLine(SystemLine{Name: name, Path: path, State: "unknown", Summary: "No answer"}))
		default:
			items := make([]AttentionItem, 0, len(results[i].items))
			for _, it := range results[i].items {
				items = append(items, cleanItem(it, s.name))
			}
			lines := make([]SystemLine, 0, len(results[i].lines))
			for _, l := range results[i].lines {
				lines = append(lines, cleanLine(l))
			}
			acks.apply(s.name, items, lines, held)
			v.items = append(v.items, items...)
			v.systems = append(v.systems, lines...)
		}
	}
	sort.SliceStable(v.items, func(a, b int) bool {
		if ra, rb := levelRank(v.items[a].Level), levelRank(v.items[b].Level); ra != rb {
			return ra < rb
		}
		sa, sb := v.items[a].Since, v.items[b].Since
		if sa.IsZero() != sb.IsZero() {
			return !sa.IsZero()
		}
		return sa.Before(sb)
	})
	return v
}

// registerAttention serves GET /_attention.json, /_activity.json and /_search.json on mux, and the acknowledge routes
// when some module offers an AckStore. Sources are the modules that implement the matching interface. nav gives the
// page of an unreachable system.
func registerAttention(mux *http.ServeMux, mods []Module, nav []NavItem, tn Tailnet) {
	att := sourcesOf[AttentionSource](mods)
	act := sourcesOf[ActivitySource](mods)
	srch := sourcesOf[SearchSource](mods)

	serve := func(w http.ResponseWriter, r *http.Request, body func(ctx context.Context, login string) any) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		var login string
		var tags []string
		var err error
		if tn != nil {
			login, tags, err = tn.WhoIs(r.Context(), r.RemoteAddr)
		}
		if tn == nil || err != nil || len(tags) > 0 {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		out, err := json.Marshal(body(r.Context(), login))
		if err != nil { // a time out of range, say: an empty answer would read as nothing to report
			slog.Error("console data could not be encoded", "path", r.URL.Path, "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		out = append(out, '\n')
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(out)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(out)
		}
	}

	// asked is who to ask for the activity of the source named only: the module whose page has that label. A name
	// no page owns could be any module's, so all are asked.
	asked := func(only string) []modOf[ActivitySource] {
		if only == "" {
			return act
		}
		var out []modOf[ActivitySource]
		for _, it := range nav {
			if it.Label != only {
				continue
			}
			for _, s := range act {
				if s.name == it.Source {
					out = append(out, s)
				}
			}
		}
		if out == nil {
			return act
		}
		return out
	}

	navPath := func(source string) string {
		for _, it := range nav {
			if it.Source == source {
				return it.Path
			}
		}
		return ""
	}

	acks := newAckKeeper(mods)
	read := func(ctx context.Context, login string) attentionView {
		return readAttention(ctx, att, navPath, acks, login)
	}
	mux.HandleFunc("GET /_attention.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(ctx context.Context, login string) any {
			v := read(ctx, login)
			for i := range v.items {
				if a := v.items[i].Acknowledged; a != nil {
					shown := *a
					shown.By = cleanText(shown.By, maxShortRunes)
					v.items[i].Acknowledged = &shown
				}
			}
			return map[string]any{"items": v.items, "systems": v.systems, "hidden_systems": v.hidden,
				"updated_at": time.Now().UTC().Format(time.RFC3339)}
		})
	})
	if acks != nil {
		registerAckRoutes(mux, tn, acks, read)
	}

	mux.HandleFunc("GET /_activity.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(ctx context.Context, login string) any {
			limit := defaultActivity
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					limit = min(n, maxActivity)
				}
			}
			only := r.URL.Query().Get("source")
			from := asked(only)
			results, errs := fanOut(ctx, from, func(c context.Context, s modOf[ActivitySource]) ([]ActivityEntry, error) {
				return s.src.Activity(c, login, limit)
			})
			entries := []ActivityEntry{}
			partial, hidden := false, false
			for i, s := range from {
				switch err := errs[i]; {
				case errors.Is(err, ErrNotVisible):
					hidden = true
				case err != nil:
					partial = true
				default:
					for _, e := range results[i] {
						e.Actor = cleanText(e.Actor, maxShortRunes)
						e.Source = cleanText(e.Source, maxShortRunes)
						if e.Source == "" {
							e.Source = s.name
						}
						e.Action = cleanText(e.Action, maxShortRunes)
						e.Detail = cleanText(e.Detail, maxDetailRunes)
						if only == "" || e.Source == only {
							entries = append(entries, e)
						}
					}
				}
			}
			if acks != nil {
				got, gotPartial, gotHidden := acks.activity(ctx, att, nav, login, limit, only)
				entries = append(entries, got...)
				partial, hidden = partial || gotPartial, hidden || gotHidden
			}
			sort.SliceStable(entries, func(a, b int) bool { return entries[a].At.After(entries[b].At) })
			if len(entries) > limit {
				entries = entries[:limit]
			}
			return map[string]any{"entries": entries, "partial": partial, "hidden_systems": hidden}
		})
	})

	mux.HandleFunc("GET /_search.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(ctx context.Context, login string) any {
			results, errs := fanOut(ctx, srch, func(c context.Context, s modOf[SearchSource]) ([]SearchItem, error) {
				return s.src.Search(c, login)
			})
			items := []SearchItem{}
			for i, s := range srch {
				if errs[i] != nil {
					continue // a failing or hidden source contributes nothing, not even a placeholder
				}
				kept := 0
				for _, it := range results[i] {
					if kept == maxSearchPerSource || len(items) == maxSearchItems {
						slog.Warn("console search cut short", "source", s.name, "offered", len(results[i]), "kept", kept)
						break
					}
					it.Label = cleanText(it.Label, maxShortRunes)
					it.Kind = cleanText(it.Kind, maxShortRunes)
					if it.Label == "" || !safeLocalPath(it.Path) {
						continue
					}
					items = append(items, it)
					kept++
				}
			}
			return map[string]any{"items": items}
		})
	})
}
