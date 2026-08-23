package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// owner_extension_id "amh.core/test" — under the amh.core/ prefix real
// core-owned call sites all use (daemon/inference's "amh.core/inference",
// daemon/extensions' own "amh.core/extensions"), so these lifecycle
// tests exercise Propose/dispatch/resolve without also needing an
// extension capability token — see authorizeEffectOwner's own doc
// comment. Authorization itself (§15 acceptance invariant #5) is
// exercised separately, further down this file, with a genuine
// non-core owner_extension_id.
func proposeEffect(t *testing.T, ts *httptest.Server, operationID, reversibility string) effectResponse {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"operation_id":       operationID,
		"owner_extension_id": "amh.core/test",
		"effect_type":        "amh.test/do-thing",
		"payload":            map[string]any{"op": operationID},
		"reversibility":      reversibility,
		"retry_class":        "never",
	})
	resp := postJSON(t, ts.URL+"/v1/operations", testAgentToken, body)
	defer resp.Body.Close()
	var eff effectResponse
	json.NewDecoder(resp.Body).Decode(&eff)
	return eff
}

// dispatchPendingBody is the /dispatch-pending request body — §15
// invariant #6 means the daemon now hashes whatever payload it's given
// fresh rather than trusting the digest Propose already stored, so a
// test walking the real happy path must send back the exact payload
// that was admitted.
func dispatchPendingBody(payload any) []byte {
	body, _ := json.Marshal(map[string]any{"payload": payload})
	return body
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

func TestPropose_MissingRetryClassOverHTTP_Is400(t *testing.T) {
	ts := newTestServer(t, false)
	body, _ := json.Marshal(map[string]any{
		"operation_id":       "op-1",
		"owner_extension_id": "amh.core/test",
		"effect_type":        "amh.test/do-thing",
		"payload":            map[string]any{"op": "op-1"},
		"reversibility":      "verified",
		// retry_class deliberately omitted.
	})
	resp := postJSON(t, ts.URL+"/v1/operations", testAgentToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 proposing with no retry_class, got %d", resp.StatusCode)
	}
}

func TestFullHappyPath_OverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "verified")

	dp := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, dispatchPendingBody(map[string]any{"op": "op-1"}))
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

	obsBody, _ := json.Marshal(map[string]string{"observation_ref": "artifact://obs-1", "observation_payload": "the real observed content"})
	o := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/observed", testAgentToken, obsBody)
	if o.StatusCode != http.StatusOK {
		t.Fatalf("observed: expected 200, got %d", o.StatusCode)
	}
	var observed effectResponse
	json.NewDecoder(o.Body).Decode(&observed)
	o.Body.Close()
	if observed.ObservationPayload != "the real observed content" {
		t.Fatalf("expected observation_payload to round-trip over HTTP, got %+v", observed)
	}

	// §9 acceptance invariant #9: reconstructible from durable records —
	// a fresh GET, not just the mutation's own response, must show it.
	reGet := getJSON(t, ts.URL+"/v1/operations/"+eff.EffectID, testAgentToken)
	var refetched effectResponse
	json.NewDecoder(reGet.Body).Decode(&refetched)
	reGet.Body.Close()
	if refetched.ObservationPayload != "the real observed content" {
		t.Fatalf("expected observation_payload to be durably readable on refetch, got %+v", refetched)
	}

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

	resp := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, dispatchPendingBody(map[string]any{"op": "op-1"}))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a needs_approval effect, got %d", resp.StatusCode)
	}
}

// TestMarkDispatchPending_PayloadMutatedAfterAdmission_FailsClosedOverHTTP
// is §15 acceptance invariant #6 exercised over the real HTTP boundary —
// the one place a Propose-time payload and a dispatch-time payload can
// genuinely diverge (two separate requests, potentially different
// processes), unlike the Go-internal call sites where the same in-memory
// value is passed to both in one function.
func TestMarkDispatchPending_PayloadMutatedAfterAdmission_FailsClosedOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	eff := proposeEffect(t, ts, "op-1", "verified")

	mutated := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, dispatchPendingBody(map[string]any{"op": "op-1", "amount": 1_000_000}))
	defer mutated.Body.Close()
	if mutated.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a payload that differs from the admitted one, got %d", mutated.StatusCode)
	}

	got := getJSON(t, ts.URL+"/v1/operations/"+eff.EffectID, testAgentToken)
	defer got.Body.Close()
	var gotEff effectResponse
	json.NewDecoder(got.Body).Decode(&gotEff)
	if gotEff.State != "admitted" {
		t.Fatalf("expected the effect to remain admitted after a digest mismatch, got %+v", gotEff)
	}

	real := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, dispatchPendingBody(map[string]any{"op": "op-1"}))
	defer real.Body.Close()
	if real.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 dispatching with the real, unmutated payload, got %d", real.StatusCode)
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

// ── §15 acceptance invariant #5: extension effect-ownership authorization ──

