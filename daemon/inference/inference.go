// Package inference is the AMH core's model-provider seam
// (docs/AMH-SPECIFICATION.md §2.1: "model-provider and tool-provider
// seams" is a core responsibility). It exists so that an ephemeral agent
// computer (daemon/sandbox) never holds a model-provider credential
// itself: the credential is registered once, by an operator, as an
// account in daemon/credentials, and every agent process calls this
// package (via daemon/api's /v1/inference/* routes) using only the same
// agent bearer token it already holds for actuation, approval, and
// everything else. Model inference is exactly the same "agent proposes
// via a token it holds; the daemon commits via a secret it holds"
// pattern daemon/actuation already uses for physical device I/O — this
// package is that pattern applied to model calls instead of SSH.
//
// A model-provider account's credential is a JSON envelope (see
// credentialEnvelope) covering both static API keys and OAuth
// subscription tokens with automatic refresh — one shape for every
// provider this package adds, rather than per-vendor storage code.
//
// Verified provider wiring (checked against each vendor's actual wire
// protocol, not assumed):
//   - Anthropic direct: kind "anthropic", default base_url.
//   - Z.ai GLM Coding Plan: kind "anthropic" with base_url
//     "https://api.z.ai/api/anthropic" (Z.ai publishes a genuine
//     Anthropic-Messages-API-compatible endpoint — same request/response
//     shape, just a different host), OR kind "openai_compatible" with
//     base_url "https://api.z.ai/api/coding/paas/v4" (the coding-plan-
//     specific OpenAI-compatible path — NOT the general /api/paas/v4).
//   - xAI Grok, SuperGrok/X Premium+ subscription: kind
//     "openai_compatible", base_url "https://api.x.ai/v1", oauth token
//     obtained via a vendor device-code flow against auth.x.ai — the
//     inference call itself is standard OpenAI-shaped chat completions
//     with the access token as a bearer credential.
//   - Self-hosted vLLM / Ollama: kind "openai_compatible", base_url
//     "http://<host>:8000/v1" (vLLM) or "http://<host>:11434/v1"
//     (Ollama), api_key any non-empty placeholder (Ollama's own docs use
//     the literal string "ollama"; neither validates it for local use).
//
// Deliberately NOT implemented: OpenAI Codex under a ChatGPT subscription
// (`codex login --device-auth`) does not call api.openai.com at all —
// verified authenticated requests route to chatgpt.com/backend-api/codex/
// responses, an internal, undocumented, SSE-based Responses API distinct
// in shape from both providers above. That is not a published contract
// this package can build against with the same confidence as the others,
// and xAI has separately been observed gating its own equivalent surface
// (Grok Build) by subscription tier without documenting it. A real OpenAI
// API key against api.openai.com (kind "openai_compatible") is the
// supported path to Codex-quality models today; it bills per-token
// against the OpenAI platform account rather than the ChatGPT plan.
package inference

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
)

// Message mirrors a single chat turn — role is "user" or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Request is provider-neutral: Provider selects which registered account's
// credential to use, Model is passed through to that provider's API
// verbatim. Provider defaults to "anthropic" if empty, matching this
// package's original single-provider behavior when no fallback chain is
// given.
//
// Providers, if non-empty, is an ordered failover chain: the caller (or
// its operator-configured default) names more than one registered
// provider — e.g. {"anthropic-prod", "anthropic-eval"}, or
// {"grok", "anthropic"} — and Complete/CountTokens try each in order,
// returning the first success. daemon/credentials.GetActiveAccountByProvider
// already documents that redundancy is expressed as distinct provider
// strings, not multiple accounts sharing one; Providers is routing on top
// of that existing design, not a new credential-selection rule. Providers
// takes precedence over Provider when both are set.
type Request struct {
	Provider  string
	Providers []string
	Model     string
	System    string
	Messages  []Message
	MaxTokens int
}

