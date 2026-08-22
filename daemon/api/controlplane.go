// Control-plane routes: install harnesses, provision computers, and
// authenticate accounts and modules — the admin surface the control-plane
// UI extension (extensions/control-plane-ui) is built against, and the
// seam any other operator tool could use instead.
//
// Extension ids are namespaced ("amh.control-plane/ui") and contain a
// literal "/", which Go's net/http ServeMux cannot represent inside a
// single {wildcard} path segment (and a trailing {id...} wildcard can't be
// followed by a version segment and an action). Rather than URL-encoding
// around that, extension id/version travel in the JSON body for mutations
// and as query parameters for the two GET routes — computers and accounts
// use plain UUIDs and keep the path-segment style the rest of this
// package already uses.
package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/inference"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/sandbox"
)

// ── Extensions ───────────────────────────────────────────────────────────

type extensionResponse struct {
	ID            string `json:"id,omitempty"`
	Version       string `json:"version,omitempty"`
	Name          string `json:"name,omitempty"`
	Publisher     string `json:"publisher,omitempty"`
	Isolation     string `json:"isolation,omitempty"`
	Status        string `json:"status,omitempty"`
	StatusReason  string `json:"status_reason,omitempty"`
	RuntimeHandle string `json:"runtime_handle,omitempty"`
	Error         string `json:"error,omitempty"`
}

func toExtensionResponse(e *extensions.Extension) extensionResponse {
	return extensionResponse{
		ID: e.ID, Version: e.Version, Name: e.Name, Publisher: e.Publisher,
		Isolation: string(e.Isolation), Status: string(e.Status),
		StatusReason: e.StatusReason, RuntimeHandle: e.RuntimeHandle,
	}
}

