// Generic policy and approval routes (docs/AMH-SPECIFICATION.md §6) over
// daemon/policy. Decide/Consume are agent-or-operator — decision 9 is
// "agents propose," and proposing (and, once admitted, dispatching) an
// action is exactly the agent-role action this seam exists to gate, not
// restrict. Approve/Deny are operator-only: an agent token must never be
// able to resolve its own ApprovalRequest, the same anti-self-approval
// property daemon/authn's doc comment describes for every other
// operator-only route.
package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

// decodeOptionalJSON decodes an optional JSON body into v, leaving v at
// its zero value for a genuinely empty body (io.EOF on the first read) —
// approve/deny both accept a body with only optional fields, and a caller
// that sends no body at all shouldn't get a 400 for that.
func decodeOptionalJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

type decisionResponse struct {
	ID                string   `json:"id,omitempty"`
	OperationID       string   `json:"operation_id,omitempty"`
	ActionDigest      string   `json:"action_digest,omitempty"`
	PolicyID          string   `json:"policy_id,omitempty"`
	PolicyVersion     string   `json:"policy_version,omitempty"`
	Result            string   `json:"result,omitempty"`
	ReasonCodes       []string `json:"reason_codes,omitempty"`
	ApprovalRequestID string   `json:"approval_request_id,omitempty"`
	DecidedAt         string   `json:"decided_at,omitempty"`
	ExpiresAt         string   `json:"expires_at,omitempty"`
	ConsumedAt        string   `json:"consumed_at,omitempty"`
	Error             string   `json:"error,omitempty"`
}

func toDecisionResponse(d *policy.Decision) decisionResponse {
	return decisionResponse{
		ID: d.ID, OperationID: d.OperationID, ActionDigest: d.ActionDigest,
		PolicyID: d.PolicyID, PolicyVersion: d.PolicyVersion, Result: string(d.Result),
		ReasonCodes: d.ReasonCodes, ApprovalRequestID: d.ApprovalRequestID,
		DecidedAt: d.DecidedAt, ExpiresAt: d.ExpiresAt, ConsumedAt: d.ConsumedAt,
	}
}

type approvalRequestResponse struct {
	ID         string `json:"id,omitempty"`
	DecisionID string `json:"decision_id,omitempty"`
	Status     string `json:"status,omitempty"`
	ResolvedBy string `json:"resolved_by,omitempty"`
	ResolvedAt string `json:"resolved_at,omitempty"`
	Reason     string `json:"reason,omitempty"`
	Error      string `json:"error,omitempty"`
}

func toApprovalRequestResponse(a *policy.ApprovalRequest) approvalRequestResponse {
	return approvalRequestResponse{
		ID: a.ID, DecisionID: a.DecisionID, Status: a.Status,
		ResolvedBy: a.ResolvedBy, ResolvedAt: a.ResolvedAt, Reason: a.Reason,
	}
}

func policyErrorStatus(err error) int {
	switch {
	case errors.Is(err, policy.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, policy.ErrAlreadyConsumed), errors.Is(err, policy.ErrNotAdmitted),
		errors.Is(err, policy.ErrDigestMismatch), errors.Is(err, policy.ErrDecisionExpired),
		errors.Is(err, policy.ErrNotPending):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type decideRequest struct {
	OperationID   string `json:"operation_id"`
	Payload       any    `json:"payload"`
	Reversibility string `json:"reversibility"`
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request) {
	var req decideRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, decisionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.OperationID == "" {
		writeJSON(w, http.StatusBadRequest, decisionResponse{Error: "operation_id is required"})
		return
	}
	d, err := s.Policy.Decide(r.Context(), policy.DecideRequest{
		OperationID:   req.OperationID,
		Payload:       req.Payload,
		Reversibility: policy.Reversibility(req.Reversibility),
	})
	if err != nil {
		writeJSON(w, policyErrorStatus(err), decisionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toDecisionResponse(d))
}

func (s *Server) handleGetDecision(w http.ResponseWriter, r *http.Request) {
	d, err := s.Policy.Get(r.Context(), r.PathValue("decisionID"))
	if err != nil {
		writeJSON(w, policyErrorStatus(err), decisionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toDecisionResponse(d))
}

type consumeRequest struct {
	// Payload is the exact action about to be dispatched — its digest is
	// computed server-side (policy.Digest) rather than trusted from a
	// client-supplied digest string, so a mismatch here can only mean the
	// caller is about to dispatch something other than what was decided
	// on, never a caller that simply typed the right-looking digest.
	Payload any `json:"payload"`
}

func (s *Server) handleConsumeDecision(w http.ResponseWriter, r *http.Request) {
	var req consumeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	digest, err := policy.Digest(req.Payload)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: err.Error()})
		return
	}
	if err := s.Policy.Consume(r.Context(), r.PathValue("decisionID"), digest); err != nil {
		writeJSON(w, policyErrorStatus(err), simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

func (s *Server) handleListPendingApprovals(w http.ResponseWriter, r *http.Request) {
	list, err := s.Policy.ListPendingApprovals(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]approvalRequestResponse, 0, len(list))
	for _, a := range list {
		out = append(out, toApprovalRequestResponse(&a))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleGetApprovalRequest(w http.ResponseWriter, r *http.Request) {
	a, err := s.Policy.GetApprovalRequest(r.Context(), r.PathValue("approvalID"))
	if err != nil {
		writeJSON(w, policyErrorStatus(err), approvalRequestResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toApprovalRequestResponse(a))
}

type resolveApprovalRequest struct {
	ResolvedBy string `json:"resolved_by,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

func (s *Server) handleApproveApprovalRequest(w http.ResponseWriter, r *http.Request) {
	var req resolveApprovalRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, decisionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	d, err := s.Policy.Approve(r.Context(), r.PathValue("approvalID"), req.ResolvedBy)
	if err != nil {
		writeJSON(w, policyErrorStatus(err), decisionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toDecisionResponse(d))
}

func (s *Server) handleDenyApprovalRequest(w http.ResponseWriter, r *http.Request) {
	var req resolveApprovalRequest
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, approvalRequestResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	a, err := s.Policy.Deny(r.Context(), r.PathValue("approvalID"), req.ResolvedBy, req.Reason)
	if err != nil {
		writeJSON(w, policyErrorStatus(err), approvalRequestResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toApprovalRequestResponse(a))
}