var (
	ErrProviderNotConfigured = errors.New("inference: no active account is registered for this provider")
	ErrProviderCallFailed    = errors.New("inference: the model provider call failed")
	// ErrAllProvidersFailed wraps the joined per-provider errors when every
	// provider in a Request's failover chain failed — see providerChain.
	ErrAllProvidersFailed = errors.New("inference: every provider in the failover chain failed")
)

// providerChain returns req's ordered list of providers to try: Providers
// if set, otherwise the single Provider (defaulting to "anthropic").
func providerChain(req Request) []string {
	if len(req.Providers) > 0 {
		return req.Providers
	}
	return []string{providerOrDefault(req.Provider)}
}

// credentialEnvelope is the JSON shape stored (encrypted) as a model-
// provider account's credential — see this package's doc comment.
type credentialEnvelope struct {
	Kind    string      `json:"kind"` // "anthropic" | "openai_compatible"
	APIKey  string      `json:"api_key,omitempty"`
	BaseURL string      `json:"base_url,omitempty"`
	OAuth   *oauthToken `json:"oauth,omitempty"`
}

// oauthToken is a subscription-based provider's access/refresh pair.
// RefreshURL and ClientID are supplied by whoever registers the account
// (the operator, reading them off their own vendor login) — this package
// does not hardcode a vendor's token endpoint, since it cannot verify one
// without a live account of its own.
type oauthToken struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresAt    time.Time `json:"expires_at"`
	RefreshURL   string    `json:"refresh_url"`
	ClientID     string    `json:"client_id"`
}

func (t *oauthToken) expired() bool {
	return time.Now().After(t.ExpiresAt.Add(-30 * time.Second)) // refresh slightly early
}

// Router resolves a Request's Provider to a registered account's
// credential and dispatches to the matching HTTP call shape.
type Router struct {
	Credentials *credentials.Store
	HTTPClient  *http.Client
}

func New(creds *credentials.Store) *Router {
	return &Router{Credentials: creds, HTTPClient: &http.Client{Timeout: 120 * time.Second}}
}

// Complete returns the model's real text response. If req names more than
// one provider (Providers), each is tried in order and the first success
// wins — a failure on one provider (misconfigured, rate-limited, down)
// does not fail the request while another configured provider is still
// usable. Every provider's failure is preserved (errors.Join) in the
// returned error so a caller can see exactly what was tried, not just the
// last attempt.
func (r *Router) Complete(ctx context.Context, req Request) (string, error) {
	var errs []error
	for _, provider := range providerChain(req) {
		text, err := r.completeOne(ctx, provider, req)
		if err == nil {
			return text, nil
		}
		errs = append(errs, fmt.Errorf("provider %q: %w", provider, err))
	}
	return "", errors.Join(append([]error{ErrAllProvidersFailed}, errs...)...)
}

func (r *Router) completeOne(ctx context.Context, provider string, req Request) (string, error) {
	env, err := r.resolveCredential(ctx, provider)
	if err != nil {
		return "", err
	}
	switch env.Kind {
	case "anthropic":
		return r.anthropicComplete(ctx, env, req)
	case "openai_compatible":
		return r.openAICompatibleComplete(ctx, env, req)
	default:
		return "", fmt.Errorf("inference: account credential has unknown kind %q", env.Kind)
	}
}

// CountTokens returns the provider's real input token count, trying each
// provider in req's failover chain in order (see Complete). Only
// implemented for the anthropic provider — see context.llm's equivalent
// note on the Python side — so a chain containing only openai_compatible
// providers will exhaust the chain and return ErrAllProvidersFailed.
func (r *Router) CountTokens(ctx context.Context, req Request) (int, error) {
	var errs []error
	for _, provider := range providerChain(req) {
		n, err := r.countTokensOne(ctx, provider, req)
		if err == nil {
			return n, nil
		}
		errs = append(errs, fmt.Errorf("provider %q: %w", provider, err))
	}
	return 0, errors.Join(append([]error{ErrAllProvidersFailed}, errs...)...)
}

