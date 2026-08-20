// Package observability wires OpenTelemetry tracing for the Go daemon,
// per docs/AMH-SPECIFICATION.md §13: spans for device actuations and
// connector calls, using GenAI-adjacent attribute naming
// (gen_ai.tool.name etc.) so a trace spans both the Python agent layer
// and the Go daemon coherently under one convention.
//
// V0 default: no exporter configured unless one is explicitly passed to
// Init — spans are still created and recorded on the tracer provider
// (visible to an in-memory test exporter), but nothing ships anywhere
// unless wired to a real exporter (OTLP, stdout, etc.), matching
// .env.example's opt-in OTEL_EXPORTER_OTLP_ENDPOINT.
package observability

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "amh.daemon"

// Init installs and returns a TracerProvider. Pass a SpanExporter to
// actually export spans (e.g. a test exporter, or an OTLP exporter in
// production); pass nil for a provider that records spans without
// exporting them anywhere.
func Init(exporter sdktrace.SpanExporter) *sdktrace.TracerProvider {
	opts := []sdktrace.TracerProviderOption{}
	if exporter != nil {
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	return sdktrace.NewTracerProvider(opts...)
}

func Tracer(tp trace.TracerProvider) trace.Tracer {
	return tp.Tracer(tracerName)
}

// ToolCallSpan starts a span for one tool/connector invocation — the Go
// daemon's equivalent of the Python agent layer's tool_call_span
// (agents/context/observability.py), using the same
// gen_ai.operation.name=execute_tool convention so traces read
// consistently across both languages.
func ToolCallSpan(ctx context.Context, tp trace.TracerProvider, toolName string) (context.Context, trace.Span) {
	ctx, span := Tracer(tp).Start(ctx, "execute_tool")
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "execute_tool"),
		attribute.String("gen_ai.tool.name", toolName),
	)
	return ctx, span
}
