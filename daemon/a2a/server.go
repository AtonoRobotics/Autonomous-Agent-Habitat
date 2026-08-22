package a2a

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
)

// agentCardPath is the well-known discovery path every A2A client fetches
// first, unauthenticated by protocol convention — a client that doesn't
// yet hold a bearer token still needs to be able to learn that it needs
// one (via the card's SecuritySchemes), which an authenticated-only
// discovery endpoint would make impossible.
const agentCardPath = "/.well-known/agent-card.json"

// HTTPAuthSecurityScheme mirrors A2A's message of the same name — the
// only SecurityScheme variant this package populates (see the package
// doc comment's Scope section: no API-key/OAuth2/OIDC/mTLS schemes are
// offered).
type HTTPAuthSecurityScheme struct {
	Description  string `json:"description,omitempty"`
	Scheme       string `json:"scheme"`
	BearerFormat string `json:"bearerFormat,omitempty"`
}

// SecurityScheme mirrors A2A's discriminated-union SecurityScheme message
// — protojson serializes a proto oneof's chosen variant under that
// variant's own JSON field name, so a real HTTPAuthSecurityScheme value
// appears here as {"httpAuthSecurityScheme": {...}}.
type SecurityScheme struct {
	HTTPAuthSecurityScheme *HTTPAuthSecurityScheme `json:"httpAuthSecurityScheme,omitempty"`
}

// SecurityRequirement mirrors A2A's SecurityRequirement message.
type SecurityRequirement struct {
	Schemes map[string][]string `json:"schemes"`
}

type AgentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	ProtocolVersion string `json:"protocolVersion"`
}

type AgentCapabilities struct {
	Streaming         bool `json:"streaming"`
	PushNotifications bool `json:"pushNotifications"`
	ExtendedAgentCard bool `json:"extendedAgentCard"`
}

type AgentSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	InputModes  []string `json:"inputModes,omitempty"`
	OutputModes []string `json:"outputModes,omitempty"`
}

// AgentCard mirrors A2A's AgentCard message — see the package doc
// comment. Provider, DocumentationURL, IconURL, and Signatures are
// omitted entirely (all optional per the proto) rather than populated
// with placeholder values.
type AgentCard struct {
	Name                 string                    `json:"name"`
	Description          string                    `json:"description"`
	SupportedInterfaces  []AgentInterface          `json:"supportedInterfaces"`
	Version              string                    `json:"version"`
	Capabilities         AgentCapabilities         `json:"capabilities"`
	SecuritySchemes      map[string]SecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []SecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                  `json:"defaultInputModes"`
	DefaultOutputModes   []string                  `json:"defaultOutputModes"`
	Skills               []AgentSkill              `json:"skills"`
}

// buildAgentCard describes this habitat as a single-skill A2A agent.
// Version "1.0.0" here names THIS AGENT CARD's own version (a required
// A2A field, distinct in kind from AMH's own "no staged rollout, no
// software version" posture — see docs/AMH-SPECIFICATION.md §14): it
// does not reintroduce release versioning for AMH itself.
func buildAgentCard(publicURL string) AgentCard {
	return AgentCard{
		Name:        "AMH Habitat",
		Description: "Autonomous Multi-Agent Habitat: accepts a natural-language goal and pursues it durably via AMH's goal-pursuit workflow.",
		SupportedInterfaces: []AgentInterface{
			{URL: publicURL, ProtocolBinding: "HTTP+JSON", ProtocolVersion: "1.0"},
		},
		Version: "1.0.0",
		Capabilities: AgentCapabilities{
			Streaming: false, PushNotifications: false, ExtendedAgentCard: false,
		},
		SecuritySchemes: map[string]SecurityScheme{
			"bearer": {HTTPAuthSecurityScheme: &HTTPAuthSecurityScheme{
				Scheme:      "Bearer",
				Description: "AMH agent or operator bearer token (daemon/authn) — the same token used for every other daemon route. Per-external-caller identity is not yet implemented; see the package doc comment.",
			}},
		},
		SecurityRequirements: []SecurityRequirement{{Schemes: map[string][]string{"bearer": {}}}},
		DefaultInputModes:    []string{"text/plain"},
		DefaultOutputModes:   []string{"text/plain"},
		Skills: []AgentSkill{
			{
				ID:          "pursue-goal",
				Name:        "Pursue a goal",
				Description: "Accepts a natural-language goal and works it autonomously through AMH's durable goal-pursuit workflow (decomposition, subordinate agents, synthesis).",
				Tags:        []string{"goal", "autonomous"},
				InputModes:  []string{"text/plain"},
				OutputModes: []string{"text/plain"},
			},
		},
	}
}

