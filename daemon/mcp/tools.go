package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// tool is one MCP-exposed capability. inputSchema is literal JSON Schema
// (draft 2020-12, per the MCP spec) describing arguments — handed to
// clients verbatim via tools/list, and used nowhere else by this package
// (Go's json.Unmarshal into a Go struct is the actual argument
// validation; the schema is documentation for the calling model/client,
// not re-validated server-side against itself).
type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
	handle      func(ctx context.Context, s *Server, arguments json.RawMessage) (toolResult, error)
}

// toolResult is a tools/call result. Per the MCP spec, a tool-level
// failure is reported HERE with IsError true — never as a JSON-RPC
// protocol error. A JSON-RPC error is reserved for malformed requests
// this package itself cannot make sense of (bad JSON, unknown method/
// tool).
type toolResult struct {
	Content []contentBlock `json:"content"`
	IsError bool           `json:"isError,omitempty"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func textResult(text string) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: text}}}
}

func errorResult(err error) toolResult {
	return toolResult{Content: []contentBlock{{Type: "text", Text: err.Error()}}, IsError: true}
}

func jsonTextResult(v any) toolResult {
	b, err := json.Marshal(v)
	if err != nil {
		return errorResult(fmt.Errorf("mcp: marshal tool result: %w", err))
	}
	return textResult(string(b))
}

// tools is this server's fixed catalog — currently empty. A capability
// becomes an MCP tool the same way the (now-removed) device-actuation
// tools worked: a JSON Schema, an argument struct, and a call into the
// same internal Go package daemon/api itself calls — this package
// deliberately does not loop back through HTTP to reach them.
var tools = []tool{}

func findTool(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}
