package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

func TestSafetyCaseLifecycle_CreateEvidenceApproveStatus(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	// 1. Agent opens a case.
	createBody, _ := json.Marshal(map[string]string{
		"subject_id":   "nutrient-doser.dispense_ml",
		"subject_type": "device_action",
		"risk_class":   "high",
	})
	resp := postJSON(t, ts.URL+"/v1/safety-cases", testAgentToken, createBody)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var created createSafetyCaseResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if created.CaseID == "" {
		t.Fatalf("expected a case_id, got empty")
	}

	// 2. Agent submits evidence.
	evidenceBody, _ := json.Marshal(map[string]any{
		"guardrail_proof": map[string]any{"guardrail": "flow_rate_limiter", "proven": true},
	})
	evResp := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/evidence", testAgentToken, evidenceBody)
	if evResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", evResp.StatusCode)
	}
	evResp.Body.Close()

	// 3. Status before approval.
	statusResp := getJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID, testAgentToken)
	var status safetyCaseStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if status.Approved || status.IndependentReview {
		t.Fatalf("expected a fresh case to be neither approved nor independently reviewed, got %+v", status)
	}

	// 4. Only the operator can approve.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	approveResp := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/approve", testOperatorToken, approveBody)
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", approveResp.StatusCode)
	}
	approveResp.Body.Close()

	// 5. Status after approval: approved AND independently reviewed together.
	statusResp2 := getJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID, testAgentToken)
	var status2 safetyCaseStatusResponse
	json.NewDecoder(statusResp2.Body).Decode(&status2)
	statusResp2.Body.Close()
	if !status2.Approved || !status2.IndependentReview {
		t.Fatalf("expected approved and independent_review both true, got %+v", status2)
	}
}

// The property this endpoint split exists for: an agent token cannot
// approve its own SafetyCase, mirroring the ApprovalGate's equivalent
// property — verified through the real running server.
func TestSafetyCaseApprove_RejectsAgentToken(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]string{
		"subject_id": "x", "subject_type": "device_action", "risk_class": "high",
	})
	resp := postJSON(t, ts.URL+"/v1/safety-cases", testAgentToken, createBody)
	var created createSafetyCaseResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	approveBody, _ := json.Marshal(map[string]string{"approved_by": "the-requesting-agent-itself"})
	agentApprove := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/approve", testAgentToken, approveBody)
	defer agentApprove.Body.Close()
	if agentApprove.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when an agent token tries to approve a safety case, got %d", agentApprove.StatusCode)
	}

	statusResp := getJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID, testAgentToken)
	var status safetyCaseStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if status.Approved {
		t.Fatalf("case must remain unapproved after a rejected agent approve attempt")
	}
}

func TestSafetyCaseRevoke_RejectsAgentTokenAndIsImmediate(t *testing.T) {
	db := testDB(t)
	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	createBody, _ := json.Marshal(map[string]string{
		"subject_id": "x", "subject_type": "device_action", "risk_class": "high",
	})
	resp := postJSON(t, ts.URL+"/v1/safety-cases", testAgentToken, createBody)
	var created createSafetyCaseResponse
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()

	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/approve", testOperatorToken, approveBody).Body.Close()

	revokeBody, _ := json.Marshal(map[string]string{"reason": "incident"})
	agentRevoke := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/revoke", testAgentToken, revokeBody)
	agentRevoke.Body.Close()
	if agentRevoke.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 when an agent token tries to revoke, got %d", agentRevoke.StatusCode)
	}

	operatorRevoke := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/revoke", testOperatorToken, revokeBody)
	defer operatorRevoke.Body.Close()
	if operatorRevoke.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for operator revoke, got %d", operatorRevoke.StatusCode)
	}

	statusResp := getJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID, testAgentToken)
	var status safetyCaseStatusResponse
	json.NewDecoder(statusResp.Body).Decode(&status)
	statusResp.Body.Close()
	if !status.Revoked || status.RevokedReason != "incident" {
		t.Fatalf("expected revoked=true with reason recorded, got %+v", status)
	}
}

// The real end-to-end loop, and the whole point of SafetyCase existing
// as a distinct path from the ApprovalGate: an irreversible device
// action, once its SafetyCase is created (by the agent), given evidence
// (by the agent), and approved (by the operator — a different
// credential), can be actuated with NO ticket_id at all. This is what
// makes §14.7's "standing autonomy grant" property real rather than
// aspirational.
func TestIrreversibleActuation_SucceedsViaApprovedSafetyCaseWithNoTicket(t *testing.T) {
	db := testDB(t)
	seedIrreversibleDeviceAction(t, db)

	tp := sdktrace.NewTracerProvider()
	server := New("", db, tp, testAuth(t), nil, t.TempDir(), nil)
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	actuateURL := ts.URL + "/v1/device-actions/nutrient-doser.dispense_ml/actuate"

	// Before any SafetyCase exists: still fails closed, exactly as
	// without one.
	noTicketBody, _ := json.Marshal(map[string]any{"params": map[string]string{"ml": "5"}})
	before := postJSON(t, actuateURL, testAgentToken, noTicketBody)
	before.Body.Close()
	if before.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before any safety case exists, got %d", before.StatusCode)
	}

	// Agent opens and evidences a case.
	createBody, _ := json.Marshal(map[string]string{
		"subject_id": "nutrient-doser.dispense_ml", "subject_type": "device_action", "risk_class": "irreversible_high_consequence",
	})
	createResp := postJSON(t, ts.URL+"/v1/safety-cases", testAgentToken, createBody)
	var created createSafetyCaseResponse
	json.NewDecoder(createResp.Body).Decode(&created)
	createResp.Body.Close()

	evidenceBody, _ := json.Marshal(map[string]any{
		"guardrail_proof": map[string]any{"guardrail": "max_daily_dose", "proven": true},
	})
	postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/evidence", testAgentToken, evidenceBody).Body.Close()

	// Still no ticket, still no approval yet: still fails closed.
	stillBefore := postJSON(t, actuateURL, testAgentToken, noTicketBody)
	stillBefore.Body.Close()
	if stillBefore.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before the case is approved, got %d", stillBefore.StatusCode)
	}

	// Operator approves the case.
	approveBody, _ := json.Marshal(map[string]string{"approved_by": "operator:jane"})
	approveResp := postJSON(t, ts.URL+"/v1/safety-cases/"+created.CaseID+"/approve", testOperatorToken, approveBody)
	approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for operator approval, got %d", approveResp.StatusCode)
	}

	// NOW the agent can actuate with NO ticket_id — the SafetyCase alone
	// is the autonomy grant.
	after := postJSON(t, actuateURL, testAgentToken, noTicketBody)
	defer after.Body.Close()
	if after.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 after safety case approval, got %d", after.StatusCode)
	}
	var actuateResp actuateResponse
	json.NewDecoder(after.Body).Decode(&actuateResp)
	if actuateResp.Result != "ok" {
		t.Fatalf("expected result 'ok', got %q (error: %q)", actuateResp.Result, actuateResp.Error)
	}

	var inverse *string
	err := db.QueryRow("SELECT inverse_payload FROM device_effect WHERE device_action_id = ?", "nutrient-doser.dispense_ml").Scan(&inverse)
	if err != nil {
		t.Fatalf("query device_effect: %v", err)
	}
	if inverse != nil {
		t.Fatalf("expected no recorded inverse for an irreversible action, got %q", *inverse)
	}
}
