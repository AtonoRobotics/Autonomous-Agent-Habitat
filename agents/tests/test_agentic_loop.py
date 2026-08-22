"""Tests for harness/agentic_loop.py — the real ReAct-style tool-calling
loop that replaced do_subagent_work's single-turn completion. Stands in
directly for the daemon's /v1/inference/complete route (same {"text": ...}
shape as test_llm.py's _FakeDaemon — one layer closer than conftest.py's
_FakeModelHandler, which stands in for the provider behind the daemon),
scripted with a queue of canned responses so each test can drive a
specific multi-turn scenario deterministically.
"""

from __future__ import annotations

import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from threading import Thread

import pytest

from context.budget import BudgetManager, approximate_token_count
from context.llm import ModelClient
from harness.agentic_loop import LoopBudgetExceededError, UnknownToolError, run_agentic_loop
from harness.vfs import VFS


class _ScriptedDaemon(BaseHTTPRequestHandler):
    responses: list[str] = []
    call_count = 0

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        self.rfile.read(length)
        cls = type(self)
        text = cls.responses[cls.call_count]
        cls.call_count += 1
        body = json.dumps({"text": text}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.end_headers()
        self.wfile.write(body)


@pytest.fixture()
def scripted_daemon():
    _ScriptedDaemon.responses = []
    _ScriptedDaemon.call_count = 0
    server = HTTPServer(("127.0.0.1", 0), _ScriptedDaemon)
    thread = Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}"
    finally:
        server.shutdown()
        thread.join(timeout=5)


def _client(scripted_daemon) -> ModelClient:
    return ModelClient(daemon_api_base_url=scripted_daemon, agent_token="tok", model="test-model")


def test_write_file_then_done_actually_writes_to_the_vfs(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_file", "args": {"path": "notes.txt", "content": "hello from the loop"}}),
        json.dumps({"tool": "done", "result": "wrote the notes"}),
    ]
    vfs = VFS(str(tmp_path / "run-1"))

    result = run_agentic_loop("write a note", vfs, _client(scripted_daemon))

    assert result.result == "wrote the notes"
    assert result.turns_used == 2
    assert vfs.read_file("notes.txt") == "hello from the loop"
    assert _ScriptedDaemon.call_count == 2


def test_a_tool_error_is_fed_back_not_raised(scripted_daemon, tmp_path):
    """A bad tool call (reading a file that doesn't exist) must not abort
    the loop — it's fed back as a "error: ..." tool-result turn, the same
    way a real coding agent sees and recovers from its own mistakes."""
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "read_file", "args": {"path": "does-not-exist.txt"}}),
        json.dumps({"tool": "done", "result": "gave up, file was missing"}),
    ]
    vfs = VFS(str(tmp_path / "run-2"))

    result = run_agentic_loop("read a missing file", vfs, _client(scripted_daemon))

    assert result.result == "gave up, file was missing"
    assert result.turns_used == 2


def test_malformed_json_response_raises(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = ["this is not json"]
    vfs = VFS(str(tmp_path / "run-3"))

    with pytest.raises(ValueError, match="not valid JSON"):
        run_agentic_loop("do something", vfs, _client(scripted_daemon))


def test_unknown_tool_name_raises(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [json.dumps({"tool": "delete_everything", "args": {}})]
    vfs = VFS(str(tmp_path / "run-4"))

    with pytest.raises(UnknownToolError):
        run_agentic_loop("do something", vfs, _client(scripted_daemon))


def test_max_turns_exceeded_raises_rather_than_fabricating_success(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [json.dumps({"tool": "ls", "args": {}})] * 5
    vfs = VFS(str(tmp_path / "run-5"))

    with pytest.raises(LoopBudgetExceededError):
        run_agentic_loop("never finish", vfs, _client(scripted_daemon), max_turns=3)

    # Never called more than max_turns times — the loop stops asking once
    # it gives up, it doesn't keep going past its own stated budget.
    assert _ScriptedDaemon.call_count == 3


def test_path_escaping_the_vfs_root_is_a_tool_error_not_a_crash(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "read_file", "args": {"path": "../../etc/passwd"}}),
        json.dumps({"tool": "done", "result": "refused to read outside my root"}),
    ]
    vfs = VFS(str(tmp_path / "run-6"))

    result = run_agentic_loop("try to escape", vfs, _client(scripted_daemon))

    assert result.result == "refused to read outside my root"


def test_write_todos_persists_to_the_vfs(scripted_daemon, tmp_path):
    _ScriptedDaemon.responses = [
        json.dumps({"tool": "write_todos", "args": {"items": ["step one", "step two"]}}),
        json.dumps({"tool": "done", "result": "planned"}),
    ]
    vfs = VFS(str(tmp_path / "run-7"))

    run_agentic_loop("make a plan", vfs, _client(scripted_daemon))

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

    result = run_agentic_loop("do enough work to compact", vfs, _client(scripted_daemon), max_turns=10, budget=budget)

    assert result.result == "finished after compaction"
    assert result.compacted is True