func extensionErrorStatus(err error) int {
	switch {
	case errors.Is(err, extensions.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, extensions.ErrAlreadyExists), errors.Is(err, extensions.ErrInvalidState),
		errors.Is(err, extensions.ErrMissingRequirement), errors.Is(err, extensions.ErrActiveDependents):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

// handleDiscoverExtension registers a manifest — this is "install a
// harness" (or a knowledge-base, memory, model-provider, or connector
// extension: the registry has no domain-specific notion of "harness," only
// of extensions and their declared capabilities).
func (s *Server) handleDiscoverExtension(w http.ResponseWriter, r *http.Request) {
	var m extensions.Manifest
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		writeJSON(w, http.StatusBadRequest, extensionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	ext, err := s.Extensions.Discover(r.Context(), m)
	if err != nil {
		writeJSON(w, extensionErrorStatus(err), extensionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toExtensionResponse(ext))
}

type extensionRefRequest struct {
	ID      string `json:"id"`
	Version string `json:"version"`
}

func (s *Server) handleActivateExtension(w http.ResponseWriter, r *http.Request) {
	var req extensionRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, extensionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	ext, err := s.Extensions.Activate(r.Context(), req.ID, req.Version)
	if err != nil {
		writeJSON(w, extensionErrorStatus(err), extensionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toExtensionResponse(ext))
}

func (s *Server) handleQuiesceExtension(w http.ResponseWriter, r *http.Request) {
	var req extensionRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, extensionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	ext, err := s.Extensions.Quiesce(r.Context(), req.ID, req.Version)
	if err != nil {
		writeJSON(w, extensionErrorStatus(err), extensionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toExtensionResponse(ext))
}

// handleDisposeExtension is "uninstall a harness" (or any other
// extension) — the verified inverse of handleDiscoverExtension+Activate,
// not a database delete: the row and its full effect history remain,
// only the running instance is torn down. This is what makes the module
// system Cordis-style removable-and-reversible rather than merely
// deletable.
func (s *Server) handleDisposeExtension(w http.ResponseWriter, r *http.Request) {
	var req extensionRefRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, extensionResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	ext, err := s.Extensions.Dispose(r.Context(), req.ID, req.Version)
	if err != nil {
		writeJSON(w, extensionErrorStatus(err), extensionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toExtensionResponse(ext))
}

func (s *Server) handleGetExtension(w http.ResponseWriter, r *http.Request) {
	id, version := r.URL.Query().Get("id"), r.URL.Query().Get("version")
	ext, err := s.Extensions.Get(r.Context(), id, version)
	if err != nil {
		writeJSON(w, extensionErrorStatus(err), extensionResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toExtensionResponse(ext))
}

func (s *Server) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	list, err := s.Extensions.List(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]extensionResponse, 0, len(list))
	for _, e := range list {
		out = append(out, toExtensionResponse(e))
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Computers ────────────────────────────────────────────────────────────

type createComputerRequest struct {
	AgentID        string            `json:"agent_id"`
	Isolation      string            `json:"isolation"`
	Image          string            `json:"image"`
	ResourceLimits map[string]string `json:"resource_limits,omitempty"`
}

type computerResponse struct {
	ID            string `json:"id,omitempty"`
	AgentID       string `json:"agent_id,omitempty"`
	Isolation     string `json:"isolation,omitempty"`
	Image         string `json:"image,omitempty"`
	Status        string `json:"status,omitempty"`
	RuntimeHandle string `json:"runtime_handle,omitempty"`
	Workdir       string `json:"workdir,omitempty"`
	DestroyReason string `json:"destroy_reason,omitempty"`
	Error         string `json:"error,omitempty"`
}

func toComputerResponse(c *sandbox.Computer) computerResponse {
	return computerResponse{
		ID: c.ID, AgentID: c.AgentID, Isolation: string(c.Isolation), Image: c.Image,
		Status: string(c.Status), RuntimeHandle: c.RuntimeHandle, Workdir: c.Workdir,
		DestroyReason: c.DestroyReason,
	}
}

func computerErrorStatus(err error) int {
	switch {
	case errors.Is(err, sandbox.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, sandbox.ErrInvalidState):
		return http.StatusConflict
	default:
		return http.StatusBadRequest
	}
}

func (s *Server) handleCreateComputer(w http.ResponseWriter, r *http.Request) {
	var req createComputerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, computerResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	c, err := s.Sandbox.Create(r.Context(), req.AgentID, sandbox.Isolation(req.Isolation), req.Image, req.ResourceLimits)
	if err != nil {
		writeJSON(w, computerErrorStatus(err), computerResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toComputerResponse(c))
}

type destroyComputerRequest struct {
	Reason string `json:"reason"`
}

func (s *Server) handleDestroyComputer(w http.ResponseWriter, r *http.Request) {
	computerID := r.PathValue("computerID")
	var req destroyComputerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, computerResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	c, err := s.Sandbox.Destroy(r.Context(), computerID, req.Reason)
	if err != nil {
		writeJSON(w, computerErrorStatus(err), computerResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toComputerResponse(c))
}

func (s *Server) handleGetComputer(w http.ResponseWriter, r *http.Request) {
	c, err := s.Sandbox.Get(r.Context(), r.PathValue("computerID"))
	if err != nil {
		writeJSON(w, computerErrorStatus(err), computerResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toComputerResponse(c))
}

func (s *Server) handleListComputers(w http.ResponseWriter, r *http.Request) {
	agentID := r.URL.Query().Get("agent_id")
	if agentID == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "agent_id query parameter is required"})
		return
	}
	list, err := s.Sandbox.ListForAgent(r.Context(), agentID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]computerResponse, 0, len(list))
	for _, c := range list {
		out = append(out, toComputerResponse(c))
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Accounts (authenticate accounts and modules) ────────────────────────

type createAccountRequest struct {
	Provider    string `json:"provider"`
	DisplayName string `json:"display_name,omitempty"`
}

type accountResponse struct {
	ID          string `json:"id,omitempty"`
	Provider    string `json:"provider,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
	Status      string `json:"status,omitempty"`
	Error       string `json:"error,omitempty"`
}

func toAccountResponse(a *credentials.Account) accountResponse {
	return accountResponse{ID: a.ID, Provider: a.Provider, DisplayName: a.DisplayName, Status: string(a.Status)}
}

// credentialsUnavailable answers a request with 503 when the daemon was
// started without AMH_CREDENTIAL_KEY — see New's doc comment. Every
// account/credential handler below checks this first.
func (s *Server) credentialsUnavailable(w http.ResponseWriter) bool {
	if s.Credentials != nil {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, simpleResponse{Error: "credential store is not configured (AMH_CREDENTIAL_KEY unset) — account/credential routes are disabled"})
	return true
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if s.credentialsUnavailable(w) {
		return
	}
	var req createAccountRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, accountResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	acct, err := s.Credentials.CreateAccount(r.Context(), req.Provider, req.DisplayName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, accountResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, toAccountResponse(acct))
}

type putCredentialRequest struct {
	Secret string `json:"secret"`
}

// handlePutAccountCredential is "authenticate an account" — storing the
// credential that lets the daemon act as that external account on the
// account owner's behalf. The secret is never echoed back, here or in any
// GET route.
func (s *Server) handlePutAccountCredential(w http.ResponseWriter, r *http.Request) {
	if s.credentialsUnavailable(w) {
		return
	}
	accountID := r.PathValue("accountID")
	var req putCredentialRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Secret == "" {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: "secret is required"})
		return
	}
	if _, err := s.Credentials.PutCredential(r.Context(), credentials.SubjectAccount, accountID, []byte(req.Secret)); err != nil {
		writeJSON(w, http.StatusBadRequest, simpleResponse{Error: err.Error()})
		return
	}
	acct, err := s.Credentials.GetAccount(r.Context(), accountID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acct))
}

func (s *Server) handleRevokeAccount(w http.ResponseWriter, r *http.Request) {
	if s.credentialsUnavailable(w) {
		return
	}
	acct, err := s.Credentials.RevokeAccount(r.Context(), r.PathValue("accountID"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, accountResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acct))
}

func (s *Server) handleGetAccount(w http.ResponseWriter, r *http.Request) {
	if s.credentialsUnavailable(w) {
		return
	}
	acct, err := s.Credentials.GetAccount(r.Context(), r.PathValue("accountID"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, accountResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, toAccountResponse(acct))
}

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	if s.credentialsUnavailable(w) {
		return
	}
	list, err := s.Credentials.ListAccounts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	out := make([]accountResponse, 0, len(list))
	for _, a := range list {
		out = append(out, toAccountResponse(a))
	}
	writeJSON(w, http.StatusOK, out)
}

// ── Inference (the model-provider seam) ─────────────────────────────────
//
// An agent process calls these with only its agent bearer token — it
// never holds a model-provider credential itself. The daemon resolves
// Provider to a registered account (created via /v1/accounts +
// /v1/accounts/{id}/credential, exactly like a GitHub or Gmail account)
// and makes the real call. See daemon/inference.

type inferenceMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type inferenceCompleteRequest struct {
	Provider  string             `json:"provider,omitempty"`
	Providers []string           `json:"providers,omitempty"`
	Model     string             `json:"model"`
	System    string             `json:"system,omitempty"`
	Messages  []inferenceMessage `json:"messages"`
	MaxTokens int                `json:"max_tokens,omitempty"`
}

type inferenceCompleteResponse struct {
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

type inferenceCountTokensResponse struct {
	InputTokens int    `json:"input_tokens,omitempty"`
	Error       string `json:"error,omitempty"`
}

// inferenceUnavailable mirrors credentialsUnavailable: inference depends
// on the same credential store, so the same configuration gate applies.
func (s *Server) inferenceUnavailable(w http.ResponseWriter) bool {
	if s.Inference != nil {
		return false
	}
	writeJSON(w, http.StatusServiceUnavailable, simpleResponse{Error: "inference is not configured (AMH_CREDENTIAL_KEY unset) — /v1/inference routes are disabled"})
	return true
}

func toInferenceMessages(in []inferenceMessage) []inference.Message {
	out := make([]inference.Message, len(in))
	for i, m := range in {
		out[i] = inference.Message{Role: m.Role, Content: m.Content}
	}
	return out
}

func inferenceErrorStatus(err error) int {
	if errors.Is(err, inference.ErrProviderNotConfigured) {
		return http.StatusNotFound
	}
	return http.StatusBadGateway
}

func (s *Server) handleInferenceComplete(w http.ResponseWriter, r *http.Request) {
	if s.inferenceUnavailable(w) {
		return
	}
	var req inferenceCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, inferenceCompleteResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, inferenceCompleteResponse{Error: "model is required"})
		return
	}
	text, err := s.Inference.Complete(r.Context(), inference.Request{
		Provider:  req.Provider,
		Providers: req.Providers,
		Model:     req.Model,
		System:    req.System,
		Messages:  toInferenceMessages(req.Messages),
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		writeJSON(w, inferenceErrorStatus(err), inferenceCompleteResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, inferenceCompleteResponse{Text: text})
}

func (s *Server) handleInferenceCountTokens(w http.ResponseWriter, r *http.Request) {
	if s.inferenceUnavailable(w) {
		return
	}
	var req inferenceCompleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, inferenceCountTokensResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, inferenceCountTokensResponse{Error: "model is required"})
		return
	}
	n, err := s.Inference.CountTokens(r.Context(), inference.Request{
		Provider:  req.Provider,
		Providers: req.Providers,
		Model:     req.Model,
		System:    req.System,
		Messages:  toInferenceMessages(req.Messages),
	})
	if err != nil {
		writeJSON(w, inferenceErrorStatus(err), inferenceCountTokensResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, inferenceCountTokensResponse{InputTokens: n})
}

type inferenceEmbedRequest struct {
	Provider  string   `json:"provider,omitempty"`
	Providers []string `json:"providers,omitempty"`
	Model     string   `json:"model"`
	Input     []string `json:"input"`
}

type inferenceEmbedResponse struct {
	Embeddings [][]float32 `json:"embeddings,omitempty"`
	Dimension  int         `json:"dimension,omitempty"`
	Error      string      `json:"error,omitempty"`
}

func (s *Server) handleInferenceEmbed(w http.ResponseWriter, r *http.Request) {
	if s.inferenceUnavailable(w) {
		return
	}
	var req inferenceEmbedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, inferenceEmbedResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.Model == "" {
		writeJSON(w, http.StatusBadRequest, inferenceEmbedResponse{Error: "model is required"})
		return
	}
	if len(req.Input) == 0 {
		writeJSON(w, http.StatusBadRequest, inferenceEmbedResponse{Error: "input is required"})
		return
	}
	result, err := s.Inference.Embed(r.Context(), inference.EmbedRequest{
		Provider:  req.Provider,
		Providers: req.Providers,
		Model:     req.Model,
		Input:     req.Input,
	})
	if err != nil {
		status := inferenceErrorStatus(err)
		if errors.Is(err, inference.ErrEmbedNotSupported) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, inferenceEmbedResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, inferenceEmbedResponse{Embeddings: result.Embeddings, Dimension: result.Dimension})
}

// ── OpenAI-compatible facade ─────────────────────────────────────────────
//
// /v1/inference/complete and /v1/inference/embed above are AMH's own wire
// shape. This facade exists for external services this codebase does not
// control the internals of — Hindsight (agents/memory episodic/knowledge
// projection) is the first — that can only be pointed at a model provider
// via a standard OpenAI-compatible base_url + api_key, not by writing a
// custom client class against them the way memory/graph_llm.py does for
// Graphiti. It is real, general infrastructure: any OpenAI-SDK-based tool
// can be given AMH model custody this way, not just Hindsight.
//
// The caller's agent bearer token IS the "api_key" (sent as
// "Authorization: Bearer <token>" by every OpenAI-compatible client) —
// authenticated by the same RequireRole middleware as every other agent
// route; no separate credential is issued.
//
// Real OpenAI wire format has one "model" string, but selecting an AMH
// account needs both a provider (which registered credential) and a model
// name (what to ask that provider for) — the same two fields
// inferenceCompleteRequest keeps separate. This facade reuses the
// "<provider>/<model>" convention litellm and OpenRouter already use for
// exactly this: split model on the first "/"; no "/" means the whole
// string is the model and the provider defaults the same way
// inference.Request.Provider already does (see providerOrDefault).

func splitOpenAIModel(model string) (provider, actualModel string) {
	if i := strings.IndexByte(model, '/'); i >= 0 {
		return model[:i], model[i+1:]
	}
	return "", model
}

type openAIChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type openAIChatCompletionRequest struct {
	Model     string              `json:"model"`
	Messages  []openAIChatMessage `json:"messages"`
	MaxTokens int                 `json:"max_tokens,omitempty"`
}

type openAIChatChoice struct {
	Index        int               `json:"index"`
	Message      openAIChatMessage `json:"message"`
	FinishReason string            `json:"finish_reason"`
}

type openAIChatCompletionResponse struct {
	ID      string             `json:"id"`
	Object  string             `json:"object"`
	Created int64              `json:"created"`
	Model   string             `json:"model"`
	Choices []openAIChatChoice `json:"choices"`
}

type openAIErrorBody struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type openAIErrorResponse struct {
	Error openAIErrorBody `json:"error"`
}

func writeOpenAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, openAIErrorResponse{Error: openAIErrorBody{Message: message, Type: "invalid_request_error"}})
}

// splitOpenAIMessages separates a leading system message (OpenAI puts
// system in the messages array; inference.Request keeps it as a separate
// field) from the rest.
func splitOpenAIMessages(in []openAIChatMessage) (system string, rest []inference.Message) {
	start := 0
	if len(in) > 0 && in[0].Role == "system" {
		system = in[0].Content
		start = 1
	}
	rest = make([]inference.Message, 0, len(in)-start)
	for _, m := range in[start:] {
		rest = append(rest, inference.Message{Role: m.Role, Content: m.Content})
	}
	return system, rest
}

func (s *Server) handleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	if s.inferenceUnavailable(w) {
		return
	}
	var req openAIChatCompletionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required")
		return
	}
	provider, model := splitOpenAIModel(req.Model)
	system, messages := splitOpenAIMessages(req.Messages)

	text, err := s.Inference.Complete(r.Context(), inference.Request{
		Provider:  provider,
		Model:     model,
		System:    system,
		Messages:  messages,
		MaxTokens: req.MaxTokens,
	})
	if err != nil {
		writeOpenAIError(w, inferenceErrorStatus(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, openAIChatCompletionResponse{
		ID:      "chatcmpl-" + uuid.NewString(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   req.Model,
		Choices: []openAIChatChoice{{
			Index:        0,
			Message:      openAIChatMessage{Role: "assistant", Content: text},
			FinishReason: "stop",
		}},
	})
}

// openAIEmbedInput accepts either a single string or a list of strings —
// the real OpenAI embeddings API supports both.
type openAIEmbedInput []string

func (in *openAIEmbedInput) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*in = openAIEmbedInput{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*in = openAIEmbedInput(many)
	return nil
}

type openAIEmbeddingsRequest struct {
	Model string           `json:"model"`
	Input openAIEmbedInput `json:"input"`
}

type openAIEmbeddingDatum struct {
	Object    string    `json:"object"`
	Index     int       `json:"index"`
	Embedding []float32 `json:"embedding"`
}

type openAIEmbeddingsResponse struct {
	Object string                 `json:"object"`
	Model  string                 `json:"model"`
	Data   []openAIEmbeddingDatum `json:"data"`
}

func (s *Server) handleOpenAIEmbeddings(w http.ResponseWriter, r *http.Request) {
	if s.inferenceUnavailable(w) {
		return
	}
	var req openAIEmbeddingsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if req.Model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "model is required")
		return
	}
	if len(req.Input) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "input is required")
		return
	}
	provider, model := splitOpenAIModel(req.Model)

	result, err := s.Inference.Embed(r.Context(), inference.EmbedRequest{
		Provider: provider,
		Model:    model,
		Input:    []string(req.Input),
	})
	if err != nil {
		status := inferenceErrorStatus(err)
		if errors.Is(err, inference.ErrEmbedNotSupported) {
			status = http.StatusBadRequest
		}
		writeOpenAIError(w, status, err.Error())
		return
	}
	data := make([]openAIEmbeddingDatum, len(result.Embeddings))
	for i, e := range result.Embeddings {
		data[i] = openAIEmbeddingDatum{Object: "embedding", Index: i, Embedding: e}
	}
	writeJSON(w, http.StatusOK, openAIEmbeddingsResponse{Object: "list", Model: req.Model, Data: data})
}
