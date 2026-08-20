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

func writeJSON(w http.ResponseWriter, status int, body actuateResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
