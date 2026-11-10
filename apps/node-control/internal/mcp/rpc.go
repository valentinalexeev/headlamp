package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/valentinalexeev/headlamp/apps/node-control/internal/core"
)

// The MCP revisions this server speaks, newest first. All of them use the same messages for what it
// offers (tools only), so it answers with the client's revision when it knows it, else with the
// newest.
var protocolVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}

// requestBodySizeMaxBytes bounds one JSON-RPC message. A policy for policy_apply is the largest
// expected input.
const requestBodySizeMaxBytes = 1 << 20

// JSON-RPC 2.0 error codes.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
)

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// errNotification marks a message that needs no answer: a notification, or a response from the
// client.
var errNotification = errors.New("no answer")

// parseMessage reads one JSON-RPC message. A batch (a JSON array) is refused: MCP since 2025-06-18
// has none.
func parseMessage(body []byte) (rpcRequest, *rpcError) {
	body = bytes.TrimSpace(body)
	if len(body) == 0 || body[0] != '{' {
		if len(body) > 0 && body[0] == '[' {
			return rpcRequest{}, &rpcError{codeInvalidRequest, "batches are not supported"}
		}
		return rpcRequest{}, &rpcError{codeParse, "the body is not a JSON object"}
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return rpcRequest{}, &rpcError{codeParse, "the body is not valid JSON"}
	}
	if req.JSONRPC != "2.0" {
		return rpcRequest{}, &rpcError{codeInvalidRequest, `jsonrpc must be "2.0"`}
	}
	if len(req.ID) > 0 && !validID(req.ID) {
		return rpcRequest{}, &rpcError{codeInvalidRequest, "id must be a string or a number"}
	}
	return req, nil
}

func validID(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}

// serveRPC answers one authenticated POST.
func (m *Module) serveRPC(w http.ResponseWriter, r *http.Request, caller core.Caller) {
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(protocolVersions, v) {
		http.Error(w, "unsupported MCP-Protocol-Version", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, requestBodySizeMaxBytes))
	if err != nil {
		http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}
	req, perr := parseMessage(body)
	if perr != nil {
		writeRPC(w, rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: perr})
		return
	}
	// A notification, or a client's response to a request this server never sends.
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result, rerr := m.dispatch(r, caller, req)
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	writeRPC(w, resp)
}

func writeRPC(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		return // the client went away; nothing more to tell it
	}
}

func (m *Module) dispatch(r *http.Request, caller core.Caller, req rpcRequest) (any, *rpcError) {
	switch req.Method {
	case "initialize":
		return m.initialize(req.Params), nil
	case "ping":
		return struct{}{}, nil
	case "tools/list":
		return map[string]any{"tools": m.listTools(caller)}, nil
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(orEmpty(req.Params), &p); err != nil || p.Name == "" {
			return nil, &rpcError{codeInvalidParams,
				"tools/call needs a name and an arguments object"}
		}
		return m.callTool(r, caller, p.Name, p.Arguments)
	}
	return nil, &rpcError{codeMethodNotFound, "method not found: " + req.Method}
}

func orEmpty(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("{}")
	}
	return raw
}

const instructions = "This server manages a headlamp site through node-control: exit nodes, " +
	"enrolled devices, scheduled tasks, the headscale policy, the funnel and serve modules. " +
	"Each tool runs with the permissions of the person who connected, and is recorded in the " +
	"audit log under that login. A policy change is applied on probation: confirm it with " +
	"policy_confirm before its deadline, or it is reverted. Prefer the read tools to find ids " +
	"and the current state before a change."

func (m *Module) initialize(params json.RawMessage) any {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	version := protocolVersions[0]
	err := json.Unmarshal(orEmpty(params), &p)
	if err == nil && slices.Contains(protocolVersions, p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo": map[string]any{
			"name": "headlamp-node-control", "title": "headlamp node-control",
			"version": core.DisplayVersion(),
		},
		"instructions": instructions,
	}
}
