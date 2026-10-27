package funnel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ErrNotPlainJSON is returned when the live policy has comments or trailing commas (headscale accepts
// them) or is not JSON at all. The composer refuses rather than drop content it cannot round-trip.
var ErrNotPlainJSON = errors.New("the policy is not plain JSON (comments or trailing commas?); edit it in the policy console instead")

// ErrPolicyShape is returned when the policy parses but "tagOwners" or "acls" has an unexpected type.
var ErrPolicyShape = errors.New("the policy has an unexpected shape (tagOwners must be an object and acls an array)")

// FormatPorts renders ranges as a sorted, merged ACL port list: [{8080,8080},{9000,9010}] -> "8080,9000-9010".
func FormatPorts(rs []Range) string {
	if len(rs) == 0 {
		return ""
	}
	s := append([]Range(nil), rs...)
	sort.Slice(s, func(i, j int) bool { return s[i].Lo < s[j].Lo })
	merged := []Range{s[0]}
	for _, r := range s[1:] {
		last := &merged[len(merged)-1]
		if r.Lo <= last.Hi+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		merged = append(merged, r)
	}
	parts := make([]string, len(merged))
	for i, r := range merged {
		if r.Lo == r.Hi {
			parts[i] = strconv.Itoa(r.Lo)
		} else {
			parts[i] = fmt.Sprintf("%d-%d", r.Lo, r.Hi)
		}
	}
	return strings.Join(parts, ",")
}

// Compose returns policy with the managed funnel entries regenerated for namespaces (name -> the ports the
// ACL must allow; the caller includes pending namespaces and pending port changes):
//
//   - tagOwners["<NSPrefix><ns>"] = Settings.TagOwners, and
//   - one rule {"action":"accept","src":["<IngressTag>"],"dst":["<NSPrefix><ns>:<ports>"]}
//
// per namespace. Entries are managed when a tagOwners key starts with NSPrefix, or a rule has exactly the
// ingress tag as source and only dsts that start with NSPrefix. Everything else, and the key order of the
// document, is left alone. When the managed entries already match, the original text is returned unchanged.
func Compose(policy string, namespaces map[string][]Range, s *Settings) (string, error) {
	if !json.Valid([]byte(policy)) {
		return "", ErrNotPlainJSON
	}
	root, err := parseOrdered(policy)
	if err != nil {
		return "", ErrNotPlainJSON
	}
	top, ok := root.(*ordObj)
	if !ok {
		return "", ErrPolicyShape
	}

	names := make([]string, 0, len(namespaces))
	for ns := range namespaces {
		names = append(names, ns)
	}
	sort.Strings(names)

	owners := make([]any, len(s.TagOwners))
	for i, o := range s.TagOwners {
		owners[i] = o
	}

	// tagOwners
	var tagOwners *ordObj
	if v, present := top.get("tagOwners"); present {
		if tagOwners, ok = v.(*ordObj); !ok {
			return "", ErrPolicyShape
		}
	}
	wantOwners := map[string]string{}
	for _, ns := range names {
		wantOwners[s.NSPrefix+ns] = compact(owners)
	}
	haveOwners := map[string]string{}
	if tagOwners != nil {
		for _, k := range tagOwners.keys {
			if strings.HasPrefix(k, s.NSPrefix) {
				haveOwners[k] = compact(tagOwners.m[k])
			}
		}
	}

	// acls
	var acls []any
	aclsPresent := false
	if v, present := top.get("acls"); present {
		aclsPresent = true
		if acls, ok = v.([]any); !ok {
			return "", ErrPolicyShape
		}
	}
	managed := func(rule any) bool {
		o, ok := rule.(*ordObj)
		if !ok {
			return false
		}
		src, ok := stringList(o.m["src"])
		if !ok || len(src) != 1 || src[0] != s.IngressTag {
			return false
		}
		dst, ok := stringList(o.m["dst"])
		if !ok || len(dst) == 0 {
			return false
		}
		for _, d := range dst {
			if !strings.HasPrefix(d, s.NSPrefix) {
				return false
			}
		}
		return true
	}
	wantRules := make([]any, 0, len(names))
	for _, ns := range names {
		wantRules = append(wantRules, newObj(
			"action", "accept",
			"src", []any{s.IngressTag},
			"dst", []any{s.NSPrefix + ns + ":" + FormatPorts(namespaces[ns])},
		))
	}
	var haveRules, kept []any
	for _, r := range acls {
		if managed(r) {
			haveRules = append(haveRules, r)
		} else {
			kept = append(kept, r)
		}
	}

	if sameStringMap(haveOwners, wantOwners) && sameRules(haveRules, wantRules) {
		return policy, nil
	}

	// Rebuild: drop the managed entries, append the generated ones.
	if len(names) > 0 || tagOwners != nil {
		if tagOwners == nil {
			tagOwners = newObj()
			top.set("tagOwners", tagOwners)
		}
		for _, k := range append([]string(nil), tagOwners.keys...) {
			if strings.HasPrefix(k, s.NSPrefix) {
				tagOwners.del(k)
			}
		}
		for _, ns := range names {
			tagOwners.set(s.NSPrefix+ns, owners)
		}
	}
	if len(names) > 0 || aclsPresent {
		top.set("acls", append(kept, wantRules...))
	}
	var buf bytes.Buffer
	writeValue(&buf, top, 0)
	buf.WriteByte('\n')
	return buf.String(), nil
}

func sameStringMap(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func sameRules(have, want []any) bool {
	if len(have) != len(want) {
		return false
	}
	h := make([]string, len(have))
	w := make([]string, len(want))
	for i := range have {
		h[i], w[i] = compact(have[i]), compact(want[i])
	}
	sort.Strings(h)
	sort.Strings(w)
	for i := range h {
		if h[i] != w[i] {
			return false
		}
	}
	return true
}

func stringList(v any) ([]string, bool) {
	l, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, len(l))
	for i, e := range l {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out[i] = s
	}
	return out, true
}

// ordObj is a JSON object that keeps its key order, which encoding/json maps do not.
type ordObj struct {
	keys []string
	m    map[string]any
}

func newObj(kv ...any) *ordObj {
	o := &ordObj{m: map[string]any{}}
	for i := 0; i < len(kv); i += 2 {
		o.set(kv[i].(string), kv[i+1])
	}
	return o
}

func (o *ordObj) get(k string) (any, bool) { v, ok := o.m[k]; return v, ok }

func (o *ordObj) set(k string, v any) {
	if _, ok := o.m[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.m[k] = v
}

func (o *ordObj) del(k string) {
	if _, ok := o.m[k]; !ok {
		return
	}
	delete(o.m, k)
	for i, kk := range o.keys {
		if kk == k {
			o.keys = append(o.keys[:i], o.keys[i+1:]...)
			break
		}
	}
}

// parseOrdered decodes one JSON value, objects as *ordObj, arrays as []any, numbers as json.Number.
func parseOrdered(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing data")
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	t, err := dec.Token()
	if err != nil {
		return nil, err
	}
	d, isDelim := t.(json.Delim)
	if !isDelim {
		return t, nil // string, json.Number, bool or nil
	}
	switch d {
	case '{':
		o := &ordObj{m: map[string]any{}}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return nil, err
			}
			k, ok := kt.(string)
			if !ok {
				return nil, errors.New("bad object key")
			}
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			o.set(k, v)
		}
		if _, err := dec.Token(); err != nil { // the closing brace
			return nil, err
		}
		return o, nil
	case '[':
		arr := []any{}
		for dec.More() {
			v, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
		}
		if _, err := dec.Token(); err != nil {
			return nil, err
		}
		return arr, nil
	}
	return nil, errors.New("unexpected delimiter")
}

