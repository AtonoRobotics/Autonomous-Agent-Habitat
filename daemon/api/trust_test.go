package api

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
)

func registerTrustedKey(t *testing.T, ts *httptest.Server, keyID string) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	body, _ := json.Marshal(map[string]string{"key_id": keyID, "public_key": hex.EncodeToString(pub)})
	resp := postJSON(t, ts.URL+"/v1/extensions/trusted-keys", testOperatorToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register trusted key: expected 201, got %d", resp.StatusCode)
	}
	return priv
}

func TestRegisterTrustedKey_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(map[string]string{"key_id": "key-1", "public_key": hex.EncodeToString(pub)})

	agentAttempt := postJSON(t, ts.URL+"/v1/extensions/trusted-keys", testAgentToken, body)
	defer agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token registering a trusted key, got %d", agentAttempt.StatusCode)
	}
}

func TestRegisterTrustedKey_RejectsDuplicateID(t *testing.T) {
	ts := newTestServer(t, false)
	registerTrustedKey(t, ts, "key-1")

	pub, _, _ := ed25519.GenerateKey(nil)
	body, _ := json.Marshal(map[string]string{"key_id": "key-1", "public_key": hex.EncodeToString(pub)})
	resp := postJSON(t, ts.URL+"/v1/extensions/trusted-keys", testOperatorToken, body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for a duplicate key_id, got %d", resp.StatusCode)
	}
}

func TestListAndGetTrustedKeys_AgentOrOperator(t *testing.T) {
	ts := newTestServer(t, false)
	registerTrustedKey(t, ts, "key-1")

	list := getJSON(t, ts.URL+"/v1/extensions/trusted-keys", testAgentToken)
	defer list.Body.Close()
	if list.StatusCode != http.StatusOK {
		t.Fatalf("list: expected 200, got %d", list.StatusCode)
	}
	var keys []trustedKeyResponse
	json.NewDecoder(list.Body).Decode(&keys)
	if len(keys) != 1 || keys[0].KeyID != "key-1" {
		t.Fatalf("expected exactly one trusted key, got %+v", keys)
	}

	get := getJSON(t, ts.URL+"/v1/extensions/trusted-keys/key-1", testAgentToken)
	defer get.Body.Close()
	if get.StatusCode != http.StatusOK {
		t.Fatalf("get: expected 200, got %d", get.StatusCode)
	}
}

func TestRevokeTrustedKey_OperatorOnly(t *testing.T) {
	ts := newTestServer(t, false)
	registerTrustedKey(t, ts, "key-1")

	agentAttempt := postJSON(t, ts.URL+"/v1/extensions/trusted-keys/key-1/revoke", testAgentToken, nil)
	defer agentAttempt.Body.Close()
	if agentAttempt.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for an agent token revoking a trusted key, got %d", agentAttempt.StatusCode)
	}

	revoke := postJSON(t, ts.URL+"/v1/extensions/trusted-keys/key-1/revoke", testOperatorToken, nil)
	defer revoke.Body.Close()
	if revoke.StatusCode != http.StatusOK {
		t.Fatalf("revoke: expected 200, got %d", revoke.StatusCode)
	}
	var revoked trustedKeyResponse
	json.NewDecoder(revoke.Body).Decode(&revoked)
	if revoked.RevokedAt == "" {
		t.Fatalf("expected revoked_at to be set")
	}
}

// signedTestManifest builds a real extensions.Manifest (not the loose
// map[string]any baseTestManifest uses elsewhere in this package) so its
// SignableDigest can be computed and signed exactly as the daemon will
// recompute it server-side after decoding the same JSON body.
func signedTestManifest(t *testing.T, id, version, keyID string, priv ed25519.PrivateKey) extensions.Manifest {
	t.Helper()
	m := extensions.Manifest{
		APIVersion: "amh/v1",
		Kind:       "Extension",
		Metadata: extensions.Metadata{
			ID: id, Name: "Test Extension", Version: version, Publisher: "amh-tests",
		},
		Spec: extensions.Spec{
			Entrypoint: "true",
			Isolation:  extensions.IsolationInProcess,
			Provides:   []extensions.CapabilityRef{},
			Requires:   []extensions.Requirement{},
			Compatibility: extensions.Compatibility{
				AMHCore: ">=0.1.0",
			},
		},
	}
	digest, err := m.SignableDigest()
	if err != nil {
		t.Fatalf("SignableDigest: %v", err)
	}
	sig := ed25519.Sign(priv, []byte(digest))
	m.Spec.Signature = &extensions.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Digest:    digest,
		Value:     hex.EncodeToString(sig),
	}
	return m
}

func TestDiscoverExtension_AdmitsValidSignedManifestOverHTTP(t *testing.T) {
	ts := newTestServer(t, false)
	priv := registerTrustedKey(t, ts, "key-1")
	m := signedTestManifest(t, "amh.test/signed", "1.0.0", "key-1", priv)

	body, _ := json.Marshal(m)
	discover := postJSON(t, ts.URL+"/v1/extensions", testOperatorToken, body)
	defer discover.Body.Close()
	if discover.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 for a validly signed manifest, got %d", discover.StatusCode)
	}
}

func TestDiscoverExtension_RejectsSignatureFromRevokedKey(t *testing.T) {
	ts := newTestServer(t, false)
	priv := registerTrustedKey(t, ts, "key-1")
	revoke := postJSON(t, ts.URL+"/v1/extensions/trusted-keys/key-1/revoke", testOperatorToken, nil)
	revoke.Body.Close()

	m := signedTestManifest(t, "amh.test/signed", "1.0.0", "key-1", priv)
	body, _ := json.Marshal(m)
	discover := postJSON(t, ts.URL+"/v1/extensions", testOperatorToken, body)
	defer discover.Body.Close()
	if discover.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a signature from a revoked key, got %d", discover.StatusCode)
	}
}
