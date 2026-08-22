package api

import (
	"encoding/json"
	"io"
	"net/http"
	"testing"
)

func TestBackupThenRestore_RoundTripsOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)

	c := generateCandidate(t, ts, "prompt")

	backup := postJSON(t, ts.URL+"/v1/backup", testOperatorToken, nil)
	defer backup.Body.Close()
	if backup.StatusCode != http.StatusOK {
		t.Fatalf("backup: expected 200, got %d", backup.StatusCode)
	}
	snapshot, err := io.ReadAll(backup.Body)
	if err != nil {
		t.Fatalf("read backup body: %v", err)
	}
	if len(snapshot) == 0 {
		t.Fatalf("expected a non-empty backup body")
	}

	// Mutate state after the backup so restore has something real to undo.
	generateCandidate(t, ts, "skill")
	before := getJSON(t, ts.URL+"/v1/selfimprove/candidates", testAgentToken)
	var beforeList []candidateResponse
	json.NewDecoder(before.Body).Decode(&beforeList)
	before.Body.Close()
	if len(beforeList) != 2 {
		t.Fatalf("expected 2 candidates before restore, got %d", len(beforeList))
	}

	restore := postJSON(t, ts.URL+"/v1/restore", testOperatorToken, snapshot)
	defer restore.Body.Close()
	if restore.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(restore.Body)
		t.Fatalf("restore: expected 200, got %d: %s", restore.StatusCode, body)
	}

	after := getJSON(t, ts.URL+"/v1/selfimprove/candidates", testAgentToken)
	defer after.Body.Close()
	var afterList []candidateResponse
	json.NewDecoder(after.Body).Decode(&afterList)
	if len(afterList) != 1 || afterList[0].ID != c.ID {
		t.Fatalf("expected only the pre-backup candidate to survive restore, got %+v", afterList)
	}
}

func TestBackup_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	agentAttempt := postJSON(t, ts.URL+"/v1/backup", testAgentToken, nil)
	defer agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token backing up, got %d", agentAttempt.StatusCode)
	}
}

func TestRestore_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	agentAttempt := postJSON(t, ts.URL+"/v1/restore", testAgentToken, []byte("irrelevant"))
	defer agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token restoring, got %d", agentAttempt.StatusCode)
	}
}

func TestRestore_RejectsCorruptSnapshot(t *testing.T) {
	ts := newTestServer(t, false)
	resp := postJSON(t, ts.URL+"/v1/restore", testOperatorToken, []byte("not a real pg_dump archive"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a corrupt snapshot, got %d", resp.StatusCode)
	}
}