// activateRealProcessExtension discovers and activates a real
// IsolationProcess extension over HTTP (the operator-only routes any
// real operator would use) and returns its id and the real capability
// token amh-daemon actually minted and handed to that launched process —
// read back from a file the script writes its own AMH_EXTENSION_TOKEN
// env var into, the only legitimate way to learn a real token (this
// package never exposes one via any API response — see
// daemon/extensions/capability.go).
func activateRealProcessExtension(t *testing.T, ts *httptest.Server, id string) (extensionID, token string) {
	t.Helper()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	scriptFile := filepath.Join(dir, "echo-token.sh")
	script := "#!/bin/sh\nprintenv AMH_EXTENSION_TOKEN > " + tokenFile + "\nexec sleep 300\n"
	if err := os.WriteFile(scriptFile, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	manifest := baseTestManifest(id, "1.0.0")
	manifest["spec"].(map[string]any)["isolation"] = "process"
	manifest["spec"].(map[string]any)["entrypoint"] = scriptFile
	body, _ := json.Marshal(manifest)
	discover := postJSON(t, ts.URL+"/v1/extensions", testOperatorToken, body)
	if discover.StatusCode != http.StatusCreated {
		t.Fatalf("discover: expected 201, got %d", discover.StatusCode)
	}
	discover.Body.Close()

	ref, _ := json.Marshal(map[string]string{"id": id, "version": "1.0.0"})
	activate := postJSON(t, ts.URL+"/v1/extensions/activate", testOperatorToken, ref)
	if activate.StatusCode != http.StatusOK {
		t.Fatalf("activate: expected 200, got %d", activate.StatusCode)
	}
	activate.Body.Close()
	t.Cleanup(func() {
		q, _ := json.Marshal(map[string]string{"id": id, "version": "1.0.0"})
		postJSON(t, ts.URL+"/v1/extensions/quiesce", testOperatorToken, q).Body.Close()
		postJSON(t, ts.URL+"/v1/extensions/dispose", testOperatorToken, q).Body.Close()
	})

	deadline := time.Now().Add(2 * time.Second)
	var raw []byte
	var err error
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(tokenFile)
		if err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(raw) == 0 {
		t.Fatalf("expected the real launched process to have written its AMH_EXTENSION_TOKEN to %s: %v", tokenFile, err)
	}
	return id, string(bytes.TrimSpace(raw))
}

func postJSONWithExtensionToken(t *testing.T, url, bearerToken, extensionToken string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	if extensionToken != "" {
		req.Header.Set("X-AMH-Extension-Token", extensionToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	return resp
}

func proposeNonCoreEffect(ts *httptest.Server, ownerExtensionID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"operation_id":       "op-authz",
		"owner_extension_id": ownerExtensionID,
		"effect_type":        "amh.test/do-thing",
		"payload":            map[string]any{"x": 1},
		"reversibility":      "verified",
		"retry_class":        "never",
	})
	return body
}

func TestPropose_NonCoreOwner_AgentTokenWithNoExtensionToken_Is403(t *testing.T) {
	ts := newTestServer(t, false)
	resp := postJSON(t, ts.URL+"/v1/operations", testAgentToken, proposeNonCoreEffect(ts, "amh.acme/widget"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 proposing on behalf of a non-core owner with no extension token, got %d", resp.StatusCode)
	}
}

func TestPropose_NonCoreOwner_WrongExtensionsToken_Is403(t *testing.T) {
	ts := newTestServer(t, false)
	_, otherToken := activateRealProcessExtension(t, ts, "amh.acme/other-widget")

	resp := postJSONWithExtensionToken(t, ts.URL+"/v1/operations", testAgentToken, otherToken, proposeNonCoreEffect(ts, "amh.acme/widget"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 presenting a different extension's own token, got %d", resp.StatusCode)
	}
}

func TestPropose_NonCoreOwner_CorrectExtensionsToken_Succeeds(t *testing.T) {
	ts := newTestServer(t, false)
	extID, token := activateRealProcessExtension(t, ts, "amh.acme/widget")

	resp := postJSONWithExtensionToken(t, ts.URL+"/v1/operations", testAgentToken, token, proposeNonCoreEffect(ts, extID))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201 presenting the matching extension's own token, got %d: %s", resp.StatusCode, body)
	}
}

func TestPropose_NonCoreOwner_OperatorTokenAlwaysAllowed(t *testing.T) {
	ts := newTestServer(t, false)
	resp := postJSON(t, ts.URL+"/v1/operations", testOperatorToken, proposeNonCoreEffect(ts, "amh.acme/widget"))
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected an operator token to always be authorized, got %d", resp.StatusCode)
	}
}

func TestMarkDispatchPending_NonCoreOwner_RequiresTheOwningExtensionsToken(t *testing.T) {
	ts := newTestServer(t, false)
	extID, token := activateRealProcessExtension(t, ts, "amh.acme/widget")

	proposeResp := postJSONWithExtensionToken(t, ts.URL+"/v1/operations", testAgentToken, token, proposeNonCoreEffect(ts, extID))
	var eff effectResponse
	json.NewDecoder(proposeResp.Body).Decode(&eff)
	proposeResp.Body.Close()
	if eff.State != "admitted" {
		t.Fatalf("expected the proposal itself to be admitted, got %+v", eff)
	}

	withoutToken := postJSON(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, nil)
	if withoutToken.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 marking dispatch-pending with no extension token, got %d", withoutToken.StatusCode)
	}
	withoutToken.Body.Close()

	withToken := postJSONWithExtensionToken(t, ts.URL+"/v1/operations/"+eff.EffectID+"/dispatch-pending", testAgentToken, token, dispatchPendingBody(map[string]any{"x": 1}))
	defer withToken.Body.Close()
	if withToken.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 marking dispatch-pending with the owning extension's own token, got %d", withToken.StatusCode)
	}
}
