// Trusted-signing-key admin routes for daemon/extensions' Ed25519 manifest
// signature verification (§14: "signed extension packs and compatibility
// qualification"). Registering or revoking a key is operator-only — it
// changes what manifests this daemon will admit as signed, the same
// "deterministic services commit" tier as an account credential write.
// Reading the trust store is agent-or-operator, like the rest of the
// extension registry's read routes: a public key is not a secret.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
)

type trustedKeyResponse struct {
	KeyID     string `json:"key_id,omitempty"`
	PublicKey string `json:"public_key,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	RevokedAt string `json:"revoked_at,omitempty"`
	Error     string `json:"error,omitempty"`
}

func toTrustedKeyResponse(k *extensions.TrustedKey) trustedKeyResponse {
	return trustedKeyResponse{KeyID: k.KeyID, PublicKey: k.PublicKeyHex, CreatedAt: k.CreatedAt, RevokedAt: k.RevokedAt}
}

func trustedKeyErrorStatus(err error) int {
	switch {
	case errors.Is(err, extensions.ErrKeyNotFound):
		return http.StatusNotFound
	case errors.Is(err, extensions.ErrKeyExists), errors.Is(err, extensions.ErrKeyRevoked):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type registerTrustedKeyRequest struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

func (s *Server) handleRegisterTrustedKey(w http.ResponseWriter, r *http.Request) {
	var req registerTrustedKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, trustedKeyResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	k, err := s.Extensions.Trust.RegisterKey(r.Context(), req.KeyID, req.PublicKey)
	if err != nil {
		writeJSON(w, trustedKeyErrorStatus(err), trustedKeyResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toTrustedKeyResponse(k))
}

func (s *Server) handleRevokeTrustedKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.Extensions.Trust.RevokeKey(r.Context(), r.PathValue("keyID"))
	if err != nil {
		writeJSON(w, trustedKeyErrorStatus(err), trustedKeyResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toTrustedKeyResponse(k))
}

func (s *Server) handleGetTrustedKey(w http.ResponseWriter, r *http.Request) {
	k, err := s.Extensions.Trust.Get(r.Context(), r.PathValue("keyID"))
	if err != nil {
		writeJSON(w, trustedKeyErrorStatus(err), trustedKeyResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toTrustedKeyResponse(k))
}

func (s *Server) handleListTrustedKeys(w http.ResponseWriter, r *http.Request) {
	list, err := s.Extensions.Trust.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]trustedKeyResponse, 0, len(list))
	for _, k := range list {
		out = append(out, toTrustedKeyResponse(k))
	}
	writeJSON(w, http.StatusOK, out)
}
