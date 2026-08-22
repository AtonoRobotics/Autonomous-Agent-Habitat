// Package actuation is the device-actuation kernel: the Go-side
// implementation of Artifact F's actuate_device / auto_reverse_effect
// durable steps. It is the single place where reversibility (§14.6),
// earned autonomy (§14.7), and the ApprovalGate (§12) meet the connector
// layer for physical device actuation.
//
// This logic lives directly in the Go daemon, which owns device I/O per
// Artifact A, and is called over HTTP by the Python agent layer
// (daemon/api, agents/workflows/actuate.py) rather than through a
// DBOS<->daemon RPC bridge — no such bridge (contracts/proto) exists;
// HTTP is the actual transport. Per the v10 core/extension boundary
// (docs/AMH-SPECIFICATION.md §8, §13, §16), this entire package —
// physical device actuation — belongs in a separate Physical AI
// extension, not AMH core; it has not yet been moved there.
package actuation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/observability"
)

// Actuator is the minimal shape a connector must satisfy to be driven by
// the actuation kernel — implemented by daemon/connectors/ssh.Connector
// (and, later, WinRM/MQTT/OPC-UA connectors).
type Actuator interface {
	RunShell(ctx context.Context, command string) (string, error)
}

// Command carries the rendered shell command(s) for one actuation. ReadState
// is only used (and only required) when the target DeviceAction is
// reversible and verified — it supplies the prior value the inverse is
// built from.
type Command struct {
	Forward   string
	ReadState string
}

type deviceAction struct {
	id              string
	reversible      bool
	inverseTemplate sql.NullString
	verifiedAt      sql.NullString
}

// ErrNoAutonomyPath is returned when a DeviceAction has no verified
// inverse, no approved SafetyCase, and no approval ticket was supplied —
// there is no legal way to execute it.
var ErrNoAutonomyPath = errors.New("actuation: no verified inverse, no approved SafetyCase, and no approval ticket")

// ExecuteTraced wraps Execute with a §13 tool-call span (§13,
// gen_ai.operation.name=execute_tool), recording the outcome (success,
// or the error) as span status — the daemon-side counterpart to the
// Python agent layer's tool_call_span around the amh-actuate CLI call.
// Execute itself stays untouched and independently testable; this is an
// additive wrapper, not a rewrite of the tested autonomy-path logic.
func ExecuteTraced(ctx context.Context, tp oteltrace.TracerProvider, db *sql.DB, act Actuator, gate *interlocks.Gate, deviceActionID string, cmd Command, ticket *interlocks.Ticket) (string, error) {
	ctx, span := observability.ToolCallSpan(ctx, tp, "device_action:"+deviceActionID)
	defer span.End()

	result, err := Execute(ctx, db, act, gate, deviceActionID, cmd, ticket)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		return "", err
	}
	span.SetAttributes(attribute.String("amh.actuation.result", result))
	return result, nil
}

// Execute runs one device actuation, choosing its autonomy path in the
// same order as Artifact F:
//  1. verified inverse -> autonomous, inverse recorded, no gate
//  2. approved & unrevoked SafetyCase -> autonomous, monitored, no gate
//  3. otherwise -> the supplied ticket must already be approved
func Execute(ctx context.Context, db *sql.DB, act Actuator, gate *interlocks.Gate, deviceActionID string, cmd Command, ticket *interlocks.Ticket) (string, error) {
	da, err := loadDeviceAction(ctx, db, deviceActionID)
	if err != nil {
		return "", err
	}

	if da.reversible && da.verifiedAt.Valid {
		if cmd.ReadState == "" {
			return "", fmt.Errorf("actuation: %s is reversible+verified but no ReadState command was supplied", deviceActionID)
		}
		priorState, err := act.RunShell(ctx, cmd.ReadState)
		if err != nil {
			return "", fmt.Errorf("actuation: read prior state: %w", err)
		}
		result, err := act.RunShell(ctx, cmd.Forward)
		if err != nil {
			return "", fmt.Errorf("actuation: invoke: %w", err)
		}
		inverseShell, err := renderInverse(da.inverseTemplate.String, priorState)
		if err != nil {
			return "", fmt.Errorf("actuation: render inverse: %w", err)
		}
		if err := recordEffect(ctx, db, da.id, cmd.Forward, &inverseShell, "success"); err != nil {
			return "", err
		}
		return result, nil
	}

	approved, err := hasApprovedSafetyCase(ctx, db, deviceActionID)
	if err != nil {
		return "", err
	}
	if approved {
		result, err := act.RunShell(ctx, cmd.Forward)
		if err != nil {
			return "", fmt.Errorf("actuation: invoke: %w", err)
		}
		if err := recordEffect(ctx, db, da.id, cmd.Forward, nil, "success"); err != nil {
			return "", err
		}
		return result, nil
	}

	if ticket == nil {
		return "", ErrNoAutonomyPath
	}
	if err := gate.Enforce(ctx, *ticket); err != nil {
		return "", fmt.Errorf("actuation: %w", err)
	}
	result, err := act.RunShell(ctx, cmd.Forward)
	if err != nil {
		return "", fmt.Errorf("actuation: invoke: %w", err)
	}
	if err := recordEffect(ctx, db, da.id, cmd.Forward, nil, "success"); err != nil {
		return "", err
	}
	return result, nil
}

