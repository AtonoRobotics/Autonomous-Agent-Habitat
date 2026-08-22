package mcp

import "encoding/json"

// JSON-RPC 2.0 envelope and standard error codes, per
// https://www.jsonrpc.org/specification and the MCP 2025-06-18 base
// protocol (docs/specification/2025-06-18/basic/*), which reuses it
// unmodified.

const jsonrpcVersion = "2.0"

// request is the shape of every incoming JSON-RPC message on the MCP
// endpoint. ID is omitted (nil) for a notification (e.g.
// notifications/initialized) — the presence/absence of ID, not the
// method name, is what distinguishes a request from a notification.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

func (r request) isNotification() bool { return len(r.ID) == 0 }

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Standard JSON-RPC 2.0 error codes — identical across every MCP
// protocol era; MCP does not redefine these, only adds new codes in
// disjoint ranges (which this package, targeting 2025-06-18, does not
// need: those were introduced in 2026-07-28).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

func errorResponse(id json.RawMessage, code int, message string) response {
	return response{JSONRPC: jsonrpcVersion, ID: id, Error: &rpcError{Code: code, Message: message}}
}

func resultResponse(id json.RawMessage, result any) response {
	return response{JSONRPC: jsonrpcVersion, ID: id, Result: result}
}
