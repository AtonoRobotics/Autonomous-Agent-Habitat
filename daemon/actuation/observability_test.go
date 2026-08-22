package actuation

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
)

func TestExecuteTracedRecordsSuccessSpan(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action
		(id, device_id, name, reversible, forward_template, read_state_template, inverse_template, verified_at)
		VALUES ($1, 'vent-actuator', 'set_open_pct', 1, $2, $3, $4, iso8601_now())`,
		"vent-actuator.set_open_pct",
		`{"shell_template": "vent-ctl set-open-pct {{open_pct}}"}`,
		`{"shell_template": "vent-ctl get-open-pct"}`,
		`{"shell_template": "vent-ctl set-open-pct {{prior}}"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{responses: map[string]string{
		"vent-ctl get-open-pct":    "40",
		"vent-ctl set-open-pct 60": "ok",
	}}
	gate := interlocks.New(db)

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	result, err := ExecuteTraced(ctx, tp, db, act, gate, "vent-actuator.set_open_pct", Command{
		Params: map[string]string{"open_pct": "60"},
	}, nil)
	if err != nil {
		t.Fatalf("ExecuteTraced: %v", err)
	}
	if result != "ok" {
		t.Fatalf("expected 'ok', got %q", result)
	}
	tp.ForceFlush(ctx)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	span := spans[0]
	if span.Name != "execute_tool" {
		t.Fatalf("expected span name 'execute_tool', got %q", span.Name)
	}
	attrs := attrMap(span.Attributes)
	if attrs["gen_ai.operation.name"] != "execute_tool" {
		t.Fatalf("expected gen_ai.operation.name=execute_tool, got %v", attrs["gen_ai.operation.name"])
	}
	if attrs["gen_ai.tool.name"] != "device_action:vent-actuator.set_open_pct" {
		t.Fatalf("unexpected gen_ai.tool.name: %v", attrs["gen_ai.tool.name"])
	}
	if attrs["amh.actuation.result"] != "ok" {
		t.Fatalf("expected amh.actuation.result=ok, got %v", attrs["amh.actuation.result"])
	}
	if span.Status.Code == codes.Error {
		t.Fatalf("expected non-error status, got error: %s", span.Status.Description)
	}
}

func TestExecuteTracedRecordsErrorSpan(t *testing.T) {
	db := testDB(t)
	seedConnectorAndDevice(t, db)
	ctx := context.Background()

	_, err := db.Exec(`INSERT INTO device_action (id, device_id, name, reversible, forward_template)
		VALUES ('vent-actuator.dispense_ml', 'vent-actuator', 'dispense_ml', 0, $1)`,
		`{"shell_template": "dose {{ml}}ml"}`,
	)
	if err != nil {
		t.Fatalf("seed device_action: %v", err)
	}

	act := &fakeActuator{}
	gate := interlocks.New(db)

	exporter := tracetest.NewInMemoryExporter()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))

	_, err = ExecuteTraced(ctx, tp, db, act, gate, "vent-actuator.dispense_ml", Command{Params: map[string]string{"ml": "5"}}, nil)
	if err != ErrNoAutonomyPath {
		t.Fatalf("expected ErrNoAutonomyPath, got %v", err)
	}
	tp.ForceFlush(ctx)

	spans := exporter.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("expected 1 span, got %d", len(spans))
	}
	if spans[0].Status.Code != codes.Error {
		t.Fatalf("expected error status on a failed actuation, got %v", spans[0].Status.Code)
	}
}

func attrMap(attrs []attribute.KeyValue) map[string]string {
	m := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		m[string(kv.Key)] = kv.Value.AsString()
	}
	return m
}
