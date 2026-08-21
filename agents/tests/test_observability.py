"""Verifies OTel spans are actually recorded (not just plumbed and
untested) for the two span types §13 calls out: agent runs and tool
calls. Uses an in-memory exporter — no OTLP endpoint required.
"""

from __future__ import annotations

import uuid

import pytest
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from context.observability import agent_run_span, init_tracing, tool_call_span


@pytest.fixture(scope="session")
def _global_exporter():
    # OTel's global TracerProvider can only be installed once per process
    # (a second set_tracer_provider() call is a silent no-op) — so tracing
    # is initialized exactly once per test session, and individual tests
    # just clear the exporter's buffer between runs.
    exporter = InMemorySpanExporter()
    init_tracing("amh-agents-test", exporter=exporter)
    return exporter


@pytest.fixture()
def span_exporter(_global_exporter):
    _global_exporter.clear()
    yield _global_exporter
    _global_exporter.clear()


def test_agent_run_span_has_genai_attributes(span_exporter):
    with agent_run_span(agent_id="goal-123", model="claude-sonnet-4-6"):
        pass

    spans = span_exporter.get_finished_spans()
    assert len(spans) == 1
    span = spans[0]
    assert span.name == "invoke_agent"
    assert span.attributes["gen_ai.operation.name"] == "invoke_agent"
    assert span.attributes["gen_ai.agent.id"] == "goal-123"
    assert span.attributes["gen_ai.request.model"] == "claude-sonnet-4-6"


def test_tool_call_span_has_genai_attributes(span_exporter):
    with tool_call_span("device_action:vent-actuator.set_open_pct", **{"amh.device.host": "127.0.0.1"}):
        pass

    spans = span_exporter.get_finished_spans()
    assert len(spans) == 1
    span = spans[0]
    assert span.name == "execute_tool"
    assert span.attributes["gen_ai.operation.name"] == "execute_tool"
    assert span.attributes["gen_ai.tool.name"] == "device_action:vent-actuator.set_open_pct"
    assert span.attributes["amh.device.host"] == "127.0.0.1"


def test_tool_call_span_records_error_on_exception(span_exporter):
    with pytest.raises(ValueError):
        with tool_call_span("device_action:broken") as span:
            span.set_attribute("error.type", "test_error")
            raise ValueError("boom")

    spans = span_exporter.get_finished_spans()
    assert len(spans) == 1
    assert spans[0].attributes["error.type"] == "test_error"


def test_pursue_goal_emits_nested_agent_run_spans(span_exporter, tmp_path):
    """The real integration: pursue_goal (parent) and each run_subagent
    (child) must each produce their own invoke_agent span, AND the child
    spans must share the parent's trace ID — proving trace-context
    propagation (context.observability.inject_trace_context /
    workflows.goal.start_subagent) actually threads through
    DBOS.start_workflow's worker-thread boundary rather than each child
    starting an unrelated trace of its own."""
    from dbos import DBOS

    from workflows import ontology
    from workflows.goal import pursue_goal
    from workflows.runtime import init_dbos

    db_path = str(tmp_path / "amh.db")
    import os

    migrations_dir = os.path.join(
        os.path.dirname(os.path.dirname(os.path.dirname(os.path.abspath(__file__)))),
        "store", "migrations",
    )
    ontology.apply_migrations(db_path, migrations_dir)

    init_dbos("amh-agents-otel-test", db_path)
    DBOS.launch()
    try:
        goal_id = str(uuid.uuid4())
        pursue_goal(goal_id, "poll temperature; open vent", db_path)
    finally:
        DBOS.destroy()

    spans = span_exporter.get_finished_spans()
    invoke_agent_spans = [s for s in spans if s.name == "invoke_agent"]
    # One for the parent goal, one per sub-task (2 clauses -> 2 subagents).
    assert len(invoke_agent_spans) == 3

    agent_ids = {s.attributes["gen_ai.agent.id"] for s in invoke_agent_spans}
    assert goal_id in agent_ids
    parent_span = next(s for s in invoke_agent_spans if s.attributes["gen_ai.agent.id"] == goal_id)
    child_spans = [s for s in invoke_agent_spans if s.attributes["gen_ai.agent.id"] != goal_id]
    assert len(child_spans) == 2
    for span in invoke_agent_spans:
        assert span.attributes["gen_ai.operation.name"] == "invoke_agent"

    # The actual point of this test: children share the parent's trace ID
    # (propagated across the DBOS.start_workflow worker-thread boundary),
    # and each child's parent_span_id points at the parent goal span.
    for child in child_spans:
        assert child.context.trace_id == parent_span.context.trace_id
        assert child.parent is not None
        assert child.parent.span_id == parent_span.context.span_id
