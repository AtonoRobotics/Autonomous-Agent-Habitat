package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestPolicyDecide_VerifiedReversibility_AdmitsOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)

	body, _ := json.Marshal(map[string]any{
		"operation_id":  "op-1",
		"payload":       map[string]any{"open_pct": 60},
		"reversibility": "verified",
	})
	resp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var d decisionResponse
	json.NewDecoder(resp.Body).Decode(&d)
	if d.Result != "admit" {
		t.Fatalf("expected admit, got %s", d.Result)
	}
	if d.ApprovalRequestID != "" {
		t.Fatalf("expected no approval request for an admitted decision")
	}
}

func TestPolicyDecide_MissingOperationID_Returns400(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]any{"payload": map[string]any{}, "reversibility": "verified"})
	resp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestPolicyDecide_RejectsRequestsWithNoToken(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": map[string]any{}, "reversibility": "verified"})
	resp := postJSON(t, ts.URL+"/v1/policy/decide", "", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
}

func TestPolicyConsume_RealPayloadRoundTrip_OverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	payload := map[string]any{"open_pct": 60}

	decideBody, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": payload, "reversibility": "verified"})
	decideResp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, decideBody)
	var d decisionResponse
	json.NewDecoder(decideResp.Body).Decode(&d)
	decideResp.Body.Close()

	consumeBody, _ := json.Marshal(map[string]any{"payload": payload})
	consume := postJSON(t, ts.URL+"/v1/policy/decisions/"+d.ID+"/consume", testAgentToken, consumeBody)
	defer consume.Body.Close()
	if consume.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", consume.StatusCode)
	}

	// A second consume of the same decision fails closed.
	consume2 := postJSON(t, ts.URL+"/v1/policy/decisions/"+d.ID+"/consume", testAgentToken, consumeBody)
	defer consume2.Body.Close()
	if consume2.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 on a repeat consume, got %d", consume2.StatusCode)
	}
}

func TestPolicyConsume_DifferentPayloadThanDecided_Returns409(t *testing.T) {
	ts := newTestServer(t, false)

	decideBody, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": map[string]any{"open_pct": 60}, "reversibility": "verified"})
	decideResp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, decideBody)
	var d decisionResponse
	json.NewDecoder(decideResp.Body).Decode(&d)
	decideResp.Body.Close()

	// Dispatching a DIFFERENT payload than what was admitted must fail —
	// the whole point of binding to the digest.
	consumeBody, _ := json.Marshal(map[string]any{"payload": map[string]any{"open_pct": 99}})
	consume := postJSON(t, ts.URL+"/v1/policy/decisions/"+d.ID+"/consume", testAgentToken, consumeBody)
	defer consume.Body.Close()
	if consume.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a digest mismatch, got %d", consume.StatusCode)
	}
}

func TestPolicyGetDecision_OverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	decideBody, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": map[string]any{}, "reversibility": "verified"})
	decideResp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, decideBody)
	var d decisionResponse
	json.NewDecoder(decideResp.Body).Decode(&d)
	decideResp.Body.Close()

	get := getJSON(t, ts.URL+"/v1/policy/decisions/"+d.ID, testAgentToken)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", get.StatusCode)
	}
}

func TestPolicyGetDecision_UnknownID_Returns404(t *testing.T) {
	ts := newTestServer(t, false)
	get := getJSON(t, ts.URL+"/v1/policy/decisions/does-not-exist", testAgentToken)
	defer get.Body.Close()
	if get.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", get.StatusCode)
	}
}

func TestPolicyApprovalLoop_ThroughHTTP_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	payload := map[string]any{"ml": 5}

	// Unverified reversibility -> needs_approval, with a bound approval request.
	decideBody, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": payload, "reversibility": "none"})
	decideResp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, decideBody)
	var d decisionResponse
	json.NewDecoder(decideResp.Body).Decode(&d)
	decideResp.Body.Close()
	if d.Result != "needs_approval" || d.ApprovalRequestID == "" {
		t.Fatalf("expected needs_approval with a bound approval request, got %+v", d)
	}

	// Consuming the needs_approval decision is refused.
	consumeBody, _ := json.Marshal(map[string]any{"payload": payload})
	blocked := postJSON(t, ts.URL+"/v1/policy/decisions/"+d.ID+"/consume", testAgentToken, consumeBody)
	blocked.Body.Close()
	if blocked.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 consuming a needs_approval decision, got %d", blocked.StatusCode)
	}

	// An agent token cannot approve its own request — decision 9's
	// anti-self-approval property, mechanically enforced.
	agentAttempt := postJSON(t, ts.URL+"/v1/policy/approvals/"+d.ApprovalRequestID+"/approve", testAgentToken, nil)
	agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token approving, got %d", agentAttempt.StatusCode)
	}

	// list pending shows the request.
	list := getJSON(t, ts.URL+"/v1/policy/approvals", testAgentToken)
	var pending []approvalRequestResponse
	json.NewDecoder(list.Body).Decode(&pending)
	list.Body.Close()
	if len(pending) != 1 || pending[0].ID != d.ApprovalRequestID {
		t.Fatalf("expected the pending approval listed, got %+v", pending)
	}

	// Operator approves — mints a fresh, consumable admit decision.
	approveBody, _ := json.Marshal(map[string]string{"resolved_by": "operator:jane"})
	approve := postJSON(t, ts.URL+"/v1/policy/approvals/"+d.ApprovalRequestID+"/approve", testOperatorToken, approveBody)
	var approved decisionResponse
	json.NewDecoder(approve.Body).Decode(&approved)
	approve.Body.Close()
	if approved.Result != "admit" || approved.ID == d.ID {
		t.Fatalf("expected a freshly minted admit decision, got %+v", approved)
	}

	// Now consuming the NEW decision succeeds.
	nowConsume := postJSON(t, ts.URL+"/v1/policy/decisions/"+approved.ID+"/consume", testAgentToken, consumeBody)
	defer nowConsume.Body.Close()
	if nowConsume.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 consuming the approved decision, got %d", nowConsume.StatusCode)
	}

	// The list is empty again.
	list2 := getJSON(t, ts.URL+"/v1/policy/approvals", testAgentToken)
	var pending2 []approvalRequestResponse
	json.NewDecoder(list2.Body).Decode(&pending2)
	list2.Body.Close()
	if len(pending2) != 0 {
		t.Fatalf("expected no pending approvals left, got %+v", pending2)
	}
}

func TestPolicyDenyApprovalRequest_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	decideBody, _ := json.Marshal(map[string]any{"operation_id": "op-1", "payload": map[string]any{}, "reversibility": "claimed"})
	decideResp := postJSON(t, ts.URL+"/v1/policy/decide", testAgentToken, decideBody)
	var d decisionResponse
	json.NewDecoder(decideResp.Body).Decode(&d)
	decideResp.Body.Close()

	agentAttempt := postJSON(t, ts.URL+"/v1/policy/approvals/"+d.ApprovalRequestID+"/deny", testAgentToken, nil)
	agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token denying, got %d", agentAttempt.StatusCode)
	}

	denyBody, _ := json.Marshal(map[string]string{"resolved_by": "operator:jane", "reason": "too risky"})
	deny := postJSON(t, ts.URL+"/v1/policy/approvals/"+d.ApprovalRequestID+"/deny", testOperatorToken, denyBody)
	var ar approvalRequestResponse
	json.NewDecoder(deny.Body).Decode(&ar)
	deny.Body.Close()
	if ar.Status != "denied" {
		t.Fatalf("expected denied, got %s", ar.Status)
	}
}
