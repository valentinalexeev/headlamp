package enroll

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Bridge selection overrides (#314). An admin can change the Phase 1 options (#313) in the console:
// the urltest tuning, the group and the order of each bridge, and a "drain" switch per bridge. The
// console never changes the host, keys, short id, SNI, port or tokens of a bridge.
//
// Precedence of each value: the console override, then enroll.json, then the built-in default. An
// empty override changes nothing, so the profiles stay byte for byte the same.

// SelectionOverride is the stored console override. Every field is optional.
type SelectionOverride struct {
	Tolerance *int   `json:"tolerance,omitempty"`
	Interval  string `json:"interval,omitempty"`
	URL       string `json:"url,omitempty"`
	// Strategy is "round-robin", "consistent-hashing", or "url-test" (no strategy, also when
	// enroll.json sets one).
	Strategy string `json:"strategy,omitempty"`
	// Groups sets the group of a bridge by name. "" removes the group of enroll.json.
	Groups map[string]string `json:"groups,omitempty"`
	// Order is the order of the bridges in the profile. Bridges not in it follow, in file order.
	Order []string `json:"order,omitempty"`
	// Drain names bridges that are left out of new profiles. Their credentials stay on the bridge.
	Drain []string `json:"drain,omitempty"`
}

// strategyURLTest is the override value that removes a strategy of enroll.json.
const strategyURLTest = "url-test"

// Empty reports whether the override changes nothing.
func (o SelectionOverride) Empty() bool {
	return o.Tolerance == nil && o.Interval == "" && o.URL == "" && o.Strategy == "" &&
		len(o.Groups) == 0 && len(o.Order) == 0 && len(o.Drain) == 0
}

// SelectionDoc is the stored state: the current override and the one before it (one-step revert).
type SelectionDoc struct {
	Current  SelectionOverride  `json:"current"`
	Previous *SelectionOverride `json:"previous,omitempty"`
	// By and At name who saved the current override and when (unix seconds).
	By string `json:"by,omitempty"`
	At int64  `json:"at,omitempty"`
}

// tuning gives the tuning part as a Selection. The bounds are the ones of enroll.json (#313).
func (o SelectionOverride) tuning() (Selection, error) {
	raw := map[string]any{}
	if o.Tolerance != nil {
		raw["tolerance"] = *o.Tolerance
	}
	if o.Interval != "" {
		raw["interval"] = o.Interval
	}
	if o.URL != "" {
		raw["url"] = o.URL
	}
	if o.Strategy != "" && o.Strategy != strategyURLTest {
		raw["strategy"] = o.Strategy
	}
	b, _ := json.Marshal(raw)
	s, err := parseSelection(b)
	if err != nil {
		return Selection{}, errors.New(strings.TrimPrefix(err.Error(), "ENROLL_CONFIG: "))
	}
	return s, nil
}

// Validate checks the override against the bridges of cfg. The errors name a field and a bridge
// only.
func (o SelectionOverride) Validate(cfg IngressConfig) error {
	if _, err := o.tuning(); err != nil {
		return err
	}
	if o.Strategy != "" && o.Strategy != strategyURLTest && o.Strategy != strategyRoundRobin &&
		o.Strategy != strategyConsistentHash {
		return fmt.Errorf("urltest.strategy must be %q, %q or %q",
			strategyURLTest, strategyRoundRobin, strategyConsistentHash)
	}
	known := map[string]bool{}
	for _, in := range cfg.Ingresses {
		known[in.Name] = true
	}
	for name, g := range o.Groups {
		if !known[name] {
			return fmt.Errorf("groups: %q is not a configured bridge", plainName(name))
		}
		if g != "" && !ingressNameRe.MatchString(g) {
			return fmt.Errorf("groups: the group of %s must match %s", name, ingressNamePatt)
		}
	}
	seen := map[string]bool{}
	for _, name := range o.Order {
		if !known[name] {
			return fmt.Errorf("order: %q is not a configured bridge", plainName(name))
		}
		if seen[name] {
			return fmt.Errorf("order: %s is in the list two times", name)
		}
		seen[name] = true
	}
	drained := map[string]bool{}
	for _, name := range o.Drain {
		if !known[name] {
			return fmt.Errorf("drain: %q is not a configured bridge", plainName(name))
		}
		drained[name] = true
	}
	if len(cfg.Ingresses) > 0 && len(drained) >= len(cfg.Ingresses) {
		return errors.New("drain: at least one bridge must stay in the profile")
	}
	return nil
}

// plainName shortens a name from a request for an error message.
func plainName(s string) string {
	if !ingressNameRe.MatchString(s) {
		return "?"
	}
	return s
}

// ApplySelection returns cfg with the override applied, for a device profile. Bridges in the
// override that cfg does not have are ignored (a bridge can go away after the save). An empty
// override returns cfg as it is.
func ApplySelection(cfg IngressConfig, o SelectionOverride) IngressConfig {
	if o.Empty() {
		return cfg
	}
	if o.Tolerance != nil {
		t := *o.Tolerance
		cfg.Selection.Tolerance = &t
	}
	if o.Interval != "" {
		if d, err := time.ParseDuration(o.Interval); err == nil {
			cfg.Selection.Interval = d
		}
	}
	if o.URL != "" {
		cfg.Selection.URL = o.URL
	}
	switch o.Strategy {
	case "":
	case strategyURLTest:
		cfg.Selection.Strategy = ""
	default:
		cfg.Selection.Strategy = o.Strategy
	}
	ins := slices.Clone(cfg.Ingresses)
	for i := range ins {
		if g, ok := o.Groups[ins[i].Name]; ok {
			ins[i].Group = g
		}
	}
	if len(o.Order) > 0 {
		pos := map[string]int{}
		for i, n := range o.Order {
			pos[n] = i
		}
		slices.SortStableFunc(ins, func(a, b Ingress) int {
			pa, oka := pos[a.Name]
			pb, okb := pos[b.Name]
			switch {
			case oka && okb:
				return pa - pb
			case oka:
				return -1
			case okb:
				return 1
			}
			return 0
		})
	}
	if len(o.Drain) > 0 {
		kept := ins[:0:0]
		for _, in := range ins {
			if !slices.Contains(o.Drain, in.Name) {
				kept = append(kept, in)
			}
		}
		// never an empty profile, also when the bridges changed after the save
		if len(kept) > 0 {
			ins = kept
		}
	}
	cfg.Ingresses = ins
	return cfg
}

// Value sources shown in the console.
const (
	SourceUI      = "ui"
	SourceFile    = "file"
	SourceDefault = "default"
)

// SelectionValue is one effective value and where it comes from.
type SelectionValue struct {
	Value  any    `json:"value"`
	Source string `json:"source"`
}

// SelectionBridge is one bridge in the console view, in profile order.
type SelectionBridge struct {
	Name        string `json:"name"`
	Group       string `json:"group"`
	GroupSource string `json:"group_source"`
	FileGroup   string `json:"file_group"`
	Drained     bool   `json:"drained"`
}

// SelectionEffective is the effective tuning and bridges with the source of each value.
type SelectionEffective struct {
	Tolerance   SelectionValue    `json:"tolerance"`
	Interval    SelectionValue    `json:"interval"`
	URL         SelectionValue    `json:"url"`
	Strategy    SelectionValue    `json:"strategy"`
	OrderSource string            `json:"order_source"`
	Bridges     []SelectionBridge `json:"bridges"`
}

// Effective resolves each value of the override over cfg (the values of enroll.json).
func Effective(cfg IngressConfig, o SelectionOverride) SelectionEffective {
	file := cfg.Selection
	var e SelectionEffective
	switch {
	case o.Tolerance != nil:
		e.Tolerance = SelectionValue{*o.Tolerance, SourceUI}
	case file.Tolerance != nil:
		e.Tolerance = SelectionValue{*file.Tolerance, SourceFile}
	default:
		e.Tolerance = SelectionValue{probeTolerance, SourceDefault}
	}
	switch {
	case o.Interval != "":
		e.Interval = SelectionValue{o.Interval, SourceUI}
	case file.Interval > 0:
		e.Interval = SelectionValue{durationText(file.Interval), SourceFile}
	default:
		e.Interval = SelectionValue{probeInterval, SourceDefault}
	}
	switch {
	case o.URL != "":
		e.URL = SelectionValue{o.URL, SourceUI}
	case file.URL != "":
		e.URL = SelectionValue{file.URL, SourceFile}
	default:
		e.URL = SelectionValue{probeURL, SourceDefault}
	}
	switch {
	case o.Strategy != "":
		e.Strategy = SelectionValue{o.Strategy, SourceUI}
	case file.Strategy != "":
		e.Strategy = SelectionValue{file.Strategy, SourceFile}
	default:
		e.Strategy = SelectionValue{strategyURLTest, SourceDefault}
	}
	e.OrderSource = SourceFile
	if len(o.Order) > 0 {
		e.OrderSource = SourceUI
	}
	fileGroup := map[string]string{}
	for _, in := range cfg.Ingresses {
		fileGroup[in.Name] = in.Group
	}
	// the order and groups of the profile, without the drain (a drained bridge stays in the list)
	view := ApplySelection(cfg, SelectionOverride{Groups: o.Groups, Order: o.Order})
	for _, in := range view.Ingresses {
		b := SelectionBridge{Name: in.Name, Group: in.Group, FileGroup: fileGroup[in.Name],
			Drained: slices.Contains(o.Drain, in.Name)}
		_, ui := o.Groups[in.Name]
		switch {
		case ui:
			b.GroupSource = SourceUI
		case in.Group != "":
			b.GroupSource = SourceFile
		default:
			b.GroupSource = SourceDefault
		}
		e.Bridges = append(e.Bridges, b)
	}
	if e.Bridges == nil {
		e.Bridges = []SelectionBridge{}
	}
	return e
}

// SelectionExport is what the pull command writes back to enroll.json: the effective tuning
// that is not a built-in default, and the group and order of the bridges. Drained bridges are
// listed apart: enroll.json has no drain key.
type SelectionExport struct {
	URLTest map[string]any    `json:"urltest"`
	Order   []string          `json:"order"`
	Groups  map[string]string `json:"groups"`
	Drained []string          `json:"drained"`
}

// Export gives the values for the pull command.
func Export(cfg IngressConfig, o SelectionOverride) SelectionExport {
	e := Effective(cfg, o)
	ex := SelectionExport{URLTest: map[string]any{}, Groups: map[string]string{}, Order: []string{},
		Drained: []string{}}
	for k, v := range map[string]SelectionValue{"tolerance": e.Tolerance, "interval": e.Interval,
		"url": e.URL, "strategy": e.Strategy} {
		if v.Source != SourceDefault && !(k == "strategy" && v.Value == strategyURLTest) {
			ex.URLTest[k] = v.Value
		}
	}
	for _, b := range e.Bridges {
		ex.Order = append(ex.Order, b.Name)
		ex.Groups[b.Name] = b.Group
		if b.Drained {
			ex.Drained = append(ex.Drained, b.Name)
		}
	}
	return ex
}
