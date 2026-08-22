// Package api is the daemon-resident HTTP surface the Python agent layer
// calls for durable workflow control-plane operations: extension
// lifecycle, per-agent compute sandboxes, account/credential management,
// the model-provider inference seam, the generic policy/approval seam,
// and the self-improvement candidate lifecycle.
//
// Every route requires a bearer token (daemon/authn) — there is no
// unauthenticated mode. Which role a route accepts is the whole
// authorization policy for this package; see Handler's doc comment.
package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/backup"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/extensions"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/inference"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/sandbox"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/selfimprove"
)

type simpleResponse struct {
	Error string `json:"error,omitempty"`
}

type Server struct {
	Addr        string
	DB          *sql.DB
	Extensions  *extensions.Registry
	Sandbox     *sandbox.Provisioner
	Credentials *credentials.Store
	Inference   *inference.Router
	Policy      *policy.Engine
	SelfImprove *selfimprove.Engine
	Backup      *backup.Backer
	Operations  *operations.Engine
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
// a soft-disable of just that surface, not a refusal to start, since the
// extension/sandbox surfaces don't depend on it. requireSignatures sets
// extensions.Registry.RequireSignatures — see that field's doc comment and
// main.go's AMH_EXTENSIONS_REQUIRE_SIGNATURES for why this defaults to
// false rather than being mandatory from day one. dbURL is the same
// connection string db was opened from — daemon/backup shells out to
// pg_dump/pg_restore, which need their own connection string rather than
// database/sql's *sql.DB handle.
func New(addr string, db *sql.DB, dbURL string, tp trace.TracerProvider, auth *authn.Authenticator, log *slog.Logger, sandboxBaseDir string, creds *credentials.Store, requireSignatures bool) *Server {
	if log == nil {
		log = slog.Default()
	}
	pol := policy.New(db)
	ops := operations.New(db, pol)
	var inferenceRouter *inference.Router
	if creds != nil {
		inferenceRouter = inference.New(creds)
		inferenceRouter.Operations = ops
	}
	ext := extensions.New(db)
	ext.RequireSignatures = requireSignatures
	return &Server{
		Addr:        addr,
		DB:          db,
		Extensions:  ext,
		Sandbox:     sandbox.New(db, sandboxBaseDir),
		Credentials: creds,
		Inference:   inferenceRouter,
		Policy:      pol,
		SelfImprove: selfimprove.New(db),
		Backup:      backup.New(dbURL),
		Operations:  ops,
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
// read them here, not scattered across handler bodies. See controlplane.go
// for handler bodies.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Control plane: extensions, computers, accounts. See controlplane.go
	// for handler bodies and the doc comment there for why extension
	// id/version travel in the request body or query string rather than
	// the URL path (namespaced extension ids contain "/", which Go's
	// ServeMux path segments cannot represent).
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

	// Trusted signing keys (§14: "signed extension packs and compatibility
	// qualification") — the operator-managed set of Ed25519 keys
	// daemon/extensions.Discover verifies a manifest's spec.signature
	// against. Register/revoke are operator-only, same rationale as
	// extension mutations above; a public key is not a secret, so reads
	// are agent-or-operator. See trust.go for handler bodies.
	mux.HandleFunc("POST /v1/extensions/trusted-keys",
		s.Auth.RequireRole(s.handleRegisterTrustedKey, authn.RoleOperator))
	mux.HandleFunc("GET /v1/extensions/trusted-keys",
		s.Auth.RequireRole(s.handleListTrustedKeys, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/extensions/trusted-keys/{keyID}",
		s.Auth.RequireRole(s.handleGetTrustedKey, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/extensions/trusted-keys/{keyID}/revoke",
		s.Auth.RequireRole(s.handleRevokeTrustedKey, authn.RoleOperator))

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

	// Inference: agent OR operator — the model-provider seam
	// (docs/AMH-SPECIFICATION.md §2.1) an ephemeral agent computer calls
	// into instead of holding a model credential itself. See
	// daemon/inference and controlplane.go's handlers.
	mux.HandleFunc("POST /v1/inference/complete",
		s.Auth.RequireRole(s.handleInferenceComplete, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/inference/count-tokens",
		s.Auth.RequireRole(s.handleInferenceCountTokens, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/inference/embed",
		s.Auth.RequireRole(s.handleInferenceEmbed, authn.RoleAgent, authn.RoleOperator))

	// OpenAI-compatible facade over the same inference seam, for external
	// services that speak that wire format directly (e.g. Hindsight's
	// HINDSIGHT_API_LLM_BASE_URL/HINDSIGHT_API_EMBEDDINGS_OPENAI_BASE_URL)
	// rather than a custom AMH client class. Same auth tier as
	// /v1/inference/* — the caller's agent token IS the "api_key".
	mux.HandleFunc("POST /v1/openai/chat/completions",
		s.Auth.RequireRole(s.handleOpenAIChatCompletions, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/openai/embeddings",
		s.Auth.RequireRole(s.handleOpenAIEmbeddings, authn.RoleAgent, authn.RoleOperator))

	// Policy and approval (§6): agent-or-operator for decide/consume — an
	// agent proposing an action and, once admitted, dispatching it is the
	// core "agents propose" half of decision 9. Approve/deny are
	// operator-only — the "deterministic services commit" half, and the
	// same anti-self-approval property every other operator-only route
	// enforces (see daemon/authn's doc comment). See daemon/policy and
	// policy.go for handler bodies.
	mux.HandleFunc("POST /v1/policy/decide",
		s.Auth.RequireRole(s.handleDecide, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/policy/decisions/{decisionID}",
		s.Auth.RequireRole(s.handleGetDecision, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/policy/decisions/{decisionID}/consume",
		s.Auth.RequireRole(s.handleConsumeDecision, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/policy/approvals",
		s.Auth.RequireRole(s.handleListPendingApprovals, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/policy/approvals/{approvalID}",
		s.Auth.RequireRole(s.handleGetApprovalRequest, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/policy/approvals/{approvalID}/approve",
		s.Auth.RequireRole(s.handleApproveApprovalRequest, authn.RoleOperator))
	mux.HandleFunc("POST /v1/policy/approvals/{approvalID}/deny",
		s.Auth.RequireRole(s.handleDenyApprovalRequest, authn.RoleOperator))

	// Self-improvement candidate lifecycle (§10): Generate is agent-or-
	// operator — an agent (or future optimizer module) proposes a
	// candidate. RecordEval, unlike Generate, is operator-only: the
	// daemon always computes the pass/fail verdict itself from raw
	// case results (see daemon/selfimprove's doc comment), but computing
	// the verdict server-side does not by itself make the EVIDENCE
	// independent — an agent holding only its own token could otherwise
	// Generate a candidate and immediately self-report fabricated
	// all-passing case_results under any evaluator_id it likes. Requiring
	// the operator token to record evidence is the one producer/evaluator
	// separation this daemon's two-role RBAC can actually express: the
	// process submitting eval results must be trusted infrastructure
	// distinct from whatever proposed the candidate, not the same agent
	// credential. Canary/promote/demote/rollback/reject are likewise
	// operator-only — switching what's live, the same "deterministic
	// services commit" tier as policy's approve/deny. See
	// daemon/selfimprove and selfimprove.go for handler bodies.
	mux.HandleFunc("POST /v1/selfimprove/candidates",
		s.Auth.RequireRole(s.handleGenerateCandidate, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/selfimprove/candidates",
		s.Auth.RequireRole(s.handleListCandidates, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/selfimprove/candidates/{candidateID}",
		s.Auth.RequireRole(s.handleGetCandidate, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/eval",
		s.Auth.RequireRole(s.handleRecordEval, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/canary",
		s.Auth.RequireRole(s.handleCanaryCandidate, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/promote",
		s.Auth.RequireRole(s.handlePromoteCandidate, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/demote",
		s.Auth.RequireRole(s.handleDemoteCandidate, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/rollback",
		s.Auth.RequireRole(s.handleRollbackCandidate, authn.RoleOperator))
	mux.HandleFunc("POST /v1/selfimprove/candidates/{candidateID}/reject",
		s.Auth.RequireRole(s.handleRejectCandidate, authn.RoleOperator))

	// Backup/restore (§14): operator-only, same rationale as extension
	// mutations and account/credential writes above — running pg_dump/
	// pg_restore against the daemon's own store is a "deterministic
	// services commit" action, not something an autonomous agent token
	// triggers. See daemon/backup and backup.go for the implementation.
	mux.HandleFunc("POST /v1/backup",
		s.Auth.RequireRole(s.handleBackup, authn.RoleOperator))
	mux.HandleFunc("POST /v1/restore",
		s.Auth.RequireRole(s.handleRestore, authn.RoleOperator))

	// External-effect lifecycle (§4, §11, acceptance invariant #2):
	// agent-or-operator throughout — Propose composes with daemon/policy
	// for admission (the actual gated moment), and every other route here
	// is the caller mechanically reporting real dispatch progress on an
	// operation it's already authorized to run, not a new authority
	// grant. See daemon/operations and operations.go for why Resolve
	// trusts the caller's terminal-outcome argument rather than computing
	// it server-side.
	mux.HandleFunc("POST /v1/operations",
		s.Auth.RequireRole(s.handlePropose, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/operations",
		s.Auth.RequireRole(s.handleListEffects, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("GET /v1/operations/{effectID}",
		s.Auth.RequireRole(s.handleGetEffect, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/operations/{effectID}/dispatch-pending",
		s.Auth.RequireRole(s.handleMarkDispatchPending, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/operations/{effectID}/dispatched",
		s.Auth.RequireRole(s.handleMarkDispatched, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/operations/{effectID}/observed",
		s.Auth.RequireRole(s.handleMarkObserved, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/operations/{effectID}/outcome-unknown",
		s.Auth.RequireRole(s.handleMarkOutcomeUnknown, authn.RoleAgent, authn.RoleOperator))
	mux.HandleFunc("POST /v1/operations/{effectID}/resolve",
		s.Auth.RequireRole(s.handleResolve, authn.RoleAgent, authn.RoleOperator))

	return mux
}

// Run blocks, serving the control-plane API, until ctx is cancelled.
// Matches the supervisor.Child.Run signature — this is one more
// supervised child of amh-daemon, alongside scheduler and health.
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

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}
