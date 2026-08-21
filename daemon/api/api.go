// Package api is the daemon-resident HTTP surface the Python agent layer
// calls to trigger device actuation — the persistent replacement for the
// amh-actuate CLI's per-call subprocess+SSH-dial pattern.
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
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
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

type Server struct {
	Addr     string
	DB       *sql.DB
	Registry *connectors.Registry
	Gate     *interlocks.Gate
	Tracer   trace.TracerProvider
	Log      *slog.Logger

	srv *http.Server
}

func New(addr string, db *sql.DB, tp trace.TracerProvider, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		Addr:     addr,
		DB:       db,
		Registry: connectors.NewRegistry(db),
		Gate:     interlocks.New(db),
		Tracer:   tp,
		Log:      log,
	}
}

// Handler builds the API's http.Handler — split out from Run so tests can
// exercise it directly via httptest without going through the
// supervisor.Child lifecycle.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/device-actions/{deviceActionID}/actuate", s.handleActuate)
	mux.HandleFunc("POST /v1/approval-gates", s.handleCreateTicket)
	mux.HandleFunc("POST /v1/approval-gates/{ticketID}/approve", s.handleApprove)
	mux.HandleFunc("GET /v1/approval-gates/{ticketID}", s.handleTicketStatus)
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

// handleApprove is the ONLY endpoint that can satisfy a ticket. It is
// deliberately dumb about who's allowed to call it — approved_by is
// recorded, not authenticated, here — because V0 has no operator-identity
// system yet (see caveat in docs/AMH-SPECIFICATION.md re: SafetyCase's
// independent_review role being deployment-specific). A real deployment
// puts an authn/authz layer in front of this endpoint; this package's job
// is the reversibility/approval bookkeeping, not identity.
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
