// Package api is the daemon-resident HTTP surface the Python agent layer
// calls to trigger device actuation — the persistent replacement for the
// amh-actuate CLI's per-call subprocess+SSH-dial pattern.
//
// Every route requires a bearer token (daemon/authn) — there is no
// unauthenticated mode. Which role a route accepts is the whole
// authorization policy for this package; see Handler's doc comment.
//
// Spec fidelity note: docs/AMH-SPECIFICATION.md Artifact A names
// contracts/proto (gRPC) as the daemon<->agent bridge. This is a
// deliberate substitute: protoc and the Go/Python gRPC plugin toolchain
// aren't available in this environment, and JSON-over-HTTP gets the same
// architectural property — a persistent daemon-owned connector registry
// instead of a process spawned per actuation — without a fragile codegen
// dependency. Same "ship the seam now, harden later" discipline the spec
// already applies to §7a/§14.6/§14.7: this package's request/response
// shape is what a future .proto file would formalize, not a different
// design. Migrating the wire format later doesn't change any of the
// logic in daemon/actuation this package calls into.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/actuation"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/safetycase"
)

type actuateRequest struct {
	Forward   string `json:"forward"`
	ReadState string `json:"read_state,omitempty"`
	TicketID  string `json:"ticket_id,omitempty"`
}

type actuateResponse struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type createTicketRequest struct {
	Action any    `json:"action"`
	Risk   string `json:"risk"`
}

type createTicketResponse struct {
	TicketID string `json:"ticket_id,omitempty"`
	Error    string `json:"error,omitempty"`
}

type approveRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type simpleResponse struct {
	Error string `json:"error,omitempty"`
}

type ticketStatusResponse struct {
	Satisfied bool   `json:"satisfied"`
	Error     string `json:"error,omitempty"`
}

type createSafetyCaseRequest struct {
	SubjectID   string `json:"subject_id"`
	SubjectType string `json:"subject_type"`
	RiskClass   string `json:"risk_class"`
}

type createSafetyCaseResponse struct {
	CaseID string `json:"case_id,omitempty"`
	Error  string `json:"error,omitempty"`
}

type submitEvidenceRequest struct {
	GuardrailProof map[string]any `json:"guardrail_proof"`
}

type approveSafetyCaseRequest struct {
	ApprovedBy string `json:"approved_by"`
}

type revokeSafetyCaseRequest struct {
	Reason string `json:"reason"`
}

type safetyCaseStatusResponse struct {
	CaseID            string `json:"case_id,omitempty"`
	SubjectID         string `json:"subject_id,omitempty"`
	SubjectType       string `json:"subject_type,omitempty"`
	RiskClass         string `json:"risk_class,omitempty"`
	IndependentReview bool   `json:"independent_review"`
	Approved          bool   `json:"approved"`
	Revoked           bool   `json:"revoked"`
	RevokedReason     string `json:"revoked_reason,omitempty"`
	Error             string `json:"error,omitempty"`
}

type Server struct {
	Addr       string
	DB         *sql.DB
	Registry   *connectors.Registry
	Gate       *interlocks.Gate
	SafetyCase *safetycase.Registry
	Tracer     trace.TracerProvider
	Auth       *authn.Authenticator
	Log        *slog.Logger

	srv *http.Server
}

// New wires the API's dependencies. auth is required — there is no
// unauthenticated mode; see amh-daemon's main.go, which refuses to start
// this server at all if its two role tokens aren't both configured.
func New(addr string, db *sql.DB, tp trace.TracerProvider, auth *authn.Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		Addr:       addr,
		DB:         db,
		Registry:   connectors.NewRegistry(db),
		Gate:       interlocks.New(db),
		SafetyCase: safetycase.New(db),
		Tracer:     tp,
		Auth:       auth,
		Log:        log,
	}
}

