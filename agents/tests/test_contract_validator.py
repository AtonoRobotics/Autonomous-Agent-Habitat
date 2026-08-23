"""Tests for harness/contract_validator.py — §11's 'Invalid model/tool
output | contract validator' recovery owner. See agentic_loop.py's own
tests (test_agentic_loop.py, test_agentic_loop_mcp.py) for the wired-in,
end-to-end proof that a validation failure is caught before dispatch;
these are the narrower unit-level checks on the validator itself."""

from __future__ import annotations

import pytest

from harness.contract_validator import (
    BUILTIN_ARG_SCHEMAS,
    ContractViolationError,
    validate_action,
    validate_tool_args,
)


def test_validate_action_accepts_a_real_tool_call():
    validate_action({"tool": "read_file", "args": {"path": "notes.txt"}})


def test_validate_action_accepts_a_real_done_action():
    validate_action({"tool": "done", "result": "all finished"})


def test_validate_action_rejects_missing_tool_key():
    with pytest.raises(ContractViolationError):
        validate_action({"args": {"path": "notes.txt"}})


def test_validate_action_rejects_non_string_tool():
    with pytest.raises(ContractViolationError):
        validate_action({"tool": 123})


def test_validate_action_rejects_a_non_object_action():
    with pytest.raises(ContractViolationError):
        validate_action("just a string, not an action object")


def test_validate_tool_args_accepts_matching_args():
    validate_tool_args("write_file", {"path": "a.txt", "content": "hi"}, BUILTIN_ARG_SCHEMAS["write_file"])


def test_validate_tool_args_rejects_missing_required_key():
    with pytest.raises(ContractViolationError):
        validate_tool_args("write_file", {"path": "a.txt"}, BUILTIN_ARG_SCHEMAS["write_file"])


def test_validate_tool_args_rejects_wrong_type():
    with pytest.raises(ContractViolationError):
        validate_tool_args("write_file", {"path": 123, "content": "hi"}, BUILTIN_ARG_SCHEMAS["write_file"])


def test_every_builtin_tool_name_has_an_arg_schema():
    from harness.agentic_loop import _BUILTIN_TOOL_NAMES

    assert set(BUILTIN_ARG_SCHEMAS) == _BUILTIN_TOOL_NAMES
