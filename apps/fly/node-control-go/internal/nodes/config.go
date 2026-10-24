// Package nodes holds the node configuration (nodes.json overrides), the
// tag-based discovery of exit nodes from headscale, and the browser-facing view.
package nodes

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Providers lists the supported cloud providers, in tag-precedence order.
var Providers = []string{"yandex", "oci", "selectel", "fly"}

// IDPattern is the shape every node id must have (it ends up in API paths).
const IDPattern = `^[a-z0-9][a-z0-9_-]*$`

var idRE = regexp.MustCompile(IDPattern)

// uiKeys are the only per-node keys ever served to the browser.
var uiKeys = []string{"startStatus", "stopStatus", "detailFields", "notConfiguredDetailFields", "prettyNotConfigured"}

// ProviderDefault is the label and status vocabulary of one provider. Status
// strings differ per provider and are compared uppercased by the UI.
type ProviderDefault map[string]any

// ProviderDefaults mirrors PROVIDER_DEFAULTS of the Python app.
var ProviderDefaults = map[string]ProviderDefault{
	"yandex":   {"label": "Yandex Compute", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": []any{"detail"}},
	"oci":      {"label": "OCI Compute", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": []any{"detail", "shape"}},
	"selectel": {"label": "Selectel Cloud", "startStatus": "RUNNING", "stopStatus": "STOPPED", "detailFields": []any{"detail"}},
	"fly": {
		"label": "Fly Machine", "startStatus": "STARTED", "stopStatus": "STOPPED",
		"detailFields":              []any{map[string]any{"field": "machine_id", "template": "machine {value}"}},
		"notConfiguredDetailFields": []any{"detail"}, "prettyNotConfigured": true,
	},
}

const (
	defaultTitle    = "node-control"
	defaultSubtitle = "on-demand start/stop for your nodes"
	statusOnlyLabel = "Tailnet only"
)

// Entry is one explicit nodes.<id> record. An entry without a provider is a
// pure override (label, hidden, tailnetName) for a discovered node.
type Entry struct {
	Provider    string
	TailnetName string
	Label       string
	Hidden      bool
	Block       map[string]any // the inner connection block, e.g. the value of "yandex": {...}
	raw         map[string]any
}

// Config is the parsed nodes.json.
type Config struct {
	Title, Subtitle string
	Nodes           map[string]Entry
	TailnetTrack    []string
	Aliases         map[string]string // old id -> node name
}

// LoadConfig reads path. A missing file is allowed and yields an empty config;
// a present but malformed one is an error.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return &Config{Nodes: map[string]Entry{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("cannot read nodes config %s: %w", path, err)
	}
	cfg, err := ParseConfig(b)
	if err != nil {
		return nil, fmt.Errorf("nodes config %s: %w", path, err)
	}
	return cfg, nil
}

// ParseConfig parses and validates nodes.json content.
func ParseConfig(b []byte) (*Config, error) {
	var top any
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("not valid JSON: %w", err)
	}
	m, ok := top.(map[string]any)
	if !ok {
		return nil, errors.New(`must be an object with a "nodes" object`)
	}
	cfg := &Config{Nodes: map[string]Entry{}}
	var err error
	if cfg.Title, err = optString(m, "title"); err != nil {
		return nil, err
	}
	if cfg.Subtitle, err = optString(m, "subtitle"); err != nil {
		return nil, err
	}
	if v, present := m["nodes"]; present {
		nm, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New(`"nodes" must be an object`)
		}
		for id, nv := range nm {
			e, err := parseEntry(id, nv)
			if err != nil {
				return nil, err
			}
			cfg.Nodes[id] = e
		}
	}
	if v, present := m["tailnetTrack"]; present {
		l, ok := v.([]any)
		if !ok {
			return nil, errors.New(`"tailnetTrack" must be a list of names`)
		}
		for _, x := range l {
			s, ok := x.(string)
			if !ok {
				return nil, errors.New(`"tailnetTrack" must be a list of names`)
			}
			cfg.TailnetTrack = append(cfg.TailnetTrack, s)
		}
	}
	if v, present := m["aliases"]; present {
		am, ok := v.(map[string]any)
		if !ok {
			return nil, errors.New(`"aliases" must be an object of old id to node name`)
		}
		cfg.Aliases = map[string]string{}
		for old, nv := range am {
			if !idRE.MatchString(old) {
				return nil, fmt.Errorf("alias id %q must match %s", old, IDPattern)
			}
			s, ok := nv.(string)
			if !ok || s == "" {
				return nil, fmt.Errorf("alias %q: node name must be a non-empty string", old)
			}
			cfg.Aliases[old] = s
		}
	}
	return cfg, nil
}

func parseEntry(id string, v any) (Entry, error) {
	if !idRE.MatchString(id) {
		return Entry{}, fmt.Errorf("node id %q must match %s", id, IDPattern)
	}
	raw, ok := v.(map[string]any)
	if !ok {
		return Entry{}, fmt.Errorf("node %q: must be an object", id)
	}
	e := Entry{raw: raw}
	if p, present := raw["provider"]; present {
		s, _ := p.(string)
		if !isProvider(s) {
			return Entry{}, fmt.Errorf("node %q: provider must be one of %s", id, strings.Join(Providers, ", "))
		}
		e.Provider = s
		if bv, present := raw[s]; present && bv != nil {
			bm, ok := bv.(map[string]any)
			if !ok {
				return Entry{}, fmt.Errorf("node %q: %q must be an object", id, s)
			}
			e.Block = bm
		}
	}
	var err error
	if e.TailnetName, err = optString(raw, "tailnetName"); err != nil {
		return Entry{}, fmt.Errorf("node %q: %w", id, err)
	}
	if e.Label, err = optString(raw, "label"); err != nil {
		return Entry{}, fmt.Errorf("node %q: %w", id, err)
	}
	if h, present := raw["hidden"]; present {
		b, ok := h.(bool)
		if !ok {
			return Entry{}, fmt.Errorf("node %q: hidden must be true or false", id)
		}
		e.Hidden = b
	}
	return e, nil
}

func optString(m map[string]any, key string) (string, error) {
	v, present := m[key]
	if !present {
		return "", nil
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%q must be a string", key)
	}
	return s, nil
}

func isProvider(s string) bool {
	for _, p := range Providers {
		if p == s {
			return true
		}
	}
	return false
}
