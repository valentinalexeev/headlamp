package enroll

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Tailnet grants, phase 2 (#139). A bridge in "grants" mode gives each credential only the tailnet targets that
// its device holds, directly or through its owner. The credentials with the same set of targets form a grant
// group. Each group has its own tailnet identity on the bridge (tag GroupTagPrefix+<8 hex>), so the headscale ACL,
// not only the xray rule, limits what the group reaches.

const (
	// TailnetGrants is the third tailnet_access value: per-device grants.
	TailnetGrants = "grants"
	// GroupTagPrefix starts every generated group tag; node-control changes only ACL rules with this prefix.
	GroupTagPrefix = "tag:ingress-g-"
	// MaxGroupsPerBridge is the number of grant groups one bridge can serve (one SOCKS instance each).
	MaxGroupsPerBridge = 8
	maxTargets         = 32
	maxTargetDst       = 16
	maxGrants          = 2048
	// Subject kinds of a grant.
	SubjectDevice = "device"
	SubjectOwner  = "owner"
	SubjectGroup  = "group"
)

var (
	targetNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	// aclDstRe is a headscale ACL destination: a tag, a host name, an address or a prefix, then ":" and ports.
	aclDstRe = regexp.MustCompile(`^(tag:[a-z0-9-]{1,63}|[A-Za-z0-9.-]{1,253}|[0-9a-fA-F:.]{2,45}(/[0-9]{1,3})?|\[[0-9a-fA-F:]{2,45}\](/[0-9]{1,3})?):(\*|[0-9]{1,5}(-[0-9]{1,5})?(,[0-9]{1,5}(-[0-9]{1,5})?)*)$`)
	groupIDRe = regexp.MustCompile(`^g-[0-9a-f]{8}$`)
)

// TailnetTarget is a named set of ACL destinations. Source is "config" (enroll.json, read-only) or "console".
type TailnetTarget struct {
	Name   string   `json:"name"`
	Dst    []string `json:"dst"`
	Source string   `json:"source,omitempty"`
}

// TailnetGrant gives one subject one target on one bridge.
type TailnetGrant struct {
	Bridge  string `json:"bridge"`
	Kind    string `json:"kind"`
	Subject string `json:"subject"`
	Target  string `json:"target"`
}

// TailnetDoc is the stored grant state: console targets, grants and per-bridge mode overrides.
type TailnetDoc struct {
	Targets []TailnetTarget   `json:"targets,omitempty"`
	Grants  []TailnetGrant    `json:"grants,omitempty"`
	Modes   map[string]string `json:"modes,omitempty"`
}

// ValidTailnetMode says whether v is a tailnet_access value.
func ValidTailnetMode(v string) bool { return v == TailnetOff || v == TailnetAll || v == TailnetGrants }

// ValidateTarget checks one target. The error names the field, never more than the target name.
func ValidateTarget(t TailnetTarget) error {
	if !targetNameRe.MatchString(t.Name) {
		return errors.New("target name must be lowercase letters, digits and hyphens (1-32)")
	}
	if len(t.Dst) == 0 || len(t.Dst) > maxTargetDst {
		return fmt.Errorf("target %s: dst must have 1-%d entries", t.Name, maxTargetDst)
	}
	for _, d := range t.Dst {
		if !aclDstRe.MatchString(d) {
			return fmt.Errorf("target %s: dst %q is not a headscale ACL destination (host or tag, then :ports)", t.Name, d)
		}
	}
	return nil
}

// parseTailnetTargets reads the tailnet_targets key of enroll.json.
func parseTailnetTargets(raw json.RawMessage) ([]TailnetTarget, error) {
	var ts []TailnetTarget
	if err := json.Unmarshal(raw, &ts); err != nil || ts == nil {
		return nil, errors.New("ENROLL_CONFIG: tailnet_targets must be an array of {name, dst}")
	}
	if len(ts) > maxTargets {
		return nil, fmt.Errorf("ENROLL_CONFIG: at most %d tailnet_targets", maxTargets)
	}
	seen := map[string]bool{}
	for i := range ts {
		if err := ValidateTarget(ts[i]); err != nil {
			return nil, errors.New("ENROLL_CONFIG: " + err.Error())
		}
		if seen[ts[i].Name] {
			return nil, fmt.Errorf("ENROLL_CONFIG: tailnet target %s: duplicate name", ts[i].Name)
		}
		seen[ts[i].Name] = true
		ts[i].Source = "config"
	}
	return ts, nil
}

// GroupID is the stable id of a set of target names: "g-" and 8 hex of the SHA-256 of the sorted names.
func GroupID(names []string) string {
	s := slices.Clone(names)
	slices.Sort(s)
	s = slices.Compact(s)
	sum := sha256.Sum256([]byte(strings.Join(s, "\n")))
	return "g-" + hex.EncodeToString(sum[:4])
}

// GroupTag is the tailnet tag of a group id.
func GroupTag(id string) string { return GroupTagPrefix + strings.TrimPrefix(id, "g-") }
