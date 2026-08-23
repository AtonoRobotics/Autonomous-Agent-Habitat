"""Real contract validation for model/tool output — the recovery owner
§11's self-healing table names ("Invalid model/tool output | contract
validator") but that, until now, had no code anywhere backing it:
agents/harness/agentic_loop.py only ever did ad hoc isinstance/key checks
on a model's parsed JSON, and never validated tool-call *args* against
anything at all — an MCP tool's own server-declared input_schema was
already being fetched via list_tools() and then completely ignored.

Two distinct checks live here:

- validate_action(action): the model's whole turn against
  contracts/agent-action.schema.json — the envelope every turn must have.
  Deliberately unchanged in effect from agentic_loop.py's prior ad hoc
  check (a malformed envelope still raises, it doesn't get fed back for
  the model to retry — see that module's docstring for why); this
  replaces "hand-rolled, could silently pass something the schema
  wouldn't" with real, tested schema validation.
- validate_tool_args(tool, args, schema): a tool's args against its own
  declared schema — BUILTIN_ARG_SCHEMAS below for built-in tools, or an
  MCP tool's own real input_schema straight from the third-party server.
  This is genuinely new: previously a malformed built-in call only
  happened to fail correctly via an accidental KeyError inside
  _dispatch_builtin_tool, and a malformed MCP call was never checked at
  all before being sent to the external server. A failure here is fed
  back to the model as a recoverable "error: ..." turn — the same
  posture agentic_loop.py already takes for every other tool-execution
  error (a bad path, a missing file, an MCP server's own runtime error)
  — not a hard raise.
"""

from __future__ import annotations

import json
import os

import jsonschema

_CONTRACTS_DIR = os.path.join(
    os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))), "contracts"
)

with open(os.path.join(_CONTRACTS_DIR, "agent-action.schema.json")) as _f:
    _ACTION_SCHEMA = json.load(_f)


class ContractViolationError(Exception):
    """A model's turn, or a tool call's args, failed real schema
    validation — §11's 'Invalid model/tool output' failure mode."""


def validate_action(action: object) -> None:
    """Raises ContractViolationError if action doesn't match
    contracts/agent-action.schema.json."""
    try:
        jsonschema.validate(action, _ACTION_SCHEMA)
    except jsonschema.ValidationError as e:
        raise ContractViolationError(e.message) from e


BUILTIN_ARG_SCHEMAS: dict[str, dict] = {
    "read_file": {
        "type": "object",
        "required": ["path"],
        "properties": {
            "path": {"type": "string"},
            "offset": {"type": "integer"},
            "limit": {"type": ["integer", "null"]},
        },
    },
    "write_file": {
        "type": "object",
        "required": ["path", "content"],
        "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
    },
    "edit_file": {
        "type": "object",
        "required": ["path", "old", "new"],
        "properties": {"path": {"type": "string"}, "old": {"type": "string"}, "new": {"type": "string"}},
    },
    "ls": {
        "type": "object",
        "properties": {"path": {"type": "string"}},
    },
    "glob": {
        "type": "object",
        "required": ["pattern"],
        "properties": {"pattern": {"type": "string"}},
    },
    "grep": {
        "type": "object",
        "required": ["pattern"],
        "properties": {"pattern": {"type": "string"}, "path": {"type": "string"}},
    },
    "write_todos": {
        "type": "object",
        "required": ["items"],
        "properties": {"items": {"type": "array", "items": {"type": "string"}}},
    },
}


def validate_tool_args(tool: str, args: object, schema: dict) -> None:
    """Raises ContractViolationError if args doesn't match schema — the
    tool's own declared shape."""
    try:
        jsonschema.validate(args, schema)
    except jsonschema.ValidationError as e:
        raise ContractViolationError(f"{tool}: {e.message}") from e
