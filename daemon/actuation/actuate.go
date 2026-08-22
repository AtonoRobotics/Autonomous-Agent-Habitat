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
//
// Command provenance: a caller supplies only named parameter values
// (Command.Params), never shell text. The actual forward and read-state
// shell commands are rendered server-side from the target device_action's
// own forward_template/read_state_template — the same design
// inverse_template already used. This is deliberate, not incidental: it's
// what makes "verified reversible" and "ticket approved for this action"
// mean something concrete tied to what the daemon will actually execute,
// rather than trusting whatever shell text a caller happened to send in
// that particular request.
package actuation

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
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

// Command carries the caller-supplied parameter values for one actuation
// — never shell text. Values are substituted into the target
// device_action's own forward_template/read_state_template (see
// renderTemplate) after passing paramValuePattern, so a parameter can
// never inject shell metacharacters into the command the daemon actually
// runs; the template's author, not the caller, controls the command's
// structure.
type Command struct {
	Params map[string]string
}

// paramValuePattern is deliberately restrictive: letters, digits, and a
// small set of punctuation common to IDs, percentages, and paths — never
// shell metacharacters (;|&`$(){}<>'"\ or whitespace). A device_action
// that genuinely needs a richer parameter value is better served by a
// more specific template than by loosening this.
var paramValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.:/=-]*$`)

// ErrInvalidParam is returned when a caller-supplied parameter value
// fails paramValuePattern.
var ErrInvalidParam = errors.New("actuation: parameter value contains disallowed characters")

func validateParams(params map[string]string) error {
	for k, v := range params {
		if !paramValuePattern.MatchString(v) {
			return fmt.Errorf("%w: param %q", ErrInvalidParam, k)
		}
	}
	return nil
}

type deviceAction struct {
	id                string
	reversible        bool
	forwardTemplate   sql.NullString
	readStateTemplate sql.NullString
	inverseTemplate   sql.NullString
	verifiedAt        sql.NullString
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
//  3. otherwise -> the supplied ticket must already be approved for this
//     exact (deviceActionID, params) action, and is consumed on success
//
// Every path follows the same journal-first discipline: a 'pending'
// device_effect row is durably recorded BEFORE the forward command runs,
// then transitioned to 'success' or 'unknown' afterward. A journal-write
// failure therefore blocks the physical effect (fails closed) instead of
// following a physical effect that already happened with nothing to show
// for it — the gap PR #3's review found.
func Execute(ctx context.Context, db *sql.DB, act Actuator, gate *interlocks.Gate, deviceActionID string, cmd Command, ticket *interlocks.Ticket) (string, error) {
	if err := validateParams(cmd.Params); err != nil {
		return "", err
	}

	da, err := loadDeviceAction(ctx, db, deviceActionID)
	if err != nil {
		return "", err
	}
	forwardShell, err := renderTemplate(da.forwardTemplate.String, cmd.Params)
	if err != nil {
		return "", fmt.Errorf("actuation: render forward command: %w", err)
	}

	if da.reversible && da.verifiedAt.Valid {
		readStateShell, err := renderTemplate(da.readStateTemplate.String, cmd.Params)
		if err != nil {
			return "", fmt.Errorf("actuation: render read-state command: %w", err)
		}
		priorState, err := act.RunShell(ctx, readStateShell)
		if err != nil {
			return "", fmt.Errorf("actuation: read prior state: %w", err)
		}
		// Render and validate the inverse from priorState before invoking
		// the forward command — a malformed inverse_template must be
		// caught before the physical effect happens, not after.
		inverseShell, err := renderTemplate(da.inverseTemplate.String, map[string]string{"prior": priorState})
		if err != nil {
			return "", fmt.Errorf("actuation: render inverse: %w", err)
		}
		return runJournaled(ctx, db, act, da.id, forwardShell, &inverseShell)
	}

	approved, err := hasApprovedSafetyCase(ctx, db, deviceActionID)
	if err != nil {
		return "", err
	}
	if approved {
		return runJournaled(ctx, db, act, da.id, forwardShell, nil)
	}

	if ticket == nil {
		return "", ErrNoAutonomyPath
	}
	if err := gate.Enforce(ctx, *ticket, deviceActionID, cmd.Params); err != nil {
		return "", fmt.Errorf("actuation: %w", err)
	}
	return runJournaled(ctx, db, act, da.id, forwardShell, nil)
}

// runJournaled persists a 'pending' device_effect row, runs the forward
// command, and transitions that row to 'success' or 'unknown' — never
// leaving a physical effect that ran with no durable record of it, and
// never running the forward command if the pending row can't be written
// in the first place (see Execute's doc comment).
func runJournaled(ctx context.Context, db *sql.DB, act Actuator, deviceActionID, forwardShell string, inverseShell *string) (string, error) {
	effectID, err := recordPendingEffect(ctx, db, deviceActionID, forwardShell, inverseShell)
	if err != nil {
		return "", err
	}
	result, err := act.RunShell(ctx, forwardShell)
	if err != nil {
		// The command errored, but per the SSH exec channel's own
		// semantics that doesn't prove the device never received it —
		// record 'unknown', never silently drop back to 'pending' or
		// assume nothing happened.
		markEffectOutcome(ctx, db, effectID, "unknown")
		return "", fmt.Errorf("actuation: invoke: %w", err)
	}
	// Best-effort: the physical effect already genuinely succeeded, so a
	// failure updating this row to 'success' must not be reported to the
	// caller as an actuation failure (that's the exact bug being fixed) —
	// the row is left durably 'pending', which is itself a legitimate
	// signal for reconciliation to pick up, rather than lost entirely.
	markEffectOutcome(ctx, db, effectID, "success")
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
		`SELECT device_action_id, inverse_payload FROM device_effect WHERE id = $1`, effectID,
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
			`UPDATE device_effect SET outcome = 'fault_unreversed' WHERE id = $1`, effectID,
		); markErr != nil {
			return fmt.Errorf("actuation: reverse failed (%v) and failed to mark fault_unreversed: %w", err, markErr)
		}
		return fmt.Errorf("actuation: reverse failed: %w", err)
	}

	_, err = db.ExecContext(ctx,
		`UPDATE device_effect SET reversed_at = iso8601_now(), outcome = 'fault_reversed' WHERE id = $1`,
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
		`SELECT reversible, forward_template, read_state_template, inverse_template, verified_at FROM device_action WHERE id = $1`, id,
	).Scan(&reversible, &da.forwardTemplate, &da.readStateTemplate, &da.inverseTemplate, &da.verifiedAt)
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
		 WHERE subject_id = $1 AND subject_type = 'device_action'
		   AND approved_at IS NOT NULL AND revoked_at IS NULL
		   AND (risk_class = 'low' OR independent_review = 1)`,
		subjectID,
	).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("actuation: query safety_case: %w", err)
	}
	return n > 0, nil
}