func (r *Router) countTokensOne(ctx context.Context, provider string, req Request) (int, error) {
	env, err := r.resolveCredential(ctx, provider)
	if err != nil {
		return 0, err
	}
	if env.Kind != "anthropic" {
		return 0, fmt.Errorf("inference: count_tokens is only implemented for the anthropic provider, got %q", env.Kind)
	}
	return r.anthropicCountTokens(ctx, env, req)
}

func providerOrDefault(p string) string {
	if p == "" {
		return "anthropic"
	}
	return p
}

// resolveCredential loads the active account for provider, decrypts its
// credential envelope, and refreshes an expired OAuth token in place
// before returning — callers never see a stale token.
func (r *Router) resolveCredential(ctx context.Context, provider string) (credentialEnvelope, error) {
	account, err := r.Credentials.GetActiveAccountByProvider(ctx, provider)
	if err != nil {
		return credentialEnvelope{}, fmt.Errorf("%w: %s", ErrProviderNotConfigured, provider)
	}
	raw, err := r.Credentials.Authenticate(ctx, credentials.SubjectAccount, account.ID)
	if err != nil {
		return credentialEnvelope{}, fmt.Errorf("%w: %s", ErrProviderNotConfigured, provider)
	}
	var env credentialEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return credentialEnvelope{}, fmt.Errorf("inference: stored credential for provider %q is not a valid envelope: %w", provider, err)
	}

	if env.OAuth != nil && env.OAuth.expired() {
		refreshed, err := r.refreshOAuth(ctx, *env.OAuth)
		if err != nil {
			return credentialEnvelope{}, fmt.Errorf("inference: refresh OAuth token for provider %q: %w", provider, err)
		}
		env.OAuth = &refreshed
		updated, err := json.Marshal(env)
		if err != nil {
			return credentialEnvelope{}, fmt.Errorf("inference: marshal refreshed credential: %w", err)
		}
		if _, err := r.Credentials.PutCredential(ctx, credentials.SubjectAccount, account.ID, updated); err != nil {
			return credentialEnvelope{}, fmt.Errorf("inference: store refreshed credential: %w", err)
		}
	}

	return env, nil
}

