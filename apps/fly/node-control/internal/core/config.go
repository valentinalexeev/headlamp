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