// Handler builds the API's http.Handler — split out from Run so tests can
// exercise it directly via httptest without going through the
// supervisor.Child lifecycle.
//
// Route-by-route role requirements are the entire authorization policy —
// read them here, not scattered across handler bodies:
//   - actuate, create-ticket, status: agent OR operator (routine agent work)
//   - approve: operator ONLY. An agent token is mechanically refused
//     (403) here — see daemon/authn's doc comment for why this is the
//     one property the whole package exists to enforce.
//   - safety-cases: create/evidence/status are agent OR operator
//     (routine agent work, same as the ApprovalGate's equivalents);
//     approve and revoke are operator ONLY — see daemon/safetycase's
//     doc comment for why the operator-only gate here IS the
//     independent review §14.7 requires, in V0's collapsed design.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/device-actions/{deviceActionID}/actuate",
		s.Auth.RequireRole(s.handleActuate, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/approval-gates",
		s.Auth.RequireRole(s.handleCreateTicket, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/approval-gates/{ticketID}/approve",
		s.Auth.RequireRole(s.handleApprove, authn.RoleOperator))
	mux.HandleFunc("GET /v1/approval-gates/{ticketID}",
		s.Auth.RequireRole(s.handleTicketStatus, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/safety-cases",
		s.Auth.RequireRole(s.handleCreateSafetyCase, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/safety-cases/{caseID}/evidence",
		s.Auth.RequireRole(s.handleSubmitSafetyCaseEvidence, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/safety-cases/{caseID}/approve",
		s.Auth.RequireRole(s.handleApproveSafetyCase, authn.RoleOperator))
	mux.HandleFunc("POST /v1/safety-cases/{caseID}/revoke",
		s.Auth.RequireRole(s.handleRevokeSafetyCase, authn.RoleOperator))
	mux.HandleFunc("GET /v1/safety-cases/{caseID}",
		s.Auth.RequireRole(s.handleSafetyCaseStatus, authn.RoleAgent, authn.RoleOperator))
	return mux
}

// Run blocks, serving the actuation API, until ctx is cancelled. Matches
// the supervisor.Child.Run signature — this is one more supervised child
// of amh-daemon, alongside scheduler and health.
func (s *Server) Run(ctx context.Context) error {
	s.srv = &http.Server{Addr: s.Addr, Handler: s.Handler()}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("api: listening", "addr", s.Addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleActuate(w http.ResponseWriter, r *http.Request) {
	deviceActionID := r.PathValue("deviceActionID")

	var req actuateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, actuateResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Forward == "" {
		writeJSON(w, http.StatusBadRequest, actuateResponse{Error: "forward is required"})
		return
	}

	act, err := s.Registry.ResolveActuator(r.Context(), deviceActionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, actuateResponse{Error: err.Error()})
		return
	}

	var ticket *interlocks.Ticket
	if req.TicketID != "" {
		ticket = &interlocks.Ticket{ID: req.TicketID}
	}

	result, err := actuation.ExecuteTraced(r.Context(), s.Tracer, s.DB, act, s.Gate, deviceActionID, actuation.Command{
		Forward:   req.Forward,
		ReadState: req.ReadState,
	}, ticket)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, actuation.ErrNoAutonomyPath) || errors.Is(err, interlocks.ErrNotSatisfied) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, actuateResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, actuateResponse{Result: result})
}

// handleCreateTicket lets a caller request approval for an action that
// has no verified inverse and no approved SafetyCase — the residue
// §12/v6 scopes the ApprovalGate to. Creating a ticket never grants
// anything by itself; it only exists so an agent-external authority (an
// operator today; a defined independent-reviewer role for a SafetyCase,
// per §14.7) has something concrete to approve via handleApprove.
func (s *Server) handleCreateTicket(w http.ResponseWriter, r *http.Request) {
	var req createTicketRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, createTicketResponse{Error: "invalid request body: " + err.Error()})
		return
	}

	risk := interlocks.Risk(req.Risk)
	if risk != interlocks.Reversible && risk != interlocks.Irreversible {
		writeJSON(w, http.StatusBadRequest, createTicketResponse{Error: "risk must be 'reversible' or 'irreversible'"})
		return
	}

	ticket, err := s.Gate.Require(r.Context(), req.Action, risk)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, createTicketResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, createTicketResponse{TicketID: ticket.ID})
}

