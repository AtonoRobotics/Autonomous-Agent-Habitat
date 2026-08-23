"""Tests for harness/agentic_loop.py — the real ReAct-style tool-calling
loop that replaced do_subagent_work's single-turn completion. Stands in
directly for the daemon's InferenceService.Complete RPC (same real
protobuf response shape as test_llm.py's _FakeInferenceService — one
layer closer than conftest.py's _FakeModelHandler, which stands in for
the provider behind the daemon), scripted with a queue of canned
responses so each test can drive a specific multi-turn scenario
deterministically.
"""

from __future__ import annotations

import json
from concurrent import futures

import grpc
import pytest

from context.budget import BudgetManager, approximate_token_count
from context.inferencepb import inference_pb2, inference_pb2_grpc
from context.llm import ModelClient
from harness.agentic_loop import LoopBudgetExceededError, UnknownToolError, run_agentic_loop
from harness.vfs import VFS


class _ScriptedDaemon(inference_pb2_grpc.InferenceServiceServicer):
    responses: list[str] = []
    usages: list[tuple[int, int]] = []
    costs: list[float] = []
    call_count = 0

    def Complete(self, request, context):
        cls = type(self)
        text = cls.responses[cls.call_count]
        input_tokens, output_tokens = cls.usages[cls.call_count] if cls.usages else (0, 0)
        cost_usd = cls.costs[cls.call_count] if cls.costs else 0.0
        cls.call_count += 1
        return inference_pb2.CompleteResponse(text=text, input_tokens=input_tokens, output_tokens=output_tokens, cost_usd=cost_usd)


@pytest.fixture()
def scripted_daemon():
    _ScriptedDaemon.responses = []
    _ScriptedDaemon.usages = []
    _ScriptedDaemon.costs = []
    _ScriptedDaemon.call_count = 0
    server = grpc.server(futures.ThreadPoolExecutor(max_workers=4))
    inference_pb2_grpc.add_InferenceServiceServicer_to_server(_ScriptedDaemon(), server)
    port = server.add_insecure_port("127.0.0.1:0")
    server.start()
    try:
        yield f"127.0.0.1:{port}"
    finally:
        server.stop(grace=1)


def _client(scripted_daemon) -> ModelClient:
    return ModelClient(daemon_grpc_addr=scripted_daemon, agent_token="tok", model="test-model")


