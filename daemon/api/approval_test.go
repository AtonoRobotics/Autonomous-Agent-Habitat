package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestApprovalGateLifecycle_CreateApproveStatus(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// 1. Agent requests a ticket for an irreversible action.
	createBody, _ := json.Marshal(map[string]any{
		"device_action_id": "nutrient-doser.dispense_ml",
		"params":           map[string]string{"ml": "5"},
		"risk":             "irreversible",
	})
	resp := postJSON(t, ts.URL+"/v1/approval-gates", testAgentToken, createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var created createTicketResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.TicketID == "" {
		t.Fatalf("expected a ticket_id, got empty")
	}

	// 2. Status must be unsatisfied before approval (agent can check this).
	statusResp := getJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID, testAgentToken)
	var status ticketStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if status.Satisfied {
		t.Fatalf("expected unsatisfied before approval")
	}

	// 3. Only the OPERATOR token can approve.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	approveResp := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testOperatorToken, approveBody)
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", approveResp.StatusCode)
	}
	approveResp.Body.Close()

	// 4. Status must now be satisfied.
	statusResp2 := getJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID, testAgentToken)
	var status2 ticketStatusResponse
	json.NewDecoder(statusResp2.Body).Decode(&status2)
	statusResp2.Body.Close()
	if !status2.Satisfied {
		t.Fatalf("expected satisfied after approval")
	}
}

// The property daemon/authn exists to enforce, verified through the real
// HTTP server rather than just the middleware in isolation: an agent
// token is mechanically refused on the approve endpoint, no matter how
// legitimate the ticket. There is no code path by which an agent can
// grant its own request.
func TestApprovalGateApprove_RejectsAgentToken(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]any{"device_action_id": "x", "params": map[string]string{"x": "y"}, "risk": "irreversible"})
	resp := postJSON(t, ts.URL+"/v1/approval-gates", testAgentToken, createBody)
	var created createTicketResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	approveBody, _ := json.Marshal(map[string]string{"approved_by": "the-requesting-agent-itself"})
	agentApprove := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testAgentToken, approveBody)
	defer agentApprove.Body.Close()
	if agentApprove.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when an agent token tries to approve, got %d", agentApprove.StatusCode)
	}

	// Confirm it's genuinely unsatisfied, not just that the response code
	// happened to be 403 — the rejected approve call must have had zero
	// effect on the ticket's state.
	statusResp := getJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID, testAgentToken)
	var status ticketStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if status.Satisfied {
		t.Fatalf("ticket must remain unsatisfied after a rejected agent approve attempt")
	}
}

func TestApprovalGateApprove_RejectsDoubleApproval(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]any{"device_action_id": "x", "params": map[string]string{"x": "y"}, "risk": "irreversible"})
	resp := postJSON(t, ts.URL+"/v1/approval-gates", testAgentToken, createBody)
	var created createTicketResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	first := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testOperatorToken, approveBody)
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected first approval to succeed, got %d", first.StatusCode)
	}

	second := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testOperatorToken, approveBody)
	defer second.Body.Close()
	if second.StatusCode == http.StatusOK {
		t.Fatalf("expected a second approval on the same ticket to be rejected")
	}
}

func TestApprovalGateCreateTicket_RejectsInvalidRisk(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"device_action_id": "x", "params": map[string]string{"x": "y"}, "risk": "not-a-real-risk"})
	resp := postJSON(t, ts.URL+"/v1/approval-gates", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid risk value, got %d", resp.StatusCode)
	}
}

// The real end-to-end loop: an irreversible device action is refused with
// no ticket, refused with an unapproved ticket, and only proceeds — with
// no recorded inverse — once the ticket created (by the agent) and
// approved (by the operator, a DIFFERENT credential) over HTTP is
// supplied. This is what makes the ApprovalGate endpoints actually
// load-bearing rather than just independently testable plumbing, and
// what makes the agent/operator token split load-bearing rather than
// just independently testable plumbing.
func TestIrreversibleActuation_RequiresApprovalCreatedAndApprovedOverHTTP(t *testing.T) {
	db := testDB(t)
	seedIrreversibleDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	actuateURL := ts.URL + "/v1/device-actions/nutrient-doser.dispense_ml/actuate"

	// No ticket at all: fail closed. (Agent identity, as it will be for
	// every actuate call here — actuation is routine agent work.)
	noTicketBody, _ := json.Marshal(map[string]any{"params": map[string]string{"ml": "5"}})
	resp := postJSON(t, actuateURL, testAgentToken, noTicketBody)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with no ticket, got %d", resp.StatusCode)
	}

	// Agent creates a ticket over HTTP.
	createBody, _ := json.Marshal(map[string]any{
		"device_action_id": "nutrient-doser.dispense_ml",
		"params":           map[string]string{"ml": "5"},
		"risk":             "irreversible",
	})
	createResp := postJSON(t, ts.URL+"/v1/approval-gates", testAgentToken, createBody)
	var created createTicketResponse
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	// Unapproved ticket: still fail closed.
	unapprovedBody, _ := json.Marshal(map[string]any{"params": map[string]string{"ml": "5"}, "ticket_id": created.TicketID})
	resp2 := postJSON(t, actuateURL, testAgentToken, unapprovedBody)
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with an unapproved ticket, got %d", resp2.StatusCode)
	}

	// The agent itself CANNOT approve its own ticket — fails closed too.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	agentApprove := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testAgentToken, approveBody)
	agentApprove.Body.Close()
	if agentApprove.StatusCode != http.StatusForbidden {
		t.Fatalf("expected the agent's own approve attempt to be rejected with 403, got %d", agentApprove.StatusCode)
	}

	// Only the operator's approval actually satisfies it.
	approveResp := postJSON(t, ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", testOperatorToken, approveBody)
	approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected operator approval to succeed, got %d", approveResp.StatusCode)
	}

	// Now the agent's actuation proceeds.
	resp3 := postJSON(t, actuateURL, testAgentToken, unapprovedBody)
	defer resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after approval, got %d", resp3.StatusCode)
	}
	var actuateResp actuateResponse
	json.NewDecoder(resp3.Body).Decode(&actuateResp)
	if actuateResp.Result != "ok" {
		t.Fatalf("expected result 'ok', got %q (error: %q)", actuateResp.Result, actuateResp.Error)
	}

	// An irreversible action's effect must still record no inverse — there
	// is nothing to auto-reverse.
	var inverse *string
	err := db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = $1", "nutrient-doser.dispense_ml").Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse != nil {
		t.Fatalf("expected no recorded inverse for an irreversible action, got %q", *inverse)
	}
}
