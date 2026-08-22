// Package mcp is the server half of AMH's "MCP client/server
// interoperability" core responsibility (docs/AMH-SPECIFICATION.md
// §2.1) — completing what agents/harness/mcp_client.py already
// implements for the client half (stdio, consuming third-party
// servers). Roadmap item: "MCP server and A2A 1.0 adapter" (§14).
//
// This package exposes a fixed catalog of AMH's own capabilities (see
// tools.go) as MCP tools an external MCP client (Claude Desktop, another
// agent) can call — the daemon owns this the same way it owns every
// other local transport (decision 2: "Go owns... local transport"),
// dispatching directly into the same internal Go packages daemon/api
// itself calls rather than looping back through HTTP to reach them.
//
// # Protocol version
//
// Implements the Streamable HTTP transport from the MCP "Legacy" era
// (2025-03-26 through 2025-11-25, wire-compatible; this server declares
// 2025-06-18) — session-scoped, with an initialize/initialized
// handshake. Verified against the official specification
// (modelcontextprotocol.io) before writing any of this, not assumed.
//
// Deliberately NOT the newer 2026-07-28 ("Modern") transport, published
// one day before this package was written: it removes sessions and the
// initialize handshake entirely in favor of a per-request
// protocol-version declaration, which is not a field-level change but a
// structurally different, mutually incompatible wire protocol — a
// server would need to implement both eras' request handling side by
// side to speak to both kinds of client, which is real, separable
// follow-up work, not attempted here. The Legacy era is what virtually
// every MCP client actually deployed as of this writing still speaks
// (including the official Python SDK's own default negotiated version,
// 2025-03-26) — a spec-latest-but-practically-unreachable server would
// not serve this package's actual purpose, which is external clients
// genuinely being able to call into this habitat today.
//
// # Scope
//
// tools/list and tools/call only — no resources, prompts, sampling, or
// roots. Session state (map[sessionID]*session) lives in process memory
// and does not survive a daemon restart; a client whose session
// disappears gets 404 and re-initializes, which is the behavior the
// spec itself prescribes for an unknown session, not a bug. Instead of
// the MCP spec's optional OAuth 2.1 authorization framework (a separate,
// large protocol surface the spec itself says is optional to adopt),
// this server reuses the same daemon/authn bearer-token scheme every
// other route on this daemon already requires — an MCP client needs a
// real AMH agent/operator token, the same as any other caller.
package mcp

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
)

// protocolVersion is what this server declares in InitializeResult,
// regardless of what a client requests — see this package's doc comment.
const protocolVersion = "2025-06-18"

const serverName = "amh-daemon"

// sessionIDHeader and sessionIDBytes: 0x21-0x7E ASCII only, per the
// 2025-06-18 spec's constraint on Mcp-Session-Id — hex-encoding random
// bytes satisfies that trivially.
const sessionIDHeader = "Mcp-Session-Id"

type mcpSession struct {
	id        string
	createdAt time.Time
}

// Server hosts the MCP endpoint. Fields mirror the subset of
// daemon/api.Server's dependencies this package's tools actually need —
// see tools.go's handlers.
type Server struct {
	Addr   string
	DB     *sql.DB
	Tracer trace.TracerProvider
	Auth   *authn.Authenticator
	Log    *slog.Logger

	mu       sync.Mutex
	sessions map[string]*mcpSession

	srv *http.Server
}

func New(addr string, db *sql.DB, tp trace.TracerProvider, auth *authn.Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{
		Addr:     addr,
		DB:       db,
		Tracer:   tp,
		Auth:     auth,
		Log:      log,
		sessions: make(map[string]*mcpSession),
	}
}

