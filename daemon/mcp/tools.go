package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/actuation"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
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
// failure (the device refused the command, the ticket wasn't approved,
// …) is reported HERE with IsError true — never as a JSON-RPC protocol
// error. A JSON-RPC error is reserved for malformed requests this
// package itself cannot make sense of (bad JSON, unknown method/tool).
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

// tools is this server's fixed catalog: the device-actuation kernel and
// the ApprovalGate pair, chosen first because together they demonstrate
// this habitat's own "no effect without an approved card" invariant
// working end to end through an external MCP client — not the full
// surface of daemon/api. Further AMH capabilities (computers, safety
// cases, accounts, inference) can be added as more tools following the
// exact same pattern: a JSON Schema, an argument struct, and a call into
// the same internal Go package daemon/api itself calls — this package
// deliberately does not loop back through HTTP to reach them.
var tools = []tool{
	{
		Name: "actuate_device",
		Description: "Execute a device_action against a real connected device. Params substitute into the " +
			"device_action's own server-owned command template — never raw shell text. If the action has no " +
			"verified inverse and no approved SafetyCase, ticket_id must name an approval_gate ticket already " +
			"approved for this exact device_action_id and params (see request_approval/check_approval); " +
			"otherwise the call fails.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_action_id": map[string]any{"type": "string", "description": "device_action.id to execute"},
				"params": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "named parameter values substituted into the device_action's forward/read-state templates",
				},
				"ticket_id": map[string]any{"type": "string", "description": "an approved approval_gate ticket ID, if this action has no other autonomy path"},
			},
			"required": []string{"device_action_id"},
		},
		handle: handleActuateDevice,
	},
	{
		Name: "request_approval",
		Description: "Request an approval_gate ticket for one exact (device_action_id, params) action that has " +
			"no verified inverse and no approved SafetyCase. Returns immediately with a ticket_id — creating a " +
			"ticket never grants anything by itself; an operator must approve it out-of-band before " +
			"actuate_device will accept it (see check_approval to poll).",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"device_action_id": map[string]any{"type": "string"},
				"params": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
				},
				"risk":   map[string]any{"type": "string", "enum": []string{"reversible", "irreversible"}},
				"reason": map[string]any{"type": "string", "description": "free-text audit context only — has no bearing on what the ticket authorizes"},
			},
			"required": []string{"device_action_id", "risk"},
		},
		handle: handleRequestApproval,
	},
	{
		Name:        "check_approval",
		Description: "Check whether an approval_gate ticket has been approved by an operator yet.",
		InputSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ticket_id": map[string]any{"type": "string"}},
			"required":   []string{"ticket_id"},
		},
		handle: handleCheckApproval,
	},
}

func findTool(name string) (tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return tool{}, false
}

type actuateDeviceArgs struct {
	DeviceActionID string            `json:"device_action_id"`
	Params         map[string]string `json:"params"`
	TicketID       string            `json:"ticket_id"`
}

func handleActuateDevice(ctx context.Context, s *Server, raw json.RawMessage) (toolResult, error) {
	var args actuateDeviceArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("mcp: invalid actuate_device arguments: %w", err)
	}
	if args.DeviceActionID == "" {
		return toolResult{}, errors.New("mcp: device_action_id is required")
	}

	act, err := s.Registry.ResolveActuator(ctx, args.DeviceActionID)
	if err != nil {
		return errorResult(err), nil
	}
	var ticket *interlocks.Ticket
	if args.TicketID != "" {
		ticket = &interlocks.Ticket{ID: args.TicketID}
	}
	result, err := actuation.ExecuteTraced(ctx, s.Tracer, s.DB, act, s.Gate, args.DeviceActionID, actuation.Command{Params: args.Params}, ticket)
	if err != nil {
		return errorResult(err), nil
	}
	return textResult(result), nil
}

type requestApprovalArgs struct {
	DeviceActionID string            `json:"device_action_id"`
	Params         map[string]string `json:"params"`
	Risk           string            `json:"risk"`
	Reason         string            `json:"reason"`
}

func handleRequestApproval(ctx context.Context, s *Server, raw json.RawMessage) (toolResult, error) {
	var args requestApprovalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("mcp: invalid request_approval arguments: %w", err)
	}
	risk := interlocks.Risk(args.Risk)
	if risk != interlocks.Reversible && risk != interlocks.Irreversible {
		return toolResult{}, errors.New(`mcp: risk must be "reversible" or "irreversible"`)
	}
	if args.DeviceActionID == "" {
		return toolResult{}, errors.New("mcp: device_action_id is required")
	}

	ticket, err := s.Gate.Require(ctx, args.DeviceActionID, args.Params, args.Reason, risk)
	if err != nil {
		return errorResult(err), nil
	}
	return jsonTextResult(map[string]string{"ticket_id": ticket.ID}), nil
}

type checkApprovalArgs struct {
	TicketID string `json:"ticket_id"`
}

func handleCheckApproval(ctx context.Context, s *Server, raw json.RawMessage) (toolResult, error) {
	var args checkApprovalArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return toolResult{}, fmt.Errorf("mcp: invalid check_approval arguments: %w", err)
	}
	if args.TicketID == "" {
		return toolResult{}, errors.New("mcp: ticket_id is required")
	}

	satisfied, err := s.Gate.IsSatisfied(ctx, interlocks.Ticket{ID: args.TicketID})
	if err != nil {
		return errorResult(err), nil
	}
	return jsonTextResult(map[string]bool{"satisfied": satisfied}), nil
}
