package a2a

import (
	"context"
	"database/sql"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func textMessage(text string) Message {
	return Message{MessageID: "msg-1", Role: RoleUser, Parts: []Part{{Text: text}}}
}

func TestCreateTaskFromMessage_NewGoal_Submitted(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("water the plants"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	if task.Status.State != TaskStateSubmitted {
		t.Fatalf("expected TASK_STATE_SUBMITTED for a freshly created goal, got %s", task.Status.State)
	}
	if task.ID == "" || task.ContextID != task.ID {
		t.Fatalf("expected a non-empty id and contextId == id, got %+v", task)
	}
}

func TestCreateTaskFromMessage_NoTextPart_Fails(t *testing.T) {
	s := NewStore(testDB(t))
	_, err := s.CreateTaskFromMessage(context.Background(), Message{MessageID: "msg-1", Role: RoleUser, Parts: []Part{{URL: "https://example.com/x.png"}}})
	if err != ErrNoTextContent {
		t.Fatalf("expected ErrNoTextContent, got %v", err)
	}
}

func TestCreateTaskFromMessage_ConcatenatesMultipleTextParts(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()
	msg := Message{MessageID: "msg-1", Role: RoleUser, Parts: []Part{{Text: "first"}, {Text: "second"}}}
	task, err := s.CreateTaskFromMessage(ctx, msg)
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}

	var text string
	if err := s.DB.QueryRowContext(ctx, `SELECT text FROM goal WHERE id = $1`, task.ID).Scan(&text); err != nil {
		t.Fatalf("query goal text: %v", err)
	}
	if text != "first\nsecond" {
		t.Fatalf("expected both text parts concatenated, got %q", text)
	}
}

func TestGetTask_ReflectsRealGoalStatus(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("do a thing"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}

	for status, want := range map[string]TaskState{
		"active": TaskStateWorking,
		"done":   TaskStateCompleted,
		"failed": TaskStateFailed,
	} {
		if _, err := s.DB.ExecContext(ctx, `UPDATE goal SET status = $1 WHERE id = $2`, status, task.ID); err != nil {
			t.Fatalf("update goal status to %s: %v", status, err)
		}
		got, err := s.GetTask(ctx, task.ID)
		if err != nil {
			t.Fatalf("GetTask: %v", err)
		}
		if got.Status.State != want {
			t.Fatalf("goal status %q: expected %s, got %s", status, want, got.Status.State)
		}
	}
}

func TestGetTask_DoneGoal_CarriesStatusMessage(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("do a thing"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE goal SET status = 'done', status_message = 'all done' WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("update goal: %v", err)
	}

	got, err := s.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if got.Status.Message == nil || got.Status.Message.Parts[0].Text != "all done" {
		t.Fatalf("expected the status message to carry the synthesized result, got %+v", got.Status.Message)
	}
}

func TestGetTask_UnknownID_ReturnsNotFound(t *testing.T) {
	s := NewStore(testDB(t))
	if _, err := s.GetTask(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestCancelTask_OpenGoal_Succeeds(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("do a thing"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	canceled, err := s.CancelTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("expected TASK_STATE_CANCELED, got %s", canceled.Status.State)
	}
}

func TestCancelTask_AlreadyTerminal_Fails(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("do a thing"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE goal SET status = 'done' WHERE id = $1`, task.ID); err != nil {
		t.Fatalf("update goal: %v", err)
	}
	if _, err := s.CancelTask(ctx, task.ID); err != ErrNotCancelable {
		t.Fatalf("expected ErrNotCancelable for an already-terminal goal, got %v", err)
	}
}

func TestCancelTask_UnknownID_ReturnsNotFound(t *testing.T) {
	s := NewStore(testDB(t))
	if _, err := s.CancelTask(context.Background(), "does-not-exist"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

func TestListTasks_FiltersByStatus(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	open, err := s.CreateTaskFromMessage(ctx, textMessage("open one"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	done, err := s.CreateTaskFromMessage(ctx, textMessage("done one"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	if _, err := s.DB.ExecContext(ctx, `UPDATE goal SET status = 'done' WHERE id = $1`, done.ID); err != nil {
		t.Fatalf("update goal: %v", err)
	}

	result, err := s.ListTasks(ctx, ListFilter{Status: TaskStateSubmitted})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != open.ID {
		t.Fatalf("expected only the open goal, got %+v", result.Tasks)
	}
	if result.TotalSize != 2 {
		t.Fatalf("expected total_size 2 across all goals regardless of filter, got %d", result.TotalSize)
	}
}

func TestListTasks_FiltersByContextID(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	task, err := s.CreateTaskFromMessage(ctx, textMessage("do a thing"))
	if err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}
	if _, err := s.CreateTaskFromMessage(ctx, textMessage("another thing")); err != nil {
		t.Fatalf("CreateTaskFromMessage: %v", err)
	}

	result, err := s.ListTasks(ctx, ListFilter{ContextID: task.ID})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(result.Tasks) != 1 || result.Tasks[0].ID != task.ID {
		t.Fatalf("expected exactly the one matching contextId, got %+v", result.Tasks)
	}
}

func TestListTasks_Pagination(t *testing.T) {
	s := NewStore(testDB(t))
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		if _, err := s.CreateTaskFromMessage(ctx, textMessage("goal")); err != nil {
			t.Fatalf("CreateTaskFromMessage: %v", err)
		}
	}

	page1, err := s.ListTasks(ctx, ListFilter{PageSize: 2})
	if err != nil {
		t.Fatalf("ListTasks page 1: %v", err)
	}
	if len(page1.Tasks) != 2 || page1.NextPageToken == "" {
		t.Fatalf("expected a full first page with a next_page_token, got %+v", page1)
	}

	page2, err := s.ListTasks(ctx, ListFilter{PageSize: 2, PageToken: page1.NextPageToken})
	if err != nil {
		t.Fatalf("ListTasks page 2: %v", err)
	}
	if len(page2.Tasks) != 1 || page2.NextPageToken != "" {
		t.Fatalf("expected exactly the remaining goal with no further page, got %+v", page2)
	}
}