func compact(v any) string {
	var b bytes.Buffer
	writeCompact(&b, v)
	return b.String()
}

func writeScalar(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case string:
		enc := json.NewEncoder(b)
		enc.SetEscapeHTML(false)
		_ = enc.Encode(x)
		b.Truncate(b.Len() - 1) // Encode appends a newline
	case bool:
		b.WriteString(strconv.FormatBool(x))
	case json.Number:
		b.WriteString(x.String())
	default:
		panic(fmt.Sprintf("unsupported JSON value %T", v))
	}
}

func writeCompact(b *bytes.Buffer, v any) {
	switch x := v.(type) {
	case *ordObj:
		b.WriteByte('{')
		for i, k := range x.keys {
			if i > 0 {
				b.WriteByte(',')
			}
			writeScalar(b, k)
			b.WriteByte(':')
			writeCompact(b, x.m[k])
		}
		b.WriteByte('}')
	case []any:
		b.WriteByte('[')
		for i, e := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			writeCompact(b, e)
		}
		b.WriteByte(']')
	default:
		writeScalar(b, v)
	}
}

// writeValue writes v indented with two spaces; empty containers stay on one line.
func writeValue(b *bytes.Buffer, v any, depth int) {
	pad := func(n int) { b.WriteString(strings.Repeat("  ", n)) }
	switch x := v.(type) {
	case *ordObj:
		if len(x.keys) == 0 {
			b.WriteString("{}")
			return
		}
		b.WriteString("{\n")
		for i, k := range x.keys {
			pad(depth + 1)
			writeScalar(b, k)
			b.WriteString(": ")
			writeValue(b, x.m[k], depth+1)
			if i < len(x.keys)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		pad(depth)
		b.WriteByte('}')
	case []any:
		if len(x) == 0 {
			b.WriteString("[]")
			return
		}
		b.WriteString("[\n")
		for i, e := range x {
			pad(depth + 1)
			writeValue(b, e, depth+1)
			if i < len(x)-1 {
				b.WriteByte(',')
			}
			b.WriteByte('\n')
		}
		pad(depth)
		b.WriteByte(']')
	default:
		writeScalar(b, v)
	}
}
