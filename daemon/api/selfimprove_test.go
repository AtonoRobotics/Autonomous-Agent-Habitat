package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func passCaseResults(n int) []bool {
	out := make([]bool, n)
	for i := range out {
		out[i] = true
	}
	return out
}

func generateCandidate(t *testing.T, ts *httptest.Server, class string) candidateResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"candidate_class": class, "ref": "ref-1", "generated_by": "optimizer-x"})
	resp := postJSON(t, ts.URL+"/v1/selfimprove/candidates", testAgentToken, body)
	defer resp.Body.Close()
	var c candidateResponse
	json.NewDecoder(resp.Body).Decode(&c)
	return c
}

func recordEval(t *testing.T, ts *httptest.Server, candidateID string, caseResults []bool) evalResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"evaluator_id": "eval-suite", "evaluator_version": "1.0.0", "case_results": caseResults})
	resp := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+candidateID+"/eval", testAgentToken, body)
	defer resp.Body.Close()
	var ev evalResponse
	json.NewDecoder(resp.Body).Decode(&ev)
	return ev
}

func TestSelfImproveGenerate_ValidClass_CreatedOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	c := generateCandidate(t, ts, "prompt")
	if c.Status != "generated" {
		t.Fatalf("expected generated, got %s", c.Status)
	}
}

func TestSelfImproveGenerate_InvalidClass_Returns400(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]string{"candidate_class": "not-a-real-class", "ref": "x"})
	resp := postJSON(t, ts.URL+"/v1/selfimprove/candidates", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
}

func TestSelfImproveRecordEval_ComputesVerdictServerSide(t *testing.T) {
	ts := newTestServer(t, false)
	c := generateCandidate(t, ts, "prompt")

	ev := recordEval(t, ts, c.ID, passCaseResults(10))
	if !ev.Passed {
		t.Fatalf("expected a 100%% pass rate to pass, got %+v", ev)
	}

	get := getJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID, testAgentToken)
	defer get.Body.Close()
	var got candidateResponse
	json.NewDecoder(get.Body).Decode(&got)
	if got.Status != "evaluated" {
		t.Fatalf("expected evaluated after a passing eval, got %s", got.Status)
	}
}

func TestSelfImproveGetCandidate_UnknownID_Returns404(t *testing.T) {
	ts := newTestServer(t, false)
	resp := getJSON(t, ts.URL+"/v1/selfimprove/candidates/does-not-exist", testAgentToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestSelfImproveListCandidates_FiltersByClass(t *testing.T) {
	ts := newTestServer(t, false)
	generateCandidate(t, ts, "prompt")
	generateCandidate(t, ts, "skill")

	resp := getJSON(t, ts.URL+"/v1/selfimprove/candidates?candidate_class=prompt", testAgentToken)
	defer resp.Body.Close()
	var list []candidateResponse
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 || list[0].CandidateClass != "prompt" {
		t.Fatalf("expected exactly one prompt candidate, got %+v", list)
	}
}

// promoteThroughCanaryOverHTTP drives one candidate through the full
// generate -> eval -> canary -> eval -> promote lifecycle using the
// operator token for every gated transition.
func promoteThroughCanaryOverHTTP(t *testing.T, ts *httptest.Server, class string) candidateResponse {
	t.Helper()
	c := generateCandidate(t, ts, class)
	recordEval(t, ts, c.ID, passCaseResults(10))

	canary := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID+"/canary", testOperatorToken, nil)
	canary.Body.Close()
	if canary.StatusCode != http.StatusOK {
		t.Fatalf("canary: expected 200, got %d", canary.StatusCode)
	}

	recordEval(t, ts, c.ID, passCaseResults(10))

	promote := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID+"/promote", testOperatorToken, nil)
	defer promote.Body.Close()
	if promote.StatusCode != http.StatusOK {
		t.Fatalf("promote: expected 200, got %d", promote.StatusCode)
	}
	var promoted candidateResponse
	json.NewDecoder(promote.Body).Decode(&promoted)
	return promoted
}

func TestSelfImproveFullLifecycle_PromoteRequiresOperator(t *testing.T) {
	ts := newTestServer(t, false)
	c := generateCandidate(t, ts, "prompt")
	recordEval(t, ts, c.ID, passCaseResults(10))

	agentCanary := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID+"/canary", testAgentToken, nil)
	agentCanary.Body.Close()
	if agentCanary.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token canarying, got %d", agentCanary.StatusCode)
	}

	promoted := promoteThroughCanaryOverHTTP(t, ts, "prompt")
	if promoted.Status != "promoted" {
		t.Fatalf("expected promoted, got %s", promoted.Status)
	}
}

func TestSelfImprovePromote_DemotesPreviousAndSetsRollbackTarget(t *testing.T) {
	ts := newTestServer(t, false)
	first := promoteThroughCanaryOverHTTP(t, ts, "prompt")
	second := promoteThroughCanaryOverHTTP(t, ts, "prompt")

	if second.RollbackTargetID != first.ID {
		t.Fatalf("expected the second promotion's rollback target to be the first, got %q want %q", second.RollbackTargetID, first.ID)
	}
}

func TestSelfImproveDemoteThenRollback_OverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	first := promoteThroughCanaryOverHTTP(t, ts, "prompt")
	second := promoteThroughCanaryOverHTTP(t, ts, "prompt")

	agentDemote := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+second.ID+"/demote", testAgentToken, nil)
	agentDemote.Body.Close()
	if agentDemote.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token demoting, got %d", agentDemote.StatusCode)
	}

	demote := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+second.ID+"/demote", testOperatorToken, nil)
	demote.Body.Close()
	if demote.StatusCode != http.StatusOK {
		t.Fatalf("demote: expected 200, got %d", demote.StatusCode)
	}

	rollback := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+second.ID+"/rollback", testOperatorToken, nil)
	defer rollback.Body.Close()
	if rollback.StatusCode != http.StatusOK {
		t.Fatalf("rollback: expected 200, got %d", rollback.StatusCode)
	}
	var rolledBack candidateResponse
	json.NewDecoder(rollback.Body).Decode(&rolledBack)
	if rolledBack.Status != "rolled_back" {
		t.Fatalf("expected rolled_back, got %s", rolledBack.Status)
	}

	restored := getJSON(t, ts.URL+"/v1/selfimprove/candidates/"+first.ID, testAgentToken)
	defer restored.Body.Close()
	var got candidateResponse
	json.NewDecoder(restored.Body).Decode(&got)
	if got.Status != "promoted" {
		t.Fatalf("expected the first candidate restored to promoted, got %s", got.Status)
	}
}

func TestSelfImproveReject_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	c := generateCandidate(t, ts, "prompt")

	agentReject := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID+"/reject", testAgentToken, nil)
	agentReject.Body.Close()
	if agentReject.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token rejecting, got %d", agentReject.StatusCode)
	}

	reject := postJSON(t, ts.URL+"/v1/selfimprove/candidates/"+c.ID+"/reject", testOperatorToken, nil)
	defer reject.Body.Close()
	if reject.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", reject.StatusCode)
	}
	var rejected candidateResponse
	json.NewDecoder(reject.Body).Decode(&rejected)
	if rejected.Status != "rejected" {
		t.Fatalf("expected rejected, got %s", rejected.Status)
	}
}
