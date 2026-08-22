"""OpenTelemetry tracing for the agent layer, per
docs/AMH-SPECIFICATION.md §13: spans for agent runs, tool calls, and
sub-agent spawns, using OTel's GenAI semantic conventions
(gen_ai.operation.name, gen_ai.agent.id, etc.) rather than ad-hoc span
names, so traces are queryable the same way across any OTel-compatible
backend.

No exporter is configured by init_tracing() unless an OTLP endpoint is
passed — spans are created and recorded on the current provider either
way (visible to an in-memory exporter in tests), but nothing is shipped
over the network unless OTEL_EXPORTER_OTLP_ENDPOINT is set, matching
.env.example's opt-in OTLP config.
"""

from __future__ import annotations

from contextlib import contextmanager
from typing import Iterator

from opentelemetry import propagate, trace
from opentelemetry.sdk.resources import SERVICE_NAME, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor, SpanExporter

_TRACER_NAME = "amh.agents"


def inject_trace_context() -> dict[str, str]:
    """Captures the currently active span's context as a plain dict (W3C
    traceparent format) that can cross a serialization boundary — a DBOS
    child workflow started via DBOS.start_workflow runs on a separate
    worker thread with no ambient OTel context of its own, so without
    this, every child workflow starts a brand-new trace instead of being
    nested under its parent's. Call this INSIDE the parent's active span
    (i.e. inside its `with agent_run_span(...):` block), then pass the
    result as an explicit argument to the child workflow, which restores
    it via agent_run_span(..., trace_context=...).

    Returns an empty dict if there is no active span — extract() on an
    empty carrier is a documented no-op, so callers don't need to
    special-case "not inside a span"."""
    carrier: dict[str, str] = {}
    propagate.inject(carrier)
    return carrier


def init_tracing(service_name: str = "amh-agents", exporter: SpanExporter | None = None) -> TracerProvider:
    """Installs a TracerProvider as the global default. Pass an explicit
    exporter (e.g. an in-memory one for tests, or an OTLP exporter for
    production) — with none given, spans are recorded but never exported,
    the correct default until an exporter is configured.

    Uses SimpleSpanProcessor (export-on-end, no batching), which keeps
    test assertions straightforward (spans are visible immediately, no
    explicit flush needed). Swap to BatchSpanProcessor if a real OTLP
    collector's throughput demands it."""
    provider = TracerProvider(resource=Resource.create({SERVICE_NAME: service_name}))
    if exporter is not None:
        provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    return provider


def get_tracer():
    return trace.get_tracer(_TRACER_NAME)


@contextmanager
def agent_run_span(agent_id: str, model: str | None = None, trace_context: dict[str, str] | None = None) -> Iterator[trace.Span]:
    """gen_ai.operation.name=invoke_agent, per OTel's GenAI semantic
    conventions — wraps one agent run (pursue_goal or a sub-agent).

    Pass trace_context (from inject_trace_context(), captured in the
    parent's own agent_run_span block) when this span belongs to a DBOS
    child workflow started via DBOS.start_workflow — otherwise the child
    runs on a separate worker thread with no ambient parent context and
    starts an unrelated trace. Omit it for a genuinely top-level run."""
    tracer = get_tracer()
    parent_context = propagate.extract(trace_context) if trace_context else None
    with tracer.start_as_current_span("invoke_agent", context=parent_context) as span:
        span.set_attribute("gen_ai.operation.name", "invoke_agent")
        span.set_attribute("gen_ai.agent.id", agent_id)
        if model:
            span.set_attribute("gen_ai.request.model", model)
        yield span


@contextmanager
def tool_call_span(tool_name: str, **extra_attrs: str) -> Iterator[trace.Span]:
    """gen_ai.operation.name=execute_tool — wraps one tool/connector
    invocation (e.g. a device actuation)."""
    tracer = get_tracer()
    with tracer.start_as_current_span("execute_tool") as span:
        span.set_attribute("gen_ai.operation.name", "execute_tool")
        span.set_attribute("gen_ai.tool.name", tool_name)
        for k, v in extra_attrs.items():
            span.set_attribute(k, v)
        yield span
