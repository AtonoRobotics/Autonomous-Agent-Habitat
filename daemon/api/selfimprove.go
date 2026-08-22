// Self-improvement candidate lifecycle routes (docs/AMH-SPECIFICATION.md
// §10) over daemon/selfimprove. Generate is agent-or-operator — an agent
// (or a future optimizer module) proposes a candidate. RecordEval and
// every state transition (Canary/Promote/Demote/Rollback/Reject) are
// operator-only: the daemon always computes the pass/fail verdict from
// raw case results, but that alone doesn't make the evidence
// independent if the same agent token that proposed the candidate can
// also submit whatever results it likes — see api.go's route-table
// doc comment for why RecordEval sits at the operator tier, not the
// "agents propose" tier Generate does.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/selfimprove"
)

type candidateResponse struct {
	ID               string `json:"id,omitempty"`
	CandidateClass   string `json:"candidate_class,omitempty"`
	Ref              string `json:"ref,omitempty"`
	Status           string `json:"status,omitempty"`
	GeneratedBy      string `json:"generated_by,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
	CanaryAt         string `json:"canary_at,omitempty"`
	PromotedAt       string `json:"promoted_at,omitempty"`
	DemotedAt        string `json:"demoted_at,omitempty"`
	RolledBackAt     string `json:"rolled_back_at,omitempty"`
	RollbackTargetID string `json:"rollback_target_id,omitempty"`
	Error            string `json:"error,omitempty"`
}

func toCandidateResponse(c *selfimprove.CandidateVersion) candidateResponse {
	return candidateResponse{
		ID: c.ID, CandidateClass: string(c.CandidateClass), Ref: c.Ref, Status: string(c.Status),
		GeneratedBy: c.GeneratedBy, CreatedAt: c.CreatedAt, CanaryAt: c.CanaryAt,
		PromotedAt: c.PromotedAt, DemotedAt: c.DemotedAt, RolledBackAt: c.RolledBackAt,
		RollbackTargetID: c.RollbackTargetID,
	}
}

type evalResponse struct {
	ID                 string         `json:"id,omitempty"`
	CandidateVersionID string         `json:"candidate_version_id,omitempty"`
	EvaluatorID        string         `json:"evaluator_id,omitempty"`
	EvaluatorVersion   string         `json:"evaluator_version,omitempty"`
	Metrics            map[string]any `json:"metrics,omitempty"`
	Passed             bool           `json:"passed"`
	EvaluatedAt        string         `json:"evaluated_at,omitempty"`
	Error              string         `json:"error,omitempty"`
}

func toEvalResponse(ev *selfimprove.Eval) evalResponse {
	return evalResponse{
		ID: ev.ID, CandidateVersionID: ev.CandidateVersionID, EvaluatorID: ev.EvaluatorID,
		EvaluatorVersion: ev.EvaluatorVersion, Metrics: ev.Metrics, Passed: ev.Passed, EvaluatedAt: ev.EvaluatedAt,
	}
}

func selfimproveErrorStatus(err error) int {
	switch {
	case errors.Is(err, selfimprove.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, selfimprove.ErrInvalidTransition), errors.Is(err, selfimprove.ErrNoCanaryEvidence),
		errors.Is(err, selfimprove.ErrNoRollbackTarget), errors.Is(err, selfimprove.ErrRollbackBlocked):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

type generateCandidateRequest struct {
	CandidateClass string `json:"candidate_class"`
	Ref            string `json:"ref"`
	GeneratedBy    string `json:"generated_by,omitempty"`
}

func (s *Server) handleGenerateCandidate(w http.ResponseWriter, r *http.Request) {
	var req generateCandidateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, candidateResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	c, err := s.SelfImprove.Generate(r.Context(), selfimprove.CandidateClass(req.CandidateClass), req.Ref, req.GeneratedBy)
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toCandidateResponse(c))
}

func (s *Server) handleGetCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Get(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}

func (s *Server) handleListCandidates(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	list, err := s.SelfImprove.List(r.Context(), selfimprove.ListFilter{
		Class:  selfimprove.CandidateClass(q.Get("candidate_class")),
		Status: selfimprove.Status(q.Get("status")),
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]candidateResponse, 0, len(list))
	for _, c := range list {
		out = append(out, toCandidateResponse(&c))
	}
	writeJSON(w, http.StatusOK, out)
}

type recordEvalRequest struct {
	EvaluatorID      string `json:"evaluator_id"`
	EvaluatorVersion string `json:"evaluator_version"`
	CaseResults      []bool `json:"case_results"`
}

func (s *Server) handleRecordEval(w http.ResponseWriter, r *http.Request) {
	var req recordEvalRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, evalResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	ev, err := s.SelfImprove.RecordEval(r.Context(), r.PathValue("candidateID"), req.EvaluatorID, req.EvaluatorVersion, req.CaseResults)
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), evalResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toEvalResponse(ev))
}

func (s *Server) handleCanaryCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Canary(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}

func (s *Server) handlePromoteCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Promote(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}

func (s *Server) handleDemoteCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Demote(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}

func (s *Server) handleRollbackCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Rollback(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}

func (s *Server) handleRejectCandidate(w http.ResponseWriter, r *http.Request) {
	c, err := s.SelfImprove.Reject(r.Context(), r.PathValue("candidateID"))
	if err != nil {
		writeJSON(w, selfimproveErrorStatus(err), candidateResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toCandidateResponse(c))
}
