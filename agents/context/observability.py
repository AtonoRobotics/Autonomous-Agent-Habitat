"""OpenTelemetry tracing for the agent layer, per
docs/AMH-SPECIFICATION.md §13: spans for agent runs, tool calls, and
sub-agent spawns, using OTel's GenAI semantic conventions
(gen_ai.operation.name, gen_ai.agent.id, etc.) rather than ad-hoc span
names, so traces are queryable the same way across any OTel-compatible
backend.

V0 default exporter: none configured by init_tracing() unless an OTLP
endpoint is passed — spans are created and recorded on the current
provider either way (visible to an in-memory exporter in tests), but
nothing is shipped over the network unless OTEL_EXPORTER_OTLP_ENDPOINT is
set, matching .env.example's opt-in OTLP config.
"""

from __future__ import annotations

from contextlib import contextmanager
from typing import Iterator

from opentelemetry import trace
from opentelemetry.sdk.resources import SERVICE_NAME, Resource
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor, SpanExporter

_TRACER_NAME = "amh.agents"


def init_tracing(service_name: str = "amh-agents", exporter: SpanExporter | None = None) -> TracerProvider:
    """Installs a TracerProvider as the global default. Pass an explicit
    exporter (e.g. an in-memory one for tests, or an OTLP exporter for
    production) — with none given, spans are recorded but never exported,
    which is a valid no-op default for local V0 runs.

    Uses SimpleSpanProcessor (export-on-end, no batching) — V0's span
    volume is modest and this keeps test assertions straightforward
    (spans are visible immediately, no explicit flush needed). Swap to
    BatchSpanProcessor if a real OTLP collector's throughput demands it."""
    provider = TracerProvider(resource=Resource.create({SERVICE_NAME: service_name}))
    if exporter is not None:
        provider.add_span_processor(SimpleSpanProcessor(exporter))
    trace.set_tracer_provider(provider)
    return provider


def get_tracer():
    return trace.get_tracer(_TRACER_NAME)


@contextmanager
def agent_run_span(agent_id: str, model: str | None = None) -> Iterator[trace.Span]:
    """gen_ai.operation.name=invoke_agent, per OTel's GenAI semantic
    conventions — wraps one agent run (pursue_goal or a sub-agent)."""
    tracer = get_tracer()
    with tracer.start_as_current_span("invoke_agent") as span:
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