// Server hosts the A2A HTTP+JSON endpoint.
type Server struct {
	Addr      string
	PublicURL string
	Store     *Store
	Tracer    trace.TracerProvider
	Auth      *authn.Authenticator
	Log       *slog.Logger

	srv *http.Server
}

func New(addr, publicURL string, store *Store, tp trace.TracerProvider, auth *authn.Authenticator, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Addr: addr, PublicURL: publicURL, Store: store, Tracer: tp, Auth: auth, Log: log}
}

// Handler builds the A2A endpoint's http.Handler — split out from Run so
// tests can exercise it directly via httptest, matching daemon/api's and
// daemon/mcp's own pattern. Agent Card discovery is unauthenticated (see
// agentCardPath's doc comment); every operational route requires an
// agent-or-operator bearer token, same as the rest of this daemon.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+agentCardPath, s.handleAgentCard)
	mux.HandleFunc("POST /message:send",
		s.Auth.RequireRole(s.handleSendMessage, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /tasks", s.Auth.RequireRole(s.handleListTasks, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /tasks/{id}", s.Auth.RequireRole(s.handleGetTask, authn.RoleAgent, authn.RoleOperator))
	// AIP-136 custom methods (":cancel") aren't representable as a
	// separate literal suffix in Go's net/http ServeMux pattern syntax —
	// {id} greedily captures everything up to the next "/", colon
	// included. This single POST /tasks/{id} route (there is no A2A
	// operation for a bare "POST /tasks/{id}") disambiguates the one
	// custom method this package implements by checking for the literal
	// ":cancel" suffix inside the handler instead.
	mux.HandleFunc("POST /tasks/{id}",
		s.Auth.RequireRole(s.handleTasksPost, authn.RoleAgent, authn.RoleOperator))
	return mux
}

// Run blocks, serving the A2A endpoint, until ctx is cancelled. Matches
// supervisor.Child.Run's signature — one more supervised child of
// amh-daemon, alongside api, mcp, and health.
func (s *Server) Run(ctx context.Context) error {
	s.srv = &http.Server{Addr: s.Addr, Handler: s.Handler()}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("a2a: listening", "addr", s.Addr)
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

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type errorBody struct {
	Error string `json:"error"`
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, buildAgentCard(s.PublicURL))
}

// SendMessageRequest mirrors A2A's message of the same name — Tenant and
// Configuration are not implemented (see package doc comment) and simply
// ignored if present, rather than rejected, matching AIP-136's guidance
// that servers tolerate fields they don't act on.
type sendMessageRequest struct {
	Message Message `json:"message"`
}

type sendMessageResponse struct {
	Task *Task `json:"task,omitempty"`
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request) {
	var req sendMessageRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody{"invalid request body: " + err.Error()})
		return
	}
	if req.Message.TaskID != "" {
		writeJSON(w, http.StatusBadRequest, errorBody{"continuing an existing task is not implemented — omit taskId to start a new task"})
		return
	}
	task, err := s.Store.CreateTaskFromMessage(r.Context(), req.Message)
	if err != nil {
		writeJSON(w, sendMessageErrorStatus(err), errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, sendMessageResponse{Task: task})
}

func sendMessageErrorStatus(err error) int {
	if errors.Is(err, ErrNoTextContent) {
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request) {
	task, err := s.Store.GetTask(r.Context(), r.PathValue("id"))
	if err != nil {
		writeJSON(w, taskErrorStatus(err), errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, task)
}

func taskErrorStatus(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrNotCancelable):
		return http.StatusConflict
	default:
		return http.StatusInternalServerError
	}
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := ListFilter{
		ContextID: q.Get("contextId"),
		Status:    TaskState(q.Get("status")),
		PageToken: q.Get("pageToken"),
	}
	if raw := q.Get("pageSize"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			filter.PageSize = n
		}
	}
	result, err := s.Store.ListTasks(r.Context(), filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"tasks":         result.Tasks,
		"nextPageToken": result.NextPageToken,
		"pageSize":      len(result.Tasks),
		"totalSize":     result.TotalSize,
	})
}

// handleTasksPost is the single POST /tasks/{id} route — see Handler's
// doc comment on why the ":cancel" custom method is dispatched here
// rather than on its own registered pattern.
func (s *Server) handleTasksPost(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(r.PathValue("id"), ":cancel")
	if !ok {
		writeJSON(w, http.StatusNotFound, errorBody{"unknown operation — POST /tasks/{id} only supports the :cancel custom method"})
		return
	}
	task, err := s.Store.CancelTask(r.Context(), id)
	if err != nil {
		writeJSON(w, taskErrorStatus(err), errorBody{err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, task)
}