// AutoReverse runs the recorded inverse for a device_effect — the
// self-healing path (§11): a fault detected post-actuation on a reversible
// action is undone automatically, no human wait. It is only callable when
// the effect has a recorded inverse; effects with inverse_payload = NULL
// (no verified inverse, or a SafetyCase-approved irreversible action) must
// escalate to the ApprovalGate instead, per Artifact F.
func AutoReverse(ctx context.Context, db *sql.DB, act Actuator, effectID string) error {
	var deviceActionID string
	var inversePayload sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT device_action_id, inverse_payload FROM device_effect WHERE id = ?`, effectID,
	).Scan(&deviceActionID, &inversePayload)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("actuation: effect %s not found", effectID)
	}
	if err != nil {
		return fmt.Errorf("actuation: load effect: %w", err)
	}
	if !inversePayload.Valid {
		return fmt.Errorf("actuation: effect %s has no recorded inverse; escalate to ApprovalGate instead", effectID)
	}

	var payload struct {
		Shell string `json:"shell"`
	}
	if err := json.Unmarshal([]byte(inversePayload.String), &payload); err != nil {
		return fmt.Errorf("actuation: parse inverse_payload: %w", err)
	}

	if _, err := act.RunShell(ctx, payload.Shell); err != nil {
		if _, markErr := db.ExecContext(ctx,
			`UPDATE device_effect SET outcome = 'fault_unreversed' WHERE id = ?`, effectID,
		); markErr != nil {
			return fmt.Errorf("actuation: reverse failed (%v) and failed to mark fault_unreversed: %w", err, markErr)
		}
		return fmt.Errorf("actuation: reverse failed: %w", err)
	}

	_, err = db.ExecContext(ctx,
		`UPDATE device_effect SET reversed_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), outcome = 'fault_reversed' WHERE id = ?`,
		effectID,
	)
	if err != nil {
		return fmt.Errorf("actuation: mark reversed: %w", err)
	}
	return nil
}

func loadDeviceAction(ctx context.Context, db *sql.DB, id string) (*deviceAction, error) {
	da := &deviceAction{id: id}
	var reversible int
	err := db.QueryRowContext(ctx,
		`SELECT reversible, inverse_template, verified_at FROM device_action WHERE id = ?`, id,
	).Scan(&reversible, &da.inverseTemplate, &da.verifiedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("actuation: device_action %s not found", id)
	}
	if err != nil {
		return nil, fmt.Errorf("actuation: load device_action %s: %w", id, err)
	}
	da.reversible = reversible != 0
	return da, nil
}

// hasApprovedSafetyCase enforces §14.7's evidence floor explicitly:
// independent_review MUST be true for any risk_class above 'low'. Today
// every case that reaches approved_at=NOT NULL also has
// independent_review=1 (daemon/safetycase.Registry.Approve sets both
// together, for every risk_class, not only the ones the floor requires
// it for) — but this query checks the rule directly rather than relying
// on that invariant holding forever as safetycase.Approve evolves. A
// case that somehow reached approved_at without independent_review
// (data migrated from elsewhere, a code path that doesn't go through
// Approve) must not silently grant autonomy here.
func hasApprovedSafetyCase(ctx context.Context, db *sql.DB, subjectID string) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM safety_case
		 WHERE subject_id = ? AND subject_type = 'device_action'
		   AND approved_at IS NOT NULL AND revoked_at IS NULL
		   AND (risk_class = 'low' OR independent_review = 1)`,
		subjectID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("actuation: query safety_case: %w", err)
	}
	return n > 0, nil
}

// renderInverse substitutes {{prior}} in the stored template's "shell_template"
// JSON field with the observed prior state. One substitution variable is
// what every currently-supported reversible device_action needs; a
// device type whose inverse depends on more than the single prior-state
// read would need this template grammar extended.
func renderInverse(templateJSON, priorState string) (string, error) {
	if templateJSON == "" {
		return "", fmt.Errorf("device_action has reversible=true but no inverse_template")
	}
	var tmpl struct {
		ShellTemplate string `json:"shell_template"`
	}
	if err := json.Unmarshal([]byte(templateJSON), &tmpl); err != nil {
		return "", fmt.Errorf("parse inverse_template: %w", err)
	}
	if tmpl.ShellTemplate == "" {
		return "", fmt.Errorf("inverse_template missing shell_template")
	}
	return strings.ReplaceAll(tmpl.ShellTemplate, "{{prior}}", priorState), nil
}

func recordEffect(ctx context.Context, db *sql.DB, deviceActionID, forwardShell string, inverseShell *string, outcome string) error {
	forwardPayload, err := json.Marshal(map[string]string{"shell": forwardShell})
	if err != nil {
		return fmt.Errorf("actuation: marshal forward_payload: %w", err)
	}
	var inversePayload any
	if inverseShell != nil {
		b, err := json.Marshal(map[string]string{"shell": *inverseShell})
		if err != nil {
			return fmt.Errorf("actuation: marshal inverse_payload: %w", err)
		}
		inversePayload = string(b)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO device_effect (id, device_action_id, forward_payload, inverse_payload, outcome)
		 VALUES (?, ?, ?, ?, ?)`,
		uuid.NewString(), deviceActionID, string(forwardPayload), inversePayload, outcome,
	)
	if err != nil {
		return fmt.Errorf("actuation: insert device_effect: %w", err)
	}
	return nil
}