// refreshOAuth performs a standard OAuth2 refresh_token grant against the
// token's own RefreshURL/ClientID — generic by design, since this package
// does not hardcode any vendor's token endpoint (see oauthToken's doc
// comment).
func (r *Router) refreshOAuth(ctx context.Context, token oauthToken) (oauthToken, error) {
	if token.RefreshURL == "" || token.RefreshToken == "" {
		return oauthToken{}, fmt.Errorf("inference: token has no refresh_url/refresh_token to refresh with")
	}
	form := fmt.Sprintf("grant_type=refresh_token&refresh_token=%s&client_id=%s", token.RefreshToken, token.ClientID)
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, token.RefreshURL, strings.NewReader(form))
	if err != nil {
		return oauthToken{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := r.HTTPClient.Do(request)
	if err != nil {
		return oauthToken{}, fmt.Errorf("%w: %v", ErrProviderCallFailed, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return oauthToken{}, fmt.Errorf("%w: refresh returned HTTP %d: %s", ErrProviderCallFailed, resp.StatusCode, string(body))
	}

	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int    `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return oauthToken{}, fmt.Errorf("inference: parse refresh response: %w", err)
	}
	newRefresh := payload.RefreshToken
	if newRefresh == "" {
		newRefresh = token.RefreshToken // some providers don't rotate the refresh token itself
	}
	return oauthToken{
		AccessToken:  payload.AccessToken,
		RefreshToken: newRefresh,
		ExpiresAt:    time.Now().Add(time.Duration(payload.ExpiresIn) * time.Second),
		RefreshURL:   token.RefreshURL,
		ClientID:     token.ClientID,
	}, nil
}

func bearerFor(env credentialEnvelope) (string, error) {
	if env.OAuth != nil {
		return env.OAuth.AccessToken, nil
	}
	if env.APIKey != "" {
		return env.APIKey, nil
	}
	return "", fmt.Errorf("inference: credential envelope has neither api_key nor oauth")
}

func (r *Router) anthropicComplete(ctx context.Context, env credentialEnvelope, req Request) (string, error) {
	baseURL := env.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	key, err := bearerFor(env)
	if err != nil {
		return "", err
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body, _ := json.Marshal(map[string]any{
		"model":      req.Model,
		"max_tokens": maxTokens,
		"system":     req.System,
		"messages":   req.Messages,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	setAnthropicHeaders(httpReq, env, key)

	resp, respBody, err := r.doJSON(httpReq)
	if err != nil {
		return "", err
	}
	var payload struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("inference: parse anthropic response (status %d): %w", resp.StatusCode, err)
	}
	var text strings.Builder
	for _, block := range payload.Content {
		if block.Type == "text" {
			text.WriteString(block.Text)
		}
	}
	return text.String(), nil
}

func (r *Router) anthropicCountTokens(ctx context.Context, env credentialEnvelope, req Request) (int, error) {
	baseURL := env.BaseURL
	if baseURL == "" {
		baseURL = "https://api.anthropic.com"
	}
	key, err := bearerFor(env)
	if err != nil {
		return 0, err
	}
	body, _ := json.Marshal(map[string]any{
		"model":    req.Model,
		"system":   req.System,
		"messages": req.Messages,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/v1/messages/count_tokens", bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	setAnthropicHeaders(httpReq, env, key)

	_, respBody, err := r.doJSON(httpReq)
	if err != nil {
		return 0, err
	}
	var payload struct {
		InputTokens int `json:"input_tokens"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return 0, fmt.Errorf("inference: parse anthropic count_tokens response: %w", err)
	}
	return payload.InputTokens, nil
}

func setAnthropicHeaders(httpReq *http.Request, env credentialEnvelope, key string) {
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	if env.OAuth != nil {
		// A subscription (OAuth) token authenticates as a bearer credential,
		// distinct from a direct API key — matches how Anthropic's own
		// first-party CLI distinguishes an OAuth session from an API key.
		httpReq.Header.Set("Authorization", "Bearer "+key)
	} else {
		httpReq.Header.Set("x-api-key", key)
	}
}

func (r *Router) openAICompatibleComplete(ctx context.Context, env credentialEnvelope, req Request) (string, error) {
	if env.BaseURL == "" {
		return "", fmt.Errorf("inference: openai_compatible credential is missing base_url")
	}
	key, err := bearerFor(env)
	if err != nil {
		return "", err
	}
	messages := make([]map[string]string, 0, len(req.Messages)+1)
	if req.System != "" {
		messages = append(messages, map[string]string{"role": "system", "content": req.System})
	}
	for _, m := range req.Messages {
		messages = append(messages, map[string]string{"role": m.Role, "content": m.Content})
	}
	maxTokens := req.MaxTokens
	if maxTokens == 0 {
		maxTokens = 4096
	}
	body, _ := json.Marshal(map[string]any{
		"model":      req.Model,
		"messages":   messages,
		"max_tokens": maxTokens,
	})

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(env.BaseURL, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+key)

	_, respBody, err := r.doJSON(httpReq)
	if err != nil {
		return "", err
	}
	var payload struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(respBody, &payload); err != nil {
		return "", fmt.Errorf("inference: parse openai-compatible response: %w", err)
	}
	if len(payload.Choices) == 0 {
		return "", fmt.Errorf("inference: openai-compatible response had no choices: %s", string(respBody))
	}
	return payload.Choices[0].Message.Content, nil
}

func (r *Router) doJSON(httpReq *http.Request) (*http.Response, []byte, error) {
	resp, err := r.HTTPClient.Do(httpReq)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrProviderCallFailed, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return resp, nil, fmt.Errorf("%w: HTTP %d: %s", ErrProviderCallFailed, resp.StatusCode, string(body))
	}
	return resp, body, nil
}
