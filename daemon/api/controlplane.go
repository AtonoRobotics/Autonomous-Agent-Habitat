// Control-plane routes: install harnesses, provision computers, configure
// connectors, and authenticate accounts and modules — the admin surface
// the control-plane UI extension (extensions/control-plane-ui) is built
// against, and the seam any other operator tool could use instead.
//
// Extension ids are namespaced ("amh.control-plane/ui") and contain a
// literal "/", which Go's net/http ServeMux cannot represent inside a
// single {wildcard} path segment (and a trailing {id...} wildcard can't be
// followed by a version segment and an action). Rather than URL-encoding
// around that, extension id/version travel in the JSON body for mutations
// and as query parameters for the two GET routes — computers, connectors,
// and accounts use plain UUIDs and keep the path-segment style the rest
// of this package already uses.
package api

import (
	"encoding/json"
	"errors"
	"net/http"

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

// ── Connectors ───────────────────────────────────────────────────────────

type createConnectorRequest struct {
	ID               string          `json:"id"`
	Type             string          `json:"type"`
	Auth             string          `json:"auth"`
	Config           json.RawMessage `json:"config,omitempty"`
	ExtensionID      string          `json:"extension_id,omitempty"`
	ExtensionVersion string          `json:"extension_version,omitempty"`
	AccountID        string          `json:"account_id,omitempty"`
}

type connectorResponse struct {
	ID     string `json:"id,omitempty"`
	Type   string `json:"type,omitempty"`
	Auth   string `json:"auth,omitempty"`
	Status string `json:"status,omitempty"`
	Error  string `json:"error,omitempty"`
}

func (s *Server) handleCreateConnector(w http.ResponseWriter, r *http.Request) {
	var req createConnectorRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, connectorResponse{Error: "invalid request body: " + err.Error()})
		return
	}
	if req.ID == "" || req.Type == "" {
		writeJSON(w, http.StatusBadRequest, connectorResponse{Error: "id and type are required"})
		return
	}
	auth := req.Auth
	if auth == "" {
		auth = "none"
	}
	var configVal any
	if len(req.Config) > 0 {
		configVal = string(req.Config)
	}
	var extIDVal, extVerVal, acctVal any
	if req.ExtensionID != "" {
		extIDVal, extVerVal = req.ExtensionID, req.ExtensionVersion
	}
	if req.AccountID != "" {
		acctVal = req.AccountID
	}
	_, err := s.DB.ExecContext(r.Context(), `
		INSERT INTO connector (id, type, auth, config, extension_id, extension_version, account_id, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'active')`,
		req.ID, req.Type, auth, configVal, extIDVal, extVerVal, acctVal,
	)
	if err != nil {
		writeJSON(w, http.StatusConflict, connectorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, connectorResponse{ID: req.ID, Type: req.Type, Auth: auth, Status: "active"})
}

func (s *Server) handleDisableConnector(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PathValue("connectorID")
	res, err := s.DB.ExecContext(r.Context(), `UPDATE connector SET status = 'disabled' WHERE id = ?`, connectorID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeJSON(w, http.StatusNotFound, simpleResponse{Error: "connector not found: " + connectorID})
		return
	}
	writeJSON(w, http.StatusOK, simpleResponse{})
}

func (s *Server) handleGetConnector(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PathValue("connectorID")
	var resp connectorResponse
	err := s.DB.QueryRowContext(r.Context(), `SELECT id, type, auth, status FROM connector WHERE id = ?`, connectorID).
		Scan(&resp.ID, &resp.Type, &resp.Auth, &resp.Status)
	if err != nil {
		writeJSON(w, http.StatusNotFound, connectorResponse{Error: "connector not found: " + connectorID})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListConnectors(w http.ResponseWriter, r *http.Request) {
	rows, err := s.DB.QueryContext(r.Context(), `SELECT id, type, auth, status FROM connector ORDER BY created_at DESC`)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
		return
	}
	defer rows.Close()
	out := []connectorResponse{}
	for rows.Next() {
		var c connectorResponse
		if err := rows.Scan(&c.ID, &c.Type, &c.Auth, &c.Status); err != nil {
			writeJSON(w, http.StatusInternalServerError, simpleResponse{Error: err.Error()})
			return
		}
		out = append(out, c)
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
