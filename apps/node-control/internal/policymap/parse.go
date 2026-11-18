package policymap

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/tailscale/hujson"
)

// knownKeys are the top-level keys of a headscale or Tailscale policy. Only groups, tagOwners,
// hosts, acls, grants, ssh and autoApprovers change the map; the others need no warning.
var knownKeys = map[string]bool{
	"groups": true, "tagOwners": true, "hosts": true, "acls": true, "grants": true, "ssh": true,
	"autoApprovers": true, "tests": true, "sshTests": true, "postures": true, "nodeAttrs": true,
	"randomizeClientPort": true, "ipsets": true, "derpMap": true, "disableIPv4": true,
	"OneCGNATRoute": true,
}

type aclRule struct {
	Action     string          `json:"action"`
	Src        []string        `json:"src"`
	Dst        []string        `json:"dst"`
	Proto      json.RawMessage `json:"proto"`
	SrcPosture []string        `json:"srcPosture"`
	ref        Ref
}

type grantRule struct {
	Src        []string                   `json:"src"`
	Dst        []string                   `json:"dst"`
	IP         []string                   `json:"ip"`
	Via        []string                   `json:"via"`
	SrcPosture []string                   `json:"srcPosture"`
	App        map[string]json.RawMessage `json:"app"`
	ref        Ref
}

type sshRule struct {
	Action string   `json:"action"`
	Src    []string `json:"src"`
	Dst    []string `json:"dst"`
	Users  []string `json:"users"`
	ref    Ref
}

type approvers struct {
	Routes   map[string][]string `json:"routes"`
	ExitNode []string            `json:"exitNode"`
}

// document is the part of a policy that the map reads.
type document struct {
	groups    map[string][]string
	tagOwners map[string][]string
	hosts     map[string]string
	acls      []aclRule
	grants    []grantRule
	ssh       []sshRule
	approvers approvers
	warnings  []string
}

// parseDocument reads policy text. Comments and trailing commas are accepted. The rule references
// get the line of the rule in text.
func parseDocument(text string) (document, error) {
	var doc document
	if strings.TrimSpace(text) == "" {
		return doc, nil
	}
	root, err := hujson.Parse([]byte(text))
	if err != nil {
		return doc, ErrSyntax
	}
	obj, ok := root.Value.(*hujson.Object)
	if !ok {
		return doc, ErrSyntax
	}
	lines := newLineIndex(text)
	for i := range obj.Members {
		member := &obj.Members[i]
		var name string
		if err := json.Unmarshal(standard(&member.Name), &name); err != nil {
			return doc, ErrSyntax
		}
		doc.readMember(name, &member.Value, lines)
	}
	return doc, nil
}

// readMember decodes one top-level member into doc. A member of the wrong shape becomes a warning.
func (doc *document) readMember(name string, value *hujson.Value, lines lineIndex) {
	var err error
	switch name {
	case "groups":
		err = json.Unmarshal(standard(value), &doc.groups)
	case "tagOwners":
		err = json.Unmarshal(standard(value), &doc.tagOwners)
	case "hosts":
		err = json.Unmarshal(standard(value), &doc.hosts)
	case "autoApprovers":
		err = json.Unmarshal(standard(value), &doc.approvers)
	case SectionACL:
		doc.acls = readRules(doc, name, value, lines, func(r *aclRule, ref Ref) { r.ref = ref })
	case SectionGrant:
		doc.grants = readRules(doc, name, value, lines, func(r *grantRule, ref Ref) { r.ref = ref })
	case SectionSSH:
		doc.ssh = readRules(doc, name, value, lines, func(r *sshRule, ref Ref) { r.ref = ref })
	default:
		if !knownKeys[name] {
			doc.warnings = append(doc.warnings,
				fmt.Sprintf("the key %q is not known; the map ignores it", name))
		}
	}
	if err != nil {
		doc.warnings = append(doc.warnings, fmt.Sprintf("cannot read %q; the map ignores it", name))
	}
}

// readRules decodes the elements of a rule section. An element that cannot be read becomes a
// warning.
func readRules[R any](doc *document, section string, value *hujson.Value, lines lineIndex,
	setRef func(*R, Ref)) []R {
	arr, ok := value.Value.(*hujson.Array)
	if !ok {
		doc.warnings = append(doc.warnings,
			fmt.Sprintf("%q is not a list; the map ignores it", section))
		return nil
	}
	rules := make([]R, 0, len(arr.Elements))
	for i := range arr.Elements {
		element := &arr.Elements[i]
		ref := Ref{Section: section, Index: i, Line: lines.line(element.StartOffset)}
		var rule R
		if err := json.Unmarshal(standard(element), &rule); err != nil {
			doc.warnings = append(doc.warnings, fmt.Sprintf(
				"%s[%d] (line %d): cannot read the rule; the map ignores it", section, i, ref.Line))
			continue
		}
		setRef(&rule, ref)
		rules = append(rules, rule)
	}
	return rules
}

// standard returns value as standard JSON. It changes value: comments and trailing commas become
// spaces, and the offsets stay as they were.
func standard(value *hujson.Value) []byte {
	value.Standardize()
	return value.Pack()
}

// lineIndex maps a byte offset of the text to its line, from 1.
type lineIndex []int

func newLineIndex(text string) lineIndex {
	starts := lineIndex{0}
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func (starts lineIndex) line(offset int) int {
	return sort.Search(len(starts), func(i int) bool { return starts[i] > offset })
}
