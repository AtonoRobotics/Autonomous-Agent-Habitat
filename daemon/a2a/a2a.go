// Package a2a is AMH's A2A 1.0 external-agent-interoperability adapter
// (docs/AMH-SPECIFICATION.md §2.1: "A2A interoperability at the external
// agent boundary"; §9: "A2A 1.0 is the external agent interoperability
// baseline"). It exposes AMH's own goal-pursuit capability to external
// A2A clients — the daemon owns this the same way it owns every other
// local/external transport (decision 2), the same role daemon/mcp plays
// for MCP tool/resource interoperability.
//
// Wire types and field names here are verified against the canonical A2A
// specification (github.com/a2aproject/A2A, specification/a2a.proto) at
// the commit this package was written against, not assumed: Task,
// TaskState, Message, Part, AgentCard, and every request/response type
// mirror that proto's message and field names one-to-one (translated to
// the camelCase JSON protojson would produce, since that's the actual
// wire format a real A2A client sends/expects).
//
// # Transport
//
// A2A officially supports three protocol bindings for the same logical
// service: JSON-RPC, gRPC, and HTTP+JSON. This package implements
// HTTP+JSON — the binding whose method/path mapping is given directly and
// unambiguously by the proto's own google.api.http annotations (POST
// /message:send, GET /tasks/{id}, POST /tasks/{id}:cancel, ...), rather
// than JSON-RPC, whose exact method-name-string convention for this proto
// isn't specified by the .proto file itself and would otherwise have to
// be assumed rather than verified.
//
// # Scope
//
// Implements: Agent Card discovery, SendMessage (creating a new task
// only — continuing an existing task's conversation is not implemented),
// GetTask, ListTasks, CancelTask. Deliberately NOT implemented:
// SendStreamingMessage/SubscribeToTask (SSE streaming), push notification
// config CRUD, GetExtendedAgentCard, and multi-tenant `tenant` routing —
// real, separable follow-up work, not attempted here, the same
// "implemented some of it for real rather than all of it partially"
// discipline daemon/mcp's own doc comment already documents for MCP's
// protocol-era choice.
//
// # What SendMessage actually does — and does not — do
//
// SendMessage durably records a real AMH Goal (store/migrations,
// `goal` table) — the same row agents/workflows/goal.py's pursue_goal
// operates on. What it does NOT do is start pursue_goal: nothing in this
// codebase yet implements "deterministic delivery of external triggers
// into DBOS using stable idempotency keys," which §3.1 lists as its own,
// separate amh-daemon responsibility. Until that dispatch mechanism
// exists (for A2A-originated goals or any other goal), a goal created
// here durably sits in TASK_STATE_SUBMITTED until something — today,
// only a direct call to pursue_goal, exactly as every existing test
// already does — works it. GetTask/ListTasks report exactly the AMH
// goal's real, current state; they do not fabricate progress.
package a2a

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var (
	ErrNotFound      = errors.New("a2a: task not found")
	ErrNoTextContent = errors.New("a2a: message has no text part")
	ErrNotCancelable = errors.New("a2a: task is already in a terminal state")
)

// TaskState mirrors the A2A TaskState enum's JSON string values exactly
// (protojson serializes a proto3 enum as its name, not its integer).
type TaskState string

const (
	TaskStateSubmitted     TaskState = "TASK_STATE_SUBMITTED"
	TaskStateWorking       TaskState = "TASK_STATE_WORKING"
	TaskStateCompleted     TaskState = "TASK_STATE_COMPLETED"
	TaskStateFailed        TaskState = "TASK_STATE_FAILED"
	TaskStateCanceled      TaskState = "TASK_STATE_CANCELED"
	TaskStateRejected      TaskState = "TASK_STATE_REJECTED"
	TaskStateInputRequired TaskState = "TASK_STATE_INPUT_REQUIRED"
	TaskStateAuthRequired  TaskState = "TASK_STATE_AUTH_REQUIRED"
)

// goalStateToTaskState translates AMH's own goal.status values into A2A's
// TaskState — the §9 "translate external lifecycle... into AMH canonical
// operation states" requirement applied in the direction this adapter
// actually needs (AMH state -> external wire state, for reporting).
// AMH's goal.status has no equivalent of INPUT_REQUIRED/AUTH_REQUIRED
// (pursue_goal never pauses for external input mid-run today), so those
// two TaskState values are never produced by this function — a real,
// current limitation, not an oversight.
func goalStateToTaskState(status string) TaskState {
	switch status {
	case "open":
		return TaskStateSubmitted
	case "active":
		return TaskStateWorking
	case "done":
		return TaskStateCompleted
	case "failed":
		return TaskStateFailed
	case "canceled":
		return TaskStateCanceled
	default:
		return TaskStateSubmitted
	}
}

