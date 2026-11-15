package core

import (
	"os"
	"strings"
)

// Env is an injectable, read-only view of the process environment.
type Env struct {
	lookup func(string) string
}

// OSEnv reads from the real process environment.
func OSEnv() Env { return Env{lookup: os.Getenv} }

// MapEnv reads from a fixed map (for tests).
func MapEnv(m map[string]string) Env {
	return Env{lookup: func(k string) string { return m[k] }}
}

// Getenv returns the value of key, or "" when unset.
func (e Env) Getenv(key string) string {
	if e.lookup == nil {
		return ""
	}
	return e.lookup(key)
}

// With returns an Env that answers the keys of over from over, and every other key from e.
// over is copied.
func (e Env) With(over map[string]string) Env {
	if len(over) == 0 {
		return e
	}
	m := make(map[string]string, len(over))
	for k, v := range over {
		m[k] = v
	}
	return Env{lookup: func(k string) string {
		if v, ok := m[k]; ok {
			return v
		}
		return e.Getenv(k)
	}}
}

// Default returns the value of key, or def when it is empty.
func (e Env) Default(key, def string) string {
	if v := e.Getenv(key); v != "" {
		return v
	}
	return def
}

// List splits a comma-separated variable, trimming blanks and dropping empties.
func (e Env) List(key string) []string {
	var out []string
	for _, p := range strings.Split(e.Getenv(key), ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// CompatStrict is COMPAT_STRICT (#300): "true" makes node-control refuse a component whose
// interface version is
// outside its compatibility matrix (no credential push to the agent, 409 to the funnel ingress, no
// task for the
// runner). Anything else, and unset, is warn only: the default.
func CompatStrict(env Env) bool { return env.Getenv("COMPAT_STRICT") == "true" }
