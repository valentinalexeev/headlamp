package mcp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/authz"
	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

const (
	// toolResponseSizeMaxBytes bounds what one tool returns to the client; the rest is cut off.
	toolResponseSizeMaxBytes = 256 << 10
	// argumentLengthMax bounds a path or query argument.
	argumentLengthMax = 256
)

// allowed reports whether caller may use t: its route is registered and the caller holds the
// route's permission.
func (m *Module) allowed(caller core.Caller, t tool) bool {
	if m.registered != nil && !m.registered(t.route) {
		return false
	}
	rule, ok := authz.Routes[t.route]
	p := authz.Default()
	if !ok || p == nil {
		return false
	}
	s := authz.Subject{Login: caller.Login, Groups: caller.Groups}
	switch rule.Kind {
	case authz.KindPerm:
		return p.Can(s, rule.Perm)
	case authz.KindAnyUser:
		return len(p.Role(s)) > 0
	}
	return false
}

func (m *Module) listTools(caller core.Caller) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		if !m.allowed(caller, t) {
			continue
		}
		out = append(out, map[string]any{
			"name": t.name, "title": t.title, "description": t.description,
			"inputSchema": t.inputSchema(),
			"annotations": map[string]any{
				"title": t.title, "readOnlyHint": t.readOnly, "destructiveHint": t.destructive,
				"idempotentHint": t.idempotent, "openWorldHint": false,
			},
		})
	}
	return out
}

func (t tool) inputSchema() map[string]any {
	props := map[string]any{}
	required := []string{}
	for _, p := range t.pathParams() {
		props[p] = map[string]any{"type": "string", "minLength": 1, "maxLength": argumentLengthMax}
		required = append(required, p)
	}
	for _, q := range t.query {
		typ := "string"
		if q.integer {
			typ = "integer"
		}
		props[q.name] = map[string]any{"type": typ, "description": q.description}
	}
	if t.body != "" {
		props["body"] = map[string]any{"type": "object", "description": "JSON object: " + t.body}
		required = append(required, "body")
	}
	return map[string]any{"type": "object", "properties": props, "required": required,
		"additionalProperties": false}
}

func findTool(name string) (tool, bool) {
	for _, t := range tools {
		if t.name == name {
			return t, true
		}
	}
	return tool{}, false
}

// callTool runs one tool. A refused or failed route is a tool result with isError, not a protocol
// error, so the model sees why.
func (m *Module) callTool(
	r *http.Request, caller core.Caller, name string, args map[string]any,
) (any, *rpcError) {
	t, ok := findTool(name)
	if !ok || !m.allowed(caller, t) {
		// An unknown tool and one the caller may not use answer the same.
		return nil, &rpcError{codeInvalidParams, "unknown tool: " + name}
	}
	req, err := t.request(r, caller, args)
	if err != nil {
		return toolResult(err.Error(), true), nil
	}
	rec := newRecorder()
	m.admin.ServeHTTP(rec, req)
	m.log.Info("mcp tool call", "login", caller.Login, "tool", t.name, "status", rec.status)
	return rec.result(), nil
}

// request builds the in-process admin request of a tool call, carrying the caller.
func (t tool) request(
	r *http.Request, caller core.Caller, args map[string]any,
) (*http.Request, error) {
	path, err := t.path(args)
	if err != nil {
		return nil, err
	}
	query, err := t.queryString(args)
	if err != nil {
		return nil, err
	}
	for k := range args {
		if !t.knows(k) {
			return nil, fmt.Errorf("unknown argument %q", k)
		}
	}
	var body []byte
	if t.method() != http.MethodGet {
		if body, err = t.bodyJSON(args); err != nil {
			return nil, err
		}
	}
	ctx := core.WithCaller(r.Context(), caller)
	u := &url.URL{Scheme: "http", Host: r.Host, Path: path, RawQuery: query}
	req, err := http.NewRequestWithContext(ctx, t.method(), u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build the request: %w", err)
	}
	req.RemoteAddr = r.RemoteAddr
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

func (t tool) knows(arg string) bool {
	if arg == "body" {
		return t.body != ""
	}
	for _, p := range t.pathParams() {
		if p == arg {
			return true
		}
	}
	for _, q := range t.query {
		if q.name == arg {
			return true
		}
	}
	return false
}

func (t tool) path(args map[string]any) (string, error) {
	_, path, _ := strings.Cut(t.route, " ")
	for _, p := range t.pathParams() {
		v, err := argument(args, p)
		if err != nil {
			return "", err
		}
		if v == "" {
			return "", fmt.Errorf("argument %q is required", p)
		}
		if v == "." || v == ".." {
			return "", fmt.Errorf("argument %q is not valid", p)
		}
		path = strings.Replace(path, "{"+p+"}", url.PathEscape(v), 1)
	}
	return path, nil
}

func (t tool) queryString(args map[string]any) (string, error) {
	q := url.Values{}
	for _, p := range t.query {
		v, err := argument(args, p.name)
		if err != nil {
			return "", err
		}
		if v != "" {
			q.Set(p.name, v)
		}
	}
	return q.Encode(), nil
}

// argument reads a path or query argument: a string or a whole number, short and without control
// characters.
func argument(args map[string]any, name string) (string, error) {
	var s string
	switch v := args[name].(type) {
	case nil:
		return "", nil
	case string:
		s = v
	case float64:
		if v != float64(int64(v)) {
			return "", fmt.Errorf("argument %q must be a whole number", name)
		}
		s = strconv.FormatInt(int64(v), 10)
	default:
		return "", fmt.Errorf("argument %q must be a string", name)
	}
	if len(s) > argumentLengthMax || strings.IndexFunc(s, unicode.IsControl) >= 0 {
		return "", fmt.Errorf("argument %q is not valid", name)
	}
	return s, nil
}

func (t tool) bodyJSON(args map[string]any) ([]byte, error) {
	v, ok := args["body"]
	if !ok || v == nil {
		if t.body != "" {
			return nil, fmt.Errorf(`argument "body" is required: %s`, t.body)
		}
		return []byte("{}"), nil
	}
	if _, isObject := v.(map[string]any); !isObject {
		return nil, fmt.Errorf(`argument "body" must be a JSON object`)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf(`argument "body": %w`, err)
	}
	return b, nil
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// recorder keeps the status and the first toolResponseSizeMaxBytes of an in-process response.
type recorder struct {
	header    http.Header
	status    int
	body      bytes.Buffer
	truncated bool
}

func newRecorder() *recorder { return &recorder{header: http.Header{}} }

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.WriteHeader(http.StatusOK)
	room := toolResponseSizeMaxBytes - rec.body.Len()
	if len(b) > room {
		rec.truncated = true
		b = b[:max(room, 0)]
	}
	rec.body.Write(b) // a bytes.Buffer write does not fail
	return len(b), nil
}

func (rec *recorder) result() map[string]any {
	status := rec.status
	if status == 0 {
		status = http.StatusOK
	}
	text := strings.TrimSpace(rec.body.String())
	if rec.truncated {
		text += "\n[cut off at " + strconv.Itoa(toolResponseSizeMaxBytes) + " bytes]"
	}
	if status >= http.StatusBadRequest {
		msg := fmt.Sprintf("HTTP %d %s", status, http.StatusText(status))
		if status == http.StatusForbidden && text == "" {
			msg += ": the caller does not hold the permission of this action"
		}
		if text != "" {
			msg += ": " + text
		}
		return toolResult(msg, true)
	}
	if text == "" {
		text = fmt.Sprintf("done (HTTP %d)", status)
	}
	return toolResult(text, false)
}