def test_write_file_then_done_actually_writes_to_the_vfs(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_file", "args": {"path": "notes.txt", "content": "hello from the loop"}}),
        json.dumps({"tool": "done", "result": "wrote the notes"}),
    ]
    vfs = VFS(str(tmp_path / "run-1"))

    result = run_agentic_loop("write a note", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.result == "wrote the notes"
    assert result.turns_used == 2
    assert vfs.read_file("notes.txt") == "hello from the loop"
    assert _ScriptedDaemon.call_count == 2


def test_accumulates_real_token_usage_across_turns(scripted_daemon, tmp_path):
    """§2.1/§14 cost accounting (AMH-LEDGER.md Tier 1): the loop must sum
    each turn's real provider-reported usage, not fabricate or drop it —
    this is the seam do_subagent_work reads to record a run's real
    tokens_in/tokens_out."""
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_file", "args": {"path": "notes.txt", "content": "hello"}}),
        json.dumps({"tool": "done", "result": "done"}),
    ]
    _ScriptedDaemon.usages = [(100, 20), (30, 5)]
    vfs = VFS(str(tmp_path / "run-usage"))

    result = run_agentic_loop("write a note", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.tokens_in == 130
    assert result.tokens_out == 25


def test_accumulates_real_cost_usd_across_turns(scripted_daemon, tmp_path):
    """§2.1/§14 cost accounting: the loop must sum each turn's real
    daemon-computed cost_usd the same way it sums tokens — this is the
    seam do_subagent_work reads to record a run's real cost_usd."""
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_file", "args": {"path": "notes.txt", "content": "hello"}}),
        json.dumps({"tool": "done", "result": "done"}),
    ]
    _ScriptedDaemon.costs = [0.0125, 0.003]
    vfs = VFS(str(tmp_path / "run-cost"))

    result = run_agentic_loop("write a note", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.cost_usd == pytest.approx(0.0155)


def test_a_tool_error_is_fed_back_not_raised(scripted_daemon, tmp_path):
    """A bad tool call (reading a file that doesn't exist) must not abort
    the loop — it's fed back as a "error: ..." tool-result turn, the same
    way a real coding agent sees and recovers from its own mistakes."""
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "read_file", "args": {"path": "does-not-exist.txt"}}),
        json.dumps({"tool": "done", "result": "gave up, file was missing"}),
    ]
    vfs = VFS(str(tmp_path / "run-2"))

    result = run_agentic_loop("read a missing file", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.result == "gave up, file was missing"
    assert result.turns_used == 2


def test_malformed_json_response_raises(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = ["this is not json"]
    vfs = VFS(str(tmp_path / "run-3"))

    with pytest.raises(ValueError, match="not valid JSON"):
        run_agentic_loop("do something", vfs, _client(scripted_daemon), scripted_daemon)


def test_unknown_tool_name_raises(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [json.dumps({"tool": "delete_everything", "args": {}})]
    vfs = VFS(str(tmp_path / "run-4"))

    with pytest.raises(UnknownToolError):
        run_agentic_loop("do something", vfs, _client(scripted_daemon), scripted_daemon)


def test_max_turns_exceeded_raises_rather_than_fabricating_success(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [json.dumps({"tool": "ls", "args": {}})] * 5
    vfs = VFS(str(tmp_path / "run-5"))

    with pytest.raises(LoopBudgetExceededError):
        run_agentic_loop("never finish", vfs, _client(scripted_daemon), scripted_daemon, max_turns=3)

    # Never called more than max_turns times — the loop stops asking once
    # it gives up, it doesn't keep going past its own stated budget.
    assert _ScriptedDaemon.call_count == 3


def test_path_escaping_the_vfs_root_is_a_tool_error_not_a_crash(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "read_file", "args": {"path": "../../etc/passwd"}}),
        json.dumps({"tool": "done", "result": "refused to read outside my root"}),
    ]
    vfs = VFS(str(tmp_path / "run-6"))

    result = run_agentic_loop("try to escape", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.result == "refused to read outside my root"


def test_builtin_tool_args_failing_contract_validation_is_fed_back_not_raised(scripted_daemon, tmp_path):
    """§11 recovery ownership ('Invalid model/tool output | contract
    validator'): write_file called with a non-string 'path' must be
    caught by real schema validation (harness/contract_validator.py's
    BUILTIN_ARG_SCHEMAS) before _dispatch_builtin_tool ever runs. Before
    this validator existed, this exact input crashed the whole loop —
    vfs.write_file does `self.root / path`, and pathlib's `/` operator
    raises a bare TypeError on a non-str/PathLike operand, which isn't
    one of _dispatch_builtin_tool's caught exception types. Real schema
    validation catches it first and feeds it back as a recoverable
    error turn instead, the same posture every other tool-execution
    error already gets."""
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_file", "args": {"path": 123, "content": "hi"}}),
        json.dumps({"tool": "done", "result": "gave up, args were invalid"}),
    ]
    vfs = VFS(str(tmp_path / "run-contract-builtin"))

    result = run_agentic_loop("write a note", vfs, _client(scripted_daemon), scripted_daemon)

    assert result.result == "gave up, args were invalid"
    assert vfs.ls(".") == []


def test_write_todos_persists_to_the_vfs(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_todos", "args": {"items": ["step one", "step two"]}}),
        json.dumps({"tool": "done", "result": "planned"}),
    ]
    vfs = VFS(str(tmp_path / "run-7"))

    run_agentic_loop("make a plan", vfs, _client(scripted_daemon), scripted_daemon)

    todos = json.loads(vfs.read_file("todos.json"))
    assert [t["text"] for t in todos] == ["step one", "step two"]


def test_compaction_fires_under_a_small_window_budget(scripted_daemon, tmp_path):
    """Real compaction, not simulated: enough real tool-call turns
    (each a real HTTP round trip) that budget.over_compact_threshold
    genuinely crosses, driven by an artificially tiny window_budget so
    the test doesn't need dozens of real turns to prove it."""
    _ScriptedDaemon.responses = [json.dumps({"tool": "ls", "args": {}}) for _ in range(6)] + [
        json.dumps({"tool": "done", "result": "finished after compaction"})
    ]
    vfs = VFS(str(tmp_path / "run-8"))
    budget = BudgetManager(window_budget=30, compact_at=0.5, count_tokens=approximate_token_count)

    result = run_agentic_loop("do enough work to compact", vfs, _client(scripted_daemon), scripted_daemon, max_turns=10, budget=budget)

    assert result.result == "finished after compaction"
    assert result.compacted is True