func isTerminal(status string) bool {
	switch status {
	case "done", "failed", "canceled":
		return true
	default:
		return false
	}
}

// Role mirrors A2A's Role enum JSON string values.
type Role string

const (
	RoleUser  Role = "ROLE_USER"
	RoleAgent Role = "ROLE_AGENT"
)

// Part mirrors A2A's Part message — a oneof in proto, represented here as
// a struct with only the relevant field set, matching protojson's
// flattened-oneof JSON encoding (the oneof's chosen field appears
// directly at the top level, not wrapped).
type Part struct {
	Text      string          `json:"text,omitempty"`
	Raw       []byte          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Filename  string          `json:"filename,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
}

// Message mirrors A2A's Message message.
type Message struct {
	MessageID        string         `json:"messageId"`
	ContextID        string         `json:"contextId,omitempty"`
	TaskID           string         `json:"taskId,omitempty"`
	Role             Role           `json:"role"`
	Parts            []Part         `json:"parts"`
	Metadata         map[string]any `json:"metadata,omitempty"`
	Extensions       []string       `json:"extensions,omitempty"`
	ReferenceTaskIDs []string       `json:"referenceTaskIds,omitempty"`
}

// textContent concatenates every text part's content — the only part
// type this adapter's one skill (goal pursuit) actually consumes. Returns
// ErrNoTextContent if the message carries no text part at all (a
// file/data-only message has nothing for pursue_goal's goal_text to be).
func (m Message) textContent() (string, error) {
	var text string
	for _, p := range m.Parts {
		if p.Text != "" {
			if text != "" {
				text += "\n"
			}
			text += p.Text
		}
	}
	if text == "" {
		return "", ErrNoTextContent
	}
	return text, nil
}

// TaskStatus mirrors A2A's TaskStatus message.
type TaskStatus struct {
	State     TaskState `json:"state"`
	Message   *Message  `json:"message,omitempty"`
	Timestamp string    `json:"timestamp,omitempty"`
}

// Task mirrors A2A's Task message. Artifacts and History are always
// empty — see the package doc comment's Scope section; AMH's goal
// ontology has no per-goal message-history store this adapter projects
// from, and no artifact linkage from a Goal (only from AMH's own,
// differently-scoped Task entity).
type Task struct {
	ID        string         `json:"id"`
	ContextID string         `json:"contextId"`
	Status    TaskStatus     `json:"status"`
	Artifacts []any          `json:"artifacts"`
	History   []Message      `json:"history"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Store translates between AMH's durable `goal` table and A2A's Task
// wire model — see the package doc comment for what SendMessage does and
// does not do.
type Store struct {
	DB *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{DB: db}
}

// iso8601Format matches store/migrations' iso8601_now() output — see
// daemon/policy's identical constant and doc comment for why this
// matters (string-comparable/sortable against every other timestamp
// column in this schema).
const iso8601Format = "2006-01-02T15:04:05.000Z"

func iso8601(t time.Time) string { return t.UTC().Format(iso8601Format) }

// CreateTaskFromMessage is SendMessage's implementation for the
// new-task case (message.TaskID empty) — continuing an existing task's
// conversation is out of scope (see package doc comment).
func (s *Store) CreateTaskFromMessage(ctx context.Context, msg Message) (*Task, error) {
	text, err := msg.textContent()
	if err != nil {
		return nil, err
	}
	goalID := uuid.NewString()
	if _, err := s.DB.ExecContext(ctx, `INSERT INTO goal (id, text, status) VALUES ($1, $2, 'open')`, goalID, text); err != nil {
		return nil, fmt.Errorf("a2a: insert goal: %w", err)
	}
	return s.GetTask(ctx, goalID)
}

// GetTask reads a goal's current, real state and reports it as a Task —
// never a cached or optimistic projection.
func (s *Store) GetTask(ctx context.Context, id string) (*Task, error) {
	var status string
	var statusMessage sql.NullString
	var createdAt string
	err := s.DB.QueryRowContext(ctx, `SELECT status, status_message, created_at FROM goal WHERE id = $1`, id).
		Scan(&status, &statusMessage, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("a2a: get goal: %w", err)
	}
	return taskFromGoal(id, status, statusMessage, createdAt), nil
}

func taskFromGoal(id, status string, statusMessage sql.NullString, timestamp string) *Task {
	taskStatus := TaskStatus{State: goalStateToTaskState(status), Timestamp: timestamp}
	if statusMessage.Valid && statusMessage.String != "" {
		taskStatus.Message = &Message{
			MessageID: uuid.NewString(),
			Role:      RoleAgent,
			Parts:     []Part{{Text: statusMessage.String}},
		}
	}
	return &Task{
		ID: id, ContextID: id, Status: taskStatus,
		Artifacts: []any{}, History: []Message{},
	}
}

// ListFilter narrows ListTasks — every field is optional (its zero value
// means "no filter on this dimension"), matching ListTasksRequest.
type ListFilter struct {
	ContextID string
	Status    TaskState
	PageSize  int
	PageToken string
}

// ListResult is ListTasks' return shape — mirrors ListTasksResponse.
type ListResult struct {
	Tasks         []Task
	NextPageToken string
	TotalSize     int
}

const defaultPageSize = 50

// ListTasks lists goals oldest first, translated to Task. PageToken is
// the created_at of the last row of the previous page — a stable cursor
// over insertion order, never an offset (which would skew under
// concurrent goal creation). created_at alone, not a (created_at, id)
// compound cursor, can in principle skip or repeat a row for two goals
// created in the same millisecond; accepted as a real but vanishingly
// unlikely edge case for this adapter's expected call volume.
func (s *Store) ListTasks(ctx context.Context, f ListFilter) (*ListResult, error) {
	pageSize := f.PageSize
	if pageSize <= 0 || pageSize > 100 {
		pageSize = defaultPageSize
	}

	var total int
	if err := s.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM goal`).Scan(&total); err != nil {
		return nil, fmt.Errorf("a2a: count goals: %w", err)
	}

	// f.ContextID narrows to a single goal, since AMH has no grouping
	// broader than one goal per context in this adapter (context_id ==
	// task_id == goal.id — see the package doc comment).
	if f.ContextID != "" {
		task, err := s.GetTask(ctx, f.ContextID)
		if errors.Is(err, ErrNotFound) {
			return &ListResult{Tasks: []Task{}, TotalSize: total}, nil
		}
		if err != nil {
			return nil, err
		}
		if f.Status != "" && task.Status.State != f.Status {
			return &ListResult{Tasks: []Task{}, TotalSize: total}, nil
		}
		return &ListResult{Tasks: []Task{*task}, TotalSize: total}, nil
	}

	query := `SELECT id, status, status_message, created_at FROM goal WHERE ($1 = '' OR created_at > $1)`
	args := []any{f.PageToken}
	if f.Status != "" {
		amhStatus := taskStateToGoalState(f.Status)
		query += ` AND status = $2 ORDER BY created_at ASC LIMIT $3`
		args = append(args, amhStatus, pageSize+1)
	} else {
		query += ` ORDER BY created_at ASC LIMIT $2`
		args = append(args, pageSize+1)
	}

	rows, err := s.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("a2a: list goals: %w", err)
	}
	defer rows.Close()

	var tasks []Task
	var createdAts []string
	for rows.Next() {
		var id, status, createdAt string
		var statusMessage sql.NullString
		if err := rows.Scan(&id, &status, &statusMessage, &createdAt); err != nil {
			return nil, fmt.Errorf("a2a: scan goal: %w", err)
		}
		tasks = append(tasks, *taskFromGoal(id, status, statusMessage, createdAt))
		createdAts = append(createdAts, createdAt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := &ListResult{TotalSize: total}
	if len(tasks) > pageSize {
		result.NextPageToken = createdAts[pageSize-1]
		tasks = tasks[:pageSize]
	}
	result.Tasks = tasks
	if result.Tasks == nil {
		result.Tasks = []Task{}
	}
	return result, nil
}

func taskStateToGoalState(s TaskState) string {
	switch s {
	case TaskStateSubmitted:
		return "open"
	case TaskStateWorking:
		return "active"
	case TaskStateCompleted:
		return "done"
	case TaskStateFailed:
		return "failed"
	case TaskStateCanceled:
		return "canceled"
	default:
		return "open"
	}
}

// CancelTask marks a non-terminal goal canceled. See the package doc
// comment: for a goal already 'active', this updates only the AMH-side
// record — there is no hook from Go into DBOS's own workflow-cancellation
// API, so an in-flight pursue_goal run is not actually interrupted by
// this call today.
func (s *Store) CancelTask(ctx context.Context, id string) (*Task, error) {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("a2a: begin cancel tx: %w", err)
	}
	defer tx.Rollback()

	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM goal WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("a2a: query goal for cancel: %w", err)
	}
	if isTerminal(status) {
		return nil, ErrNotCancelable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE goal SET status = 'canceled', status_message = $1 WHERE id = $2`,
		"canceled via A2A CancelTask", id); err != nil {
		return nil, fmt.Errorf("a2a: cancel goal: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("a2a: commit cancel: %w", err)
	}
	return s.GetTask(ctx, id)
}