// handleApprove is the ONLY endpoint that can satisfy a ticket, and the
// only route gated to authn.RoleOperator alone (see Handler's routing
// table) — an agent-role bearer token is refused with 403 before this
// function body ever runs. approved_by is still a free-text field, not a
// second identity check: it records WHICH operator approved (for audit),
// while the bearer token is what proves the caller IS an operator at
// all. V0 has one static operator token, not a multi-operator identity
// system (see caveat in docs/AMH-SPECIFICATION.md re: SafetyCase's
// independent_review role being deployment-specific) — approved_by lets
// that distinction exist in the audit trail even though this package
// can't yet verify it cryptographically.
func (s *Server) handleApprove(w http.ResponseWriter, r *http.Request) {
	ticketID := r.PathValue("ticketID")

	var req approveRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.ApprovedBy == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "approved_by is required"})
		return
	}

	if err := s.Gate.Approve(r.Context(), interlocks.Ticket{ID: ticketID}, req.ApprovedBy); err != nil {
		writeJSON(w, http.StatusConflict, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

func (s *Server) handleTicketStatus(w http.ResponseWriter, r *http.Request) {
	ticketID := r.PathValue("ticketID")
	satisfied, err := s.Gate.IsSatisfied(r.Context(), interlocks.Ticket{ID: ticketID})
	if err != nil {
		writeJSON(w, http.StatusNotFound, ticketStatusResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, ticketStatusResponse{Satisfied: satisfied})
}

// handleCreateSafetyCase opens a new SafetyCase for a subject that has no
// verified inverse — the harder evidence path to earned autonomy for
// irreversible/high-consequence actions (§14.7). Creating a case grants
// nothing by itself; evidence accumulates via handleSubmitSafetyCaseEvidence
// and only handleApproveSafetyCase (operator-only) can make it load-bearing.
func (s *Server) handleCreateSafetyCase(w http.ResponseWriter, r *http.Request) {
	var req createSafetyCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, createSafetyCaseResponse{Error: "invalid request body: " + err.Error()})
		return
	}

	caseID, err := s.SafetyCase.Create(r.Context(), req.SubjectID, safetycase.SubjectType(req.SubjectType), safetycase.RiskClass(req.RiskClass))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, safetycase.ErrInvalidRiskClass) || errors.Is(err, safetycase.ErrInvalidSubjectType) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, createSafetyCaseResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, createSafetyCaseResponse{CaseID: caseID})
}

func (s *Server) handleSubmitSafetyCaseEvidence(w http.ResponseWriter, r *http.Request) {
	caseID := r.PathValue("caseID")

	var req submitEvidenceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}

	if err := s.SafetyCase.SubmitEvidence(r.Context(), caseID, req.GuardrailProof); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, safetycase.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeJSON(w, status, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

// handleApproveSafetyCase is gated to authn.RoleOperator alone (see
// Handler's routing table) — this IS the independent review §14.7
// requires in V0's collapsed design; see daemon/safetycase's doc comment.
func (s *Server) handleApproveSafetyCase(w http.ResponseWriter, r *http.Request) {
	caseID := r.PathValue("caseID")

	var req approveSafetyCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.ApprovedBy == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "approved_by is required"})
		return
	}

	if err := s.SafetyCase.Approve(r.Context(), caseID, req.ApprovedBy); err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, safetycase.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, safetycase.ErrAlreadyApproved), errors.Is(err, safetycase.ErrAlreadyRevoked):
			status = http.StatusConflict
		}
		writeJSON(w, status, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

// handleRevokeSafetyCase is gated to authn.RoleOperator alone. Per §14.7
// this is immediate and final — no rate window, and the revoked row is
// never re-approved (see daemon/safetycase.Registry.Approve).
func (s *Server) handleRevokeSafetyCase(w http.ResponseWriter, r *http.Request) {
	caseID := r.PathValue("caseID")

	var req revokeSafetyCaseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Reason == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "reason is required"})
		return
	}

	if err := s.SafetyCase.Revoke(r.Context(), caseID, req.Reason); err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, safetycase.ErrNotFound):
			status = http.StatusNotFound
		case errors.Is(err, safetycase.ErrAlreadyRevoked), errors.Is(err, safetycase.ErrNotApproved):
			status = http.StatusConflict
		}
		writeJSON(w, status, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

func (s *Server) handleSafetyCaseStatus(w http.ResponseWriter, r *http.Request) {
	caseID := r.PathValue("caseID")
	status, err := s.SafetyCase.Status(r.Context(), caseID)
	if err != nil {
		httpStatus := http.StatusInternalServerError
		if errors.Is(err, safetycase.ErrNotFound) {
			httpStatus = http.StatusNotFound
		}
		writeJSON(w, httpStatus, safetyCaseStatusResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, safetyCaseStatusResponse{
		CaseID:            status.ID,
		SubjectID:         status.SubjectID,
		SubjectType:       string(status.SubjectType),
		RiskClass:         string(status.RiskClass),
		IndependentReview: status.IndependentReview,
		Approved:          status.Approved,
		Revoked:           status.Revoked,
		RevokedReason:     status.RevokedReason,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
