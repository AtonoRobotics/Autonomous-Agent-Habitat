// Package api is the daemon-resident HTTP surface the Python agent layer
// calls to trigger device actuation — the persistent replacement for the
// amh-actuate CLI's per-call subprocess+SSH-dial pattern.
//
// Every route requires a bearer token (daemon/authn) — there is no
// unauthenticated mode. Which role a route accepts is the whole
// authorization policy for this package; see Handler's doc comment.
//
// Spec fidelity note: docs/AMH-SPECIFICATION.md Artifact A names
// contracts/proto (gRPC) as the daemon<->agent bridge. This package uses
// JSON-over-HTTP instead: protoc and the Go/Python gRPC plugin toolchain
// aren't available in this environment, and JSON-over-HTTP gets the same
// architectural property — a persistent daemon-owned connector registry
// instead of a process spawned per actuation — without a fragile codegen
// dependency. This package's request/response shape is what a .proto file
// would formalize if the transport changes; that migration would not
// touch any of the logic in daemon/actuation this package calls into.
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
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/inference"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/safetycase"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/sandbox"
)

type actuateRequest struct {
	Params   map[string]string `json:"params"`
	TicketID string            `json:"ticket_id,omitempty"`
}

type actuateResponse struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type createTicketRequest struct {
	DeviceActionID string            `json:"device_action_id"`
	Params         map[string]string `json:"params"`
	Reason         string            `json:"reason,omitempty"`
	Risk           string            `json:"risk"`
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
	Addr        string
	DB          *sql.DB
	Registry    *connectors.Registry
	Gate        *interlocks.Gate
	SafetyCase  *safetycase.Registry
	Extensions  *extensions.Registry
	Sandbox     *sandbox.Provisioner
	Credentials *credentials.Store
	Inference   *inference.Router
	Tracer      trace.TracerProvider
	Auth        *authn.Authenticator
	Log         *slog.Logger

	srv *http.Server
}

