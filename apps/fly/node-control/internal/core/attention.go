package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	it.Detail = cleanText(it.Detail, maxDetailRunes)
	it.ActionLabel = cleanText(it.ActionLabel, maxShortRunes)
	if !safeLocalPath(it.ActionPath) {
		it.ActionPath, it.ActionLabel = "", ""
	}
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

// registerAttention serves GET /_attention.json, /_activity.json and /_search.json on mux. Sources
// are the modules that implement the matching interface. nav gives the page of an unreachable system.
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
		out, _ := json.Marshal(body(r.Context(), login))
		out = append(out, '\n')
		h.Set("Content-Type", "application/json")
		h.Set("Content-Length", strconv.Itoa(len(out)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(out)
		}
	}

	navPath := func(source string) string {
		for _, it := range nav {
			if it.Source == source {
				return it.Path
			}
		}
		return ""
	}

	type attOut struct {
		items []AttentionItem
		lines []SystemLine
	}
	mux.HandleFunc("GET /_attention.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(ctx context.Context, login string) any {
			results, errs := fanOut(ctx, att, func(c context.Context, s modOf[AttentionSource]) (attOut, error) {
				i, l, err := s.src.Attention(c, login)
				return attOut{i, l}, err
			})
			items, systems := []AttentionItem{}, []SystemLine{}
			hidden := false
			for i, s := range att {
				switch err := errs[i]; {
				case errors.Is(err, ErrNotVisible):
					hidden = true
				case err != nil:
					// The error text is never shown: it may carry upstream detail.
					name := cleanText(s.name, maxShortRunes)
					path := navPath(s.name)
					it := AttentionItem{ID: "unknown-" + name, Source: s.name, Level: LevelCritical,
						Title: name + " could not be checked", Detail: "Its status is unknown until it answers again."}
					if path != "" {
						it.ActionLabel, it.ActionPath = "Open", path
					}
					items = append(items, cleanItem(it, s.name))
					systems = append(systems, cleanLine(SystemLine{Name: name, Path: path, State: "unknown", Summary: "No answer"}))
				default:
					for _, it := range results[i].items {
						items = append(items, cleanItem(it, s.name))
					}
					for _, l := range results[i].lines {
						systems = append(systems, cleanLine(l))
					}
				}
			}
			sort.SliceStable(items, func(a, b int) bool {
				if ra, rb := levelRank(items[a].Level), levelRank(items[b].Level); ra != rb {
					return ra < rb
				}
				sa, sb := items[a].Since, items[b].Since
				if sa.IsZero() != sb.IsZero() {
					return !sa.IsZero()
				}
				return sa.Before(sb)
			})
			return map[string]any{"items": items, "systems": systems, "hidden_systems": hidden,
				"updated_at": time.Now().UTC().Format(time.RFC3339)}
		})
	})

	mux.HandleFunc("GET /_activity.json", func(w http.ResponseWriter, r *http.Request) {
		serve(w, r, func(ctx context.Context, login string) any {
			limit := defaultActivity
			if v := r.URL.Query().Get("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 {
					limit = min(n, maxActivity)
				}
			}
			only := r.URL.Query().Get("source")
			results, errs := fanOut(ctx, act, func(c context.Context, s modOf[ActivitySource]) ([]ActivityEntry, error) {
				return s.src.Activity(c, login, limit)
			})
			entries := []ActivityEntry{}
			partial, hidden := false, false
			for i, s := range act {
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
			for i := range srch {
				if errs[i] != nil {
					continue // a failing or hidden source contributes nothing, not even a placeholder
				}
				for _, it := range results[i] {
					it.Label = cleanText(it.Label, maxShortRunes)
					it.Kind = cleanText(it.Kind, maxShortRunes)
					if it.Label == "" || !safeLocalPath(it.Path) {
						continue
					}
					items = append(items, it)
				}
			}
			return map[string]any{"items": items}
		})
	})
}
