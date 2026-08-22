"""Proves pursue_goal's subagent fan-out is actually bounded (Artifact F /
docs/AMH-SPECIFICATION.md §14, V1: "isolated subordinate-agent workflows
with bounded concurrency") — not just that the code calls a Queue API, but
that concurrent subagent execution genuinely never exceeds the configured
limit.

Runs pursue_goal in a subprocess with AMH_SUBAGENT_CONCURRENCY=1: the
concurrency bound is read once, at import time, into a module-level DBOS
Queue (workflows/goal.py) — by the time this test file's own process has
already imported workflows.goal (transitively, via other test files in the
same session), a plain monkeypatch.setenv would be too late to affect it.
A subprocess gets a fresh interpreter, and therefore a fresh Queue built
from this test's own environment.
"""

from __future__ import annotations

import json
import os
import subprocess
import sys
import textwrap
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from conftest import REPO_ROOT, register_model_provider_account

# How long each subagent's fake model call "thinks" for — long enough that
# two genuinely concurrent calls would overlap and both be observed by
# _ConcurrencyTrackingHandler, short enough to keep the test fast.
_SUBAGENT_THINK_TIME_S = 0.2


class _ConcurrencyTrackingHandler(BaseHTTPRequestHandler):
    """Same decompose-vs-subagent distinction as conftest.py's
    _FakeModelHandler, plus real concurrency tracking: every subagent call
    holds a lock-protected counter open for _SUBAGENT_THINK_TIME_S, so two
    calls that are genuinely running at the same time will overlap that
    window and both be counted — the whole point of the test."""

    lock = threading.Lock()
    current = 0
    max_observed = 0
    subagent_call_count = 0

    def log_message(self, format, *args):
        pass

    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length))
        messages = body["messages"]
        system_content = messages[0]["content"] if messages and messages[0]["role"] == "system" else ""
        user_content = next(m["content"] for m in reversed(messages) if m["role"] == "user")

        if "JSON array" in system_content:
            clauses = [c.strip() for c in user_content.split(";") if c.strip()] or [user_content]
            content = json.dumps([{"objective": c} for c in clauses])
        else:
            cls = type(self)
            with cls.lock:
                cls.current += 1
                cls.max_observed = max(cls.max_observed, cls.current)
                cls.subagent_call_count += 1
            time.sleep(_SUBAGENT_THINK_TIME_S)
            with cls.lock:
                cls.current -= 1
            content = f"completed: {user_content}"

        response = json.dumps({"choices": [{"message": {"role": "assistant", "content": content}}]}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(response)))
        self.end_headers()
        self.wfile.write(response)


def test_pursue_goal_bounds_subagent_concurrency(daemon, db_path):
    server = ThreadingHTTPServer(("127.0.0.1", 0), _ConcurrencyTrackingHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        port = server.server_address[1]
        register_model_provider_account(daemon, f"http://127.0.0.1:{port}", provider="test-fake")

        goal_id = str(uuid.uuid4())
        # Four clauses -> four subagents. With the default concurrency (5)
        # they would all be free to run at once; this only proves anything
        # because the subprocess below sets AMH_SUBAGENT_CONCURRENCY=1.
        goal_text = "water the plants; check the sensors; log the readings; report status"

        script = textwrap.dedent(f"""
            import sys
            sys.path.insert(0, {(REPO_ROOT + "/agents")!r})
            from dbos import DBOS
            from workflows.goal import pursue_goal
            from workflows.runtime import init_dbos

            init_dbos("amh-subagent-concurrency-test", {db_path!r})
            DBOS.launch()
            try:
                result = pursue_goal({goal_id!r}, {goal_text!r}, {db_path!r}, {daemon.base_url!r}, {daemon.agent_token!r})
                print("RESULT:" + result)
            finally:
                DBOS.destroy()
        """)
        env = dict(
            os.environ,
            AMH_SUBAGENT_CONCURRENCY="1",
            ADAPTER_MODEL="test-fake-model",
            ADAPTER_PROVIDER="test-fake",
        )
        result = subprocess.run(
            [sys.executable, "-c", script],
            cwd=os.path.join(REPO_ROOT, "agents"),
            capture_output=True,
            text=True,
            timeout=60,
            env=env,
        )
        assert result.returncode == 0, result.stderr
        assert "RESULT:" in result.stdout
    finally:
        server.shutdown()
        thread.join(timeout=5)

    # All four subagents genuinely ran (not silently dropped or merged).
    assert _ConcurrencyTrackingHandler.subagent_call_count == 4
    # The actual property under test: never more than one subagent's model
    # call was in flight at the same time, despite four being eligible to
    # start at once.
    assert _ConcurrencyTrackingHandler.max_observed == 1, (
        f"expected AMH_SUBAGENT_CONCURRENCY=1 to serialize all subagent calls, "
        f"but observed {_ConcurrencyTrackingHandler.max_observed} running concurrently"
    )