// renderTemplate substitutes each {{key}} in templateJSON's
// "shell_template" field with vars[key]. Used for forward_template and
// read_state_template (vars = the caller's validated Command.Params) and
// for inverse_template (vars = {"prior": <observed prior state>}) — one
// substitution grammar for every template a device_action owns.
func renderTemplate(templateJSON string, vars map[string]string) (string, error) {
	if templateJSON == "" {
		return "", fmt.Errorf("template is not configured")
	}
	var tmpl struct {
		ShellTemplate string `json:"shell_template"`
	}
	if err := json.Unmarshal([]byte(templateJSON), &tmpl); err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}
	if tmpl.ShellTemplate == "" {
		return "", fmt.Errorf("template missing shell_template")
	}
	rendered := tmpl.ShellTemplate
	for k, v := range vars {
		rendered = strings.ReplaceAll(rendered, "{{"+k+"}}", v)
	}
	return rendered, nil
}

// recordPendingEffect durably records the intent to run forwardShell
// BEFORE it runs — see runJournaled and Execute's doc comment.
func recordPendingEffect(ctx context.Context, db *sql.DB, deviceActionID, forwardShell string, inverseShell *string) (string, error) {
	forwardPayload, err := json.Marshal(map[string]string{"shell": forwardShell})
	if err != nil {
		return "", fmt.Errorf("actuation: marshal forward_payload: %w", err)
	}
	var inversePayload any
	if inverseShell != nil {
		b, err := json.Marshal(map[string]string{"shell": *inverseShell})
		if err != nil {
			return "", fmt.Errorf("actuation: marshal inverse_payload: %w", err)
		}
		inversePayload = string(b)
	}
	id := uuid.NewString()
	_, err = db.ExecContext(ctx,
		`INSERT INTO device_effect (id, device_action_id, forward_payload, inverse_payload, outcome)
		 VALUES ($1, $2, $3, $4, 'pending')`,
		id, deviceActionID, string(forwardPayload), inversePayload,
	)
	if err != nil {
		return "", fmt.Errorf("actuation: insert pending device_effect: %w", err)
	}
	return id, nil
}

func markEffectOutcome(ctx context.Context, db *sql.DB, effectID, outcome string) {
	// Best-effort by design (see runJournaled): a failure here must not
	// turn into an actuation error for an already-successful physical
	// effect. The row is left at its last durable state either way.
	db.ExecContext(ctx, `UPDATE device_effect SET outcome = $1 WHERE id = $2`, outcome, effectID)
}
