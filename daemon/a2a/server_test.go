package a2a

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
)

const (
	testAgentToken    = "test-agent-token"
	testOperatorToken = "test-operator-token"
)

func testServer(t *testing.T, db *sql.DB) *httptest.Server {
	t.Helper()
	auth, err := authn.New(testAgentToken, testOperatorToken)
	if err != nil {
		t.Fatalf("authn.New: %v", err)
	}
	tp := sdktrace.NewTracerProvider()
	s := New("", "http://example.invalid/a2a", NewStore(db), tp, auth, nil)
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url, token string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func getJSON(t *testing.T, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func TestAgentCard_UnauthenticatedAndWellFormed(t *testing.T) {
	ts := testServer(t, testDB(t))

	resp := getJSON(t, ts.URL+agentCardPath, "")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with no token, got %d", resp.StatusCode)
	}
	var card AgentCard
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		t.Fatalf("decode agent card: %v", err)
	}
	if card.Name == "" || len(card.Skills) == 0 {
		t.Fatalf("expected a real name and at least one skill, got %+v", card)
	}
	if len(card.SupportedInterfaces) == 0 || card.SupportedInterfaces[0].ProtocolBinding != "HTTP+JSON" {
		t.Fatalf("expected an HTTP+JSON supported interface, got %+v", card.SupportedInterfaces)
	}
	if card.SecuritySchemes["bearer"].HTTPAuthSecurityScheme == nil {
		t.Fatalf("expected the card to declare its Bearer security scheme, got %+v", card.SecuritySchemes)
	}
}

func TestSendMessage_CreatesRealGoal_RejectsRequestsWithNoToken(t *testing.T) {
	ts := testServer(t, testDB(t))

	body, _ := json.Marshal(sendMessageRequest{Message: textMessage("water the plants")})
	unauth := postJSON(t, ts.URL+"/message:send", "", body)
	defer unauth.Body.Close()
	if unauth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 with no token, got %d", unauth.StatusCode)
	}

	resp := postJSON(t, ts.URL+"/message:send", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var decoded sendMessageResponse
	json.NewDecoder(resp.Body).Decode(&decoded)
	if decoded.Task == nil || decoded.Task.Status.State != TaskStateSubmitted {
		t.Fatalf("expected a submitted task, got %+v", decoded)
	}
}

func TestSendMessage_NoTextPart_Returns400(t *testing.T) {
	ts := testServer(t, testDB(t))
	body, _ := json.Marshal(sendMessageRequest{Message: Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{URL: "https://example.com"}}}})
	resp := postJSON(t, ts.URL+"/message:send", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestSendMessage_WithTaskID_Returns400(t *testing.T) {
	ts := testServer(t, testDB(t))
	msg := textMessage("continue please")
	msg.TaskID = "some-existing-task"
	body, _ := json.Marshal(sendMessageRequest{Message: msg})
	resp := postJSON(t, ts.URL+"/message:send", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an unsupported existing-task continuation, got %d", resp.StatusCode)
	}
}

func TestGetTask_OverHTTP_RoundTrips(t *testing.T) {
	ts := testServer(t, testDB(t))
	body, _ := json.Marshal(sendMessageRequest{Message: textMessage("water the plants")})
	sendResp := postJSON(t, ts.URL+"/message:send", testAgentToken, body)
	var sent sendMessageResponse
	json.NewDecoder(sendResp.Body).Decode(&sent)
	sendResp.Body.Close()

	get := getJSON(t, ts.URL+"/tasks/"+sent.Task.ID, testAgentToken)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", get.StatusCode)
	}
	var got Task
	json.NewDecoder(get.Body).Decode(&got)
	if got.ID != sent.Task.ID {
		t.Fatalf("expected the same task id, got %+v", got)
	}
}

func TestGetTask_UnknownID_Returns404(t *testing.T) {
	ts := testServer(t, testDB(t))
	get := getJSON(t, ts.URL+"/tasks/does-not-exist", testAgentToken)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", get.StatusCode)
	}
}

func TestCancelTask_OverHTTP(t *testing.T) {
	ts := testServer(t, testDB(t))
	body, _ := json.Marshal(sendMessageRequest{Message: textMessage("water the plants")})
	sendResp := postJSON(t, ts.URL+"/message:send", testAgentToken, body)
	var sent sendMessageResponse
	json.NewDecoder(sendResp.Body).Decode(&sent)
	sendResp.Body.Close()

	cancel := postJSON(t, ts.URL+"/tasks/"+sent.Task.ID+":cancel", testAgentToken, nil)
	defer cancel.Body.Close()
	if cancel.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", cancel.StatusCode)
	}
	var canceled Task
	json.NewDecoder(cancel.Body).Decode(&canceled)
	if canceled.Status.State != TaskStateCanceled {
		t.Fatalf("expected TASK_STATE_CANCELED, got %s", canceled.Status.State)
	}

	// A second cancel of the now-terminal task fails closed.
	again := postJSON(t, ts.URL+"/tasks/"+sent.Task.ID+":cancel", testAgentToken, nil)
	defer again.Body.Close()
	if again.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 canceling an already-canceled task, got %d", again.StatusCode)
	}
}

func TestListTasks_OverHTTP(t *testing.T) {
	ts := testServer(t, testDB(t))
	body, _ := json.Marshal(sendMessageRequest{Message: textMessage("water the plants")})
	postJSON(t, ts.URL+"/message:send", testAgentToken, body).Body.Close()

	list := getJSON(t, ts.URL+"/tasks", testAgentToken)
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", list.StatusCode)
	}
	var decoded struct {
		Tasks     []Task `json:"tasks"`
		TotalSize int    `json:"totalSize"`
	}
	json.NewDecoder(list.Body).Decode(&decoded)
	if len(decoded.Tasks) != 1 || decoded.TotalSize != 1 {
		t.Fatalf("expected exactly the one created task, got %+v", decoded)
	}
}