// Handler builds the MCP endpoint's http.Handler — split out from Run so
// tests can exercise it directly via httptest, matching daemon/api's own
// pattern. Every method requires a valid agent or operator bearer token;
// which MCP method/tool was requested is decided inside handleMCP, not
// by the route table (there is only one route: the MCP endpoint itself).
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /mcp", s.Auth.RequireRole(s.handlePost, authn.RoleAgent, authn.RoleOperator))
	// No server-initiated messages (no subscriptions, no long-running
	// elicitation) — the 2025-06-18 spec explicitly allows a server with
	// nothing to push to refuse the SSE-stream GET with 405 rather than
	// implement a stream that never emits anything.
	mux.HandleFunc("GET /mcp", s.Auth.RequireRole(s.handleGet, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("DELETE /mcp", s.Auth.RequireRole(s.handleDelete, authn.RoleAgent, authn.RoleOperator))
	return mux
}

// Run blocks, serving the MCP endpoint, until ctx is cancelled. Matches
// supervisor.Child.Run's signature — one more supervised child of
// amh-daemon, alongside api and health.
func (s *Server) Run(ctx context.Context) error {
	s.srv = &http.Server{Addr: s.Addr, Handler: s.Handler()}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("mcp: listening", "addr", s.Addr)
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

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusMethodNotAllowed)
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get(sessionIDHeader)
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, ok := s.sessions[id]
	delete(s.sessions, id)
	s.mu.Unlock()
	if !ok {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	if ct := r.Header.Get("Content-Type"); ct != "" && !strings.HasPrefix(ct, "application/json") {
		http.Error(w, "Content-Type must be application/json", http.StatusUnsupportedMediaType)
		return
	}

	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusOK, errorResponse(nil, codeParseError, "parse error: "+err.Error()))
		return
	}

	if req.Method == "initialize" {
		s.handleInitialize(w, req)
		return
	}

	// Every other method requires a session established by a prior
	// initialize call — see this package's doc comment on why session
	// state is in-memory only.
	sessionID := r.Header.Get(sessionIDHeader)
	if sessionID == "" {
		http.Error(w, "missing "+sessionIDHeader+" header", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, ok := s.sessions[sessionID]
	s.mu.Unlock()
	if !ok {
		http.Error(w, "unknown or expired session", http.StatusNotFound)
		return
	}

	if req.isNotification() {
		// notifications/initialized is the only notification this server
		// expects; any notification (recognized or not) just gets
		// acknowledged with no body — a notification never gets a
		// JSON-RPC response by definition.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	switch req.Method {
	case "tools/list":
		s.handleToolsList(w, req)
	case "tools/call":
		s.handleToolsCall(w, r.Context(), req)
	default:
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeMethodNotFound, "method not found: "+req.Method))
	}
}

type initializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	ClientInfo      map[string]any `json:"clientInfo"`
}

func (s *Server) handleInitialize(w http.ResponseWriter, req request) {
	var params initializeParams
	if len(req.Params) > 0 {
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInvalidParams, "invalid initialize params: "+err.Error()))
			return
		}
	}

	id, err := newSessionID()
	if err != nil {
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInternalError, "could not create a session"))
		return
	}
	s.mu.Lock()
	s.sessions[id] = &mcpSession{id: id, createdAt: time.Now()}
	s.mu.Unlock()

	w.Header().Set(sessionIDHeader, id)
	writeJSON(w, http.StatusOK, resultResponse(req.ID, map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
		"serverInfo":      map[string]any{"name": serverName, "version": "1"},
	}))
}

func (s *Server) handleToolsList(w http.ResponseWriter, req request) {
	// No pagination: the catalog is small and fixed (see tools.go) — a
	// single response always returns everything, so nextCursor is never
	// set. If the catalog grows enough to need pagination, this is where
	// cursor/nextCursor would be threaded through.
	writeJSON(w, http.StatusOK, resultResponse(req.ID, map[string]any{"tools": tools}))
}

type toolsCallParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (s *Server) handleToolsCall(w http.ResponseWriter, ctx context.Context, req request) {
	var params toolsCallParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInvalidParams, "invalid tools/call params: "+err.Error()))
		return
	}
	t, ok := findTool(params.Name)
	if !ok {
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInvalidParams, "unknown tool: "+params.Name))
		return
	}
	arguments := params.Arguments
	if len(arguments) == 0 {
		arguments = json.RawMessage("{}")
	}
	result, err := t.handle(ctx, s, arguments)
	if err != nil {
		// A handler returns a non-nil error only for malformed arguments
		// this package itself cannot make sense of — that's a protocol-
		// level problem with the request, not a tool-level failure (see
		// toolResult's doc comment).
		writeJSON(w, http.StatusOK, errorResponse(req.ID, codeInvalidParams, err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, resultResponse(req.ID, result))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func newSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
