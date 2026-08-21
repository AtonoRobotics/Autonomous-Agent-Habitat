package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestApprovalGateLifecycle_CreateApproveStatus(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// 1. Create a ticket for an irreversible action.
	createBody, _ := json.Marshal(map[string]any{
		"action": map[string]string{"device_action_id": "nutrient-doser.dispense_ml"},
		"risk":   "irreversible",
	})
	resp, err := http.Post(ts.URL+"/v1/approval-gates", "application/json", bytes.NewReader(createBody))
	if err != nil {
		t.Fatalf("POST create ticket: %v", err)
	}
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var created createTicketResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.TicketID == "" {
		t.Fatalf("expected a ticket_id, got empty")
	}

	// 2. Status must be unsatisfied before approval.
	statusResp, err := http.Get(ts.URL + "/v1/approval-gates/" + created.TicketID)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	var status ticketStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if status.Satisfied {
		t.Fatalf("expected unsatisfied before approval")
	}

	// 3. Approve.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	approveResp, err := http.Post(ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", "application/json", bytes.NewReader(approveBody))
	if err != nil {
		t.Fatalf("POST approve: %v", err)
	}
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", approveResp.StatusCode)
	}
	approveResp.Body.Close()

	// 4. Status must now be satisfied.
	statusResp2, err := http.Get(ts.URL + "/v1/approval-gates/" + created.TicketID)
	if err != nil {
		t.Fatalf("GET status: %v", err)
	}
	var status2 ticketStatusResponse
	json.NewDecoder(statusResp2.Body).Decode(&status2)
	statusResp2.Body.Close()
	if !status2.Satisfied {
		t.Fatalf("expected satisfied after approval")
	}
}

func TestApprovalGateApprove_RejectsDoubleApproval(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]any{"action": map[string]string{"x": "y"}, "risk": "irreversible"})
	resp, _ := http.Post(ts.URL+"/v1/approval-gates", "application/json", bytes.NewReader(createBody))
	var created createTicketResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	first, _ := http.Post(ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", "application/json", bytes.NewReader(approveBody))
	first.Body.Close()
	if first.StatusCode != http.StatusOK {
		t.Fatalf("expected first approval to succeed, got %d", first.StatusCode)
	}

	second, err := http.Post(ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", "application/json", bytes.NewReader(approveBody))
	if err != nil {
		t.Fatalf("POST second approve: %v", err)
	}
	defer second.Body.Close()
	if second.StatusCode == http.StatusOK {
		t.Fatalf("expected a second approval on the same ticket to be rejected")
	}
}

func TestApprovalGateCreateTicket_RejectsInvalidRisk(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	body, _ := json.Marshal(map[string]any{"action": map[string]string{"x": "y"}, "risk": "not-a-real-risk"})
	resp, err := http.Post(ts.URL+"/v1/approval-gates", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for an invalid risk value, got %d", resp.StatusCode)
	}
}

// The real end-to-end loop: an irreversible device action is refused with
// no ticket, refused with an unapproved ticket, and only proceeds — with
// no recorded inverse — once the ticket created and approved entirely
// over HTTP is supplied. This is what makes the ApprovalGate endpoints
// actually load-bearing rather than just independently testable plumbing.
func TestIrreversibleActuation_RequiresApprovalCreatedAndApprovedOverHTTP(t *testing.T) {
	db := testDB(t)
	seedIrreversibleDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	actuateURL := ts.URL + "/v1/device-actions/nutrient-doser.dispense_ml/actuate"

	// No ticket at all: fail closed.
	noTicketBody, _ := json.Marshal(map[string]string{"forward": "dose 5ml"})
	resp, _ := http.Post(actuateURL, "application/json", bytes.NewReader(noTicketBody))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with no ticket, got %d", resp.StatusCode)
	}

	// Create a ticket over HTTP.
	createBody, _ := json.Marshal(map[string]any{
		"action": map[string]string{"device_action_id": "nutrient-doser.dispense_ml"},
		"risk":   "irreversible",
	})
	createResp, _ := http.Post(ts.URL+"/v1/approval-gates", "application/json", bytes.NewReader(createBody))
	var created createTicketResponse
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	// Unapproved ticket: still fail closed.
	unapprovedBody, _ := json.Marshal(map[string]string{"forward": "dose 5ml", "ticket_id": created.TicketID})
	resp2, _ := http.Post(actuateURL, "application/json", bytes.NewReader(unapprovedBody))
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with an unapproved ticket, got %d", resp2.StatusCode)
	}

	// Approve over HTTP.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	approveResp, _ := http.Post(ts.URL+"/v1/approval-gates/"+created.TicketID+"/approve", "application/json", bytes.NewReader(approveBody))
	approveResp.Body.Close()

	// Now it proceeds.
	resp3, err := http.Post(actuateURL, "application/json", bytes.NewReader(unapprovedBody))
	if err != nil {
		t.Fatalf("POST actuate: %v", err)
	}
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
	err = db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = ?", "nutrient-doser.dispense_ml").Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse != nil {
		t.Fatalf("expected no recorded inverse for an irreversible action, got %q", *inverse)
	}
}
