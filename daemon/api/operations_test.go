package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func proposeEffect(t *testing.T, ts *httptest.Server, operationID, reversibility string) effectResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"operation_id":       operationID,
		"owner_extension_id": "amh.test/widget",
		"effect_type":        "amh.test/do-thing",
		"payload":            map[string]any{"op": operationID},
		"reversibility":      reversibility,
	})
	resp := postJSON(t, ts.URL+"/v1/operations", testAgentToken, body)
	defer resp.Body.Close()
	var eff effectResponse
	json.NewDecoder(resp.Body).Decode(&eff)
	return eff
}

func TestPropose_AdmitsOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "verified")
	if eff.State != "admitted" {
		t.Fatalf("expected admitted, got %+v", eff)
	}
}

func TestPropose_NeedsApprovalOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "none")
	if eff.State != "needs_approval" {
		t.Fatalf("expected needs_approval, got %+v", eff)
	}
}

func TestFullHappyPath_OverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "verified")

	dp := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, nil)
	if dp.StatusCode != http.StatusOK {
		t.Fatalf("dispatch-pending: expected 200, got %d", dp.StatusCode)
	}
	dp.Body.Close()

	dispatchBody, _ := json.Marshal(map[string]string{"external_command_id": "cmd-123"})
	d := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatched", testAgentToken, dispatchBody)
	if d.StatusCode != http.StatusOK {
		t.Fatalf("dispatched: expected 200, got %d", d.StatusCode)
	}
	var dispatched effectResponse
	json.NewDecoder(d.Body).Decode(&dispatched)
	d.Body.Close()
	if dispatched.ExternalCommandID != "cmd-123" {
		t.Fatalf("expected external_command_id recorded, got %+v", dispatched)
	}

	obsBody, _ := json.Marshal(map[string]string{"observation_ref": "artifact://obs-1"})
	o := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/observed", testAgentToken, obsBody)
	if o.StatusCode != http.StatusOK {
		t.Fatalf("observed: expected 200, got %d", o.StatusCode)
	}
	o.Body.Close()

	resolveBody, _ := json.Marshal(map[string]string{"terminal": "confirmed"})
	res := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/resolve", testAgentToken, resolveBody)
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("resolve: expected 200, got %d", res.StatusCode)
	}
	var resolved effectResponse
	json.NewDecoder(res.Body).Decode(&resolved)
	if resolved.State != "confirmed" {
		t.Fatalf("expected confirmed, got %+v", resolved)
	}
}

func TestMarkDispatchPending_RejectsNonAdmittedOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "none")

	resp := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a needs_approval effect, got %d", resp.StatusCode)
	}
}

func TestGetEffect_UnknownReturns404(t *testing.T) {
	ts := newTestServer(t, false)
	resp := getJSON(t, ts.URL+"/v1/operations/does-not-exist", testAgentToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

func TestListEffects_ByOperationOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	proposeEffect(t, ts, "op-1", "verified")

	resp := getJSON(t, ts.URL+"/v1/operations?operation_id=op-1", testAgentToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var list []effectResponse
	json.NewDecoder(resp.Body).Decode(&list)
	if len(list) != 1 {
		t.Fatalf("expected exactly one effect, got %+v", list)
	}
}

func TestListEffects_RequiresOperationIDOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	resp := getJSON(t, ts.URL+"/v1/operations", testAgentToken)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 without operation_id, got %d", resp.StatusCode)
	}
}
