// External-effect lifecycle routes over daemon/operations (§4). All
// agent-or-operator: Propose is the "agents propose" half of decision 9
// (admission itself is gated inside daemon/policy, which Propose calls);
// every other transition here is the caller mechanically reporting real
// dispatch progress it already has authority to perform, not a new grant
// of authority — see daemon/operations's doc comment for why Resolve's
// terminal-outcome argument is trusted from the caller rather than
// computed server-side, unlike daemon/selfimprove's RecordEval.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

type effectResponse struct {
	EffectID          string `json:"effect_id,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
	OwnerExtensionID  string `json:"owner_extension_id,omitempty"`
	EffectType        string `json:"effect_type,omitempty"`
	DecisionID        string `json:"decision_id,omitempty"`
	State             string `json:"state,omitempty"`
	ForwardDigest     string `json:"forward_digest,omitempty"`
	ExternalCommandID string `json:"external_command_id,omitempty"`
	ObservationRef    string `json:"observation_ref,omitempty"`
	ErrorCode         string `json:"error_code,omitempty"`
	ErrorRetryable    bool   `json:"error_retryable,omitempty"`
	ErrorMessage      string `json:"error_message,omitempty"`
	CreatedAt         string `json:"created_at,omitempty"`
	UpdatedAt         string `json:"updated_at,omitempty"`
	Error             string `json:"error,omitempty"`
}

func toEffectResponse(eff *operations.Effect) effectResponse {
	return effectResponse{
		EffectID: eff.EffectID, OperationID: eff.OperationID, OwnerExtensionID: eff.OwnerExtensionID,
		EffectType: eff.EffectType, DecisionID: eff.DecisionID, State: string(eff.State),
		ForwardDigest: eff.ForwardDigest, ExternalCommandID: eff.ExternalCommandID, ObservationRef: eff.ObservationRef,
		ErrorCode: eff.ErrorCode, ErrorRetryable: eff.ErrorRetryable, ErrorMessage: eff.ErrorMessage,
		CreatedAt: eff.CreatedAt, UpdatedAt: eff.UpdatedAt,
	}
}

func operationsErrorStatus(err error) int {
	switch {
	case errors.Is(err, operations.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, operations.ErrInvalidTransition), errors.Is(err, policy.ErrAlreadyConsumed),
		errors.Is(err, policy.ErrDigestMismatch), errors.Is(err, policy.ErrDecisionExpired), errors.Is(err, policy.ErrNotAdmitted):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type proposeRequest struct {
	OperationID      string `json:"operation_id"`
	OwnerExtensionID string `json:"owner_extension_id"`
	EffectType       string `json:"effect_type"`
	Payload          any    `json:"payload"`
	Reversibility    string `json:"reversibility"`
}

func (s *Server) handlePropose(w http.ResponseWriter, r *http.Request) {
	var req proposeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, effectResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	eff, err := s.Operations.Propose(r.Context(), operations.ProposeRequest{
		OperationID: req.OperationID, OwnerExtensionID: req.OwnerExtensionID, EffectType: req.EffectType,
		Payload: req.Payload, Reversibility: policy.Reversibility(req.Reversibility),
	})
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toEffectResponse(eff))
}

func (s *Server) handleGetEffect(w http.ResponseWriter, r *http.Request) {
	eff, err := s.Operations.Get(r.Context(), r.PathValue("effectID"))
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}

func (s *Server) handleListEffects(w http.ResponseWriter, r *http.Request) {
	operationID := r.URL.Query().Get("operation_id")
	if operationID == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "operation_id query parameter is required"})
		return
	}
	list, err := s.Operations.ListByOperation(r.Context(), operationID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]effectResponse, 0, len(list))
	for _, eff := range list {
		out = append(out, toEffectResponse(eff))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleMarkDispatchPending(w http.ResponseWriter, r *http.Request) {
	eff, err := s.Operations.MarkDispatchPending(r.Context(), r.PathValue("effectID"))
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}

type markDispatchedRequest struct {
	ExternalCommandID string `json:"external_command_id"`
}

func (s *Server) handleMarkDispatched(w http.ResponseWriter, r *http.Request) {
	var req markDispatchedRequest
	json.NewDecoder(r.Body).Decode(&req) // body is optional; a zero value is fine
	eff, err := s.Operations.MarkDispatched(r.Context(), r.PathValue("effectID"), req.ExternalCommandID)
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}

type markObservedRequest struct {
	ObservationRef string `json:"observation_ref"`
}

func (s *Server) handleMarkObserved(w http.ResponseWriter, r *http.Request) {
	var req markObservedRequest
	json.NewDecoder(r.Body).Decode(&req)
	eff, err := s.Operations.MarkObserved(r.Context(), r.PathValue("effectID"), req.ObservationRef)
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}

func (s *Server) handleMarkOutcomeUnknown(w http.ResponseWriter, r *http.Request) {
	eff, err := s.Operations.MarkOutcomeUnknown(r.Context(), r.PathValue("effectID"))
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}

type resolveRequest struct {
	Terminal  string `json:"terminal"`
	ErrorCode string `json:"error_code,omitempty"`
	Retryable bool   `json:"retryable,omitempty"`
	Message   string `json:"message,omitempty"`
}

func (s *Server) handleResolve(w http.ResponseWriter, r *http.Request) {
	var req resolveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, effectResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	var effErr *operations.EffectError
	if req.ErrorCode != "" || req.Message != "" {
		effErr = &operations.EffectError{Code: req.ErrorCode, Retryable: req.Retryable, Message: req.Message}
	}
	eff, err := s.Operations.Resolve(r.Context(), r.PathValue("effectID"), operations.State(req.Terminal), effErr)
	if err != nil {
		writeJSON(w, operationsErrorStatus(err), effectResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toEffectResponse(eff))
}