// New wires the API's dependencies. auth is required — there is no
// unauthenticated mode; see amh-daemon's main.go, which refuses to start
// this server at all if its two role tokens aren't both configured.
// creds may be nil (control-plane account/credential routes are then
// unavailable) — see main.go, which treats a missing AMH_CREDENTIAL_KEY as
// a soft-disable of just that surface, not a refusal to start, since
// device actuation and the extension/sandbox surfaces don't depend on it.
func New(addr string, db *sql.DB, tp trace.TracerProvider, auth *authn.Authenticator, log *slog.Logger, sandboxBaseDir string, creds *credentials.Store) *Server {
	if log == nil {
		log = slog.Default()
	}
	var inferenceRouter *inference.Router
	if creds != nil {
		inferenceRouter = inference.New(creds)
	}
	return &Server{
		Addr:        addr,
		DB:          db,
		Registry:    connectors.NewRegistry(db),
		Gate:        interlocks.New(db),
		SafetyCase:  safetycase.New(db),
		Extensions:  extensions.New(db),
		Sandbox:     sandbox.New(db, sandboxBaseDir),
		Credentials: creds,
		Inference:   inferenceRouter,
		Tracer:      tp,
		Auth:        auth,
		Log:         log,
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
//     independent review §14.7 requires.
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

	// Control plane: extensions, computers, connectors, accounts. See
	// controlplane.go for handler bodies and the doc comment there for why
	// extension id/version travel in the request body or query string
	// rather than the URL path (namespaced extension ids contain "/",
	// which Go's ServeMux path segments cannot represent).
	//
	// Extension mutations (discover/activate/quiesce/dispose) are
	// operator-only: activating an extension runs new code with
	// daemon-level reach, the exact class of action decision 9
	// ("agents propose; deterministic services commit") reserves for a
	// deterministic, human-gated path rather than autonomous agent action.
	mux.HandleFunc("POST /v1/extensions",
		s.Auth.RequireRole(s.handleDiscoverExtension, authn.RoleOperator))
	mux.HandleFunc("POST /v1/extensions/activate",
		s.Auth.RequireRole(s.handleActivateExtension, authn.RoleOperator))
	mux.HandleFunc("POST /v1/extensions/quiesce",
		s.Auth.RequireRole(s.handleQuiesceExtension, authn.RoleOperator))
	mux.HandleFunc("POST /v1/extensions/dispose",
		s.Auth.RequireRole(s.handleDisposeExtension, authn.RoleOperator))
	mux.HandleFunc("GET /v1/extensions",
		s.Auth.RequireRole(s.handleListExtensions, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/extensions/get",
		s.Auth.RequireRole(s.handleGetExtension, authn.RoleAgent, authn.RoleOperator))

	// Computers: agent OR operator for both create and destroy — a
	// computer's Create/Destroy pair is always reversible by construction
	// (daemon/sandbox), so per §12/v6 (reversibility is the sole gating
	// axis) an agent provisioning or tearing down its own compute instance
	// needs no operator gate, unlike installing an extension.
	mux.HandleFunc("POST /v1/computers",
		s.Auth.RequireRole(s.handleCreateComputer, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/computers/{computerID}/destroy",
		s.Auth.RequireRole(s.handleDestroyComputer, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/computers/{computerID}",
		s.Auth.RequireRole(s.handleGetComputer, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/computers",
		s.Auth.RequireRole(s.handleListComputers, authn.RoleAgent, authn.RoleOperator))

	// Connectors: create/disable are operator-only (a connector carries
	// network reach and, via account_id, may carry credential access).
	mux.HandleFunc("POST /v1/connectors",
		s.Auth.RequireRole(s.handleCreateConnector, authn.RoleOperator))
	mux.HandleFunc("POST /v1/connectors/{connectorID}/disable",
		s.Auth.RequireRole(s.handleDisableConnector, authn.RoleOperator))
	mux.HandleFunc("GET /v1/connectors",
		s.Auth.RequireRole(s.handleListConnectors, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/connectors/{connectorID}",
		s.Auth.RequireRole(s.handleGetConnector, authn.RoleAgent, authn.RoleOperator))

	// Accounts: creating an account, storing/rotating its credential
	// ("authenticate an account"), and revoking are all operator-only —
	// secret material and external identity are exactly the actions
	// decision 9 reserves from autonomous agent action. Reads return
	// metadata only (daemon/credentials.Account never carries a secret).
	mux.HandleFunc("POST /v1/accounts",
		s.Auth.RequireRole(s.handleCreateAccount, authn.RoleOperator))
	mux.HandleFunc("POST /v1/accounts/{accountID}/credential",
		s.Auth.RequireRole(s.handlePutAccountCredential, authn.RoleOperator))
	mux.HandleFunc("POST /v1/accounts/{accountID}/revoke",
		s.Auth.RequireRole(s.handleRevokeAccount, authn.RoleOperator))
	mux.HandleFunc("GET /v1/accounts",
		s.Auth.RequireRole(s.handleListAccounts, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/accounts/{accountID}",
		s.Auth.RequireRole(s.handleGetAccount, authn.RoleAgent, authn.RoleOperator))

	// Inference: agent OR operator, same tier as actuation — this is the
	// model-provider seam (docs/AMH-SPECIFICATION.md §2.1) an ephemeral
	// agent computer calls into instead of holding a model credential
	// itself. See daemon/inference and controlplane.go's handlers.
	mux.HandleFunc("POST /v1/inference/complete",
		s.Auth.RequireRole(s.handleInferenceComplete, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/inference/count-tokens",
		s.Auth.RequireRole(s.handleInferenceCountTokens, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/inference/embed",
		s.Auth.RequireRole(s.handleInferenceEmbed, authn.RoleAgent, authn.RoleOperator))

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

	act, err := s.Registry.ResolveActuator(r.Context(), deviceActionID)
	if err != nil {
		status := http.StatusNotFound
		if errors.Is(err, connectors.ErrConnectorDisabled) {
			status = http.StatusForbidden
		}
		writeJSON(w, status, actuateResponse{Error: err.Error()})
		return
	}

	var ticket *interlocks.Ticket
	if req.TicketID != "" {
		ticket = &interlocks.Ticket{ID: req.TicketID}
	}

	result, err := actuation.ExecuteTraced(r.Context(), s.Tracer, s.DB, act, s.Gate, deviceActionID, actuation.Command{
		Params: req.Params,
	}, ticket)
	if err != nil {
		status := http.StatusInternalServerError
		switch {
		case errors.Is(err, actuation.ErrNoAutonomyPath),
			errors.Is(err, interlocks.ErrNotSatisfied),
			errors.Is(err, interlocks.ErrActionMismatch),
			errors.Is(err, interlocks.ErrTicketAlreadyUsed):
			status = http.StatusForbidden
		case errors.Is(err, actuation.ErrInvalidParam):
			status = http.StatusBadRequest
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
	if req.DeviceActionID == "" {
		writeJSON(w, http.StatusBadRequest, createTicketResponse{Error: "device_action_id is required"})
		return
	}

	ticket, err := s.Gate.Require(r.Context(), req.DeviceActionID, req.Params, req.Reason, risk)
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
// all. There is one static operator token, not a multi-operator identity
// system (see caveat in docs/AMH-SPECIFICATION.md re: SafetyCase's
// independent_review role being deployment-specific) — approved_by lets
// that distinction exist in the audit trail even though this package
// cannot verify it cryptographically.
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
// requires; see daemon/safetycase's doc comment.
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
