// Package authn is the daemon API's authentication/authorization layer:
// static bearer tokens mapped to one of two roles. It exists to make one
// specific security property mechanical rather than a convention: an
// agent must never be able to grant itself an operator-only action —
// installing an extension, writing a credential — the same anti-reward-
// hacking discipline (§14.7) applied to identity instead of eval logging.
//
// Two static, long-lived tokens (one per role), configured via
// environment variables, checked with constant-time comparison. This is
// deliberately not a full identity system — no per-user tokens, no
// expiry, no revocation list — because those aren't what the security
// property here needs: "the requesting agent literally does not hold the
// credential needed to perform the operator-only action" is a fact
// enforced by the server, not a hopeful comment. A multi-operator
// deployment needing per-operator accountability puts a proper identity
// provider in front of this.
package authn

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
)

// Role is who a request is authenticated as. There is no implicit
// hierarchy between roles — see allows() below.
type Role string

const (
	RoleAgent    Role = "agent"
	RoleOperator Role = "operator"
)

// Authenticator holds the two configured tokens. Both must be non-empty
// and distinct — see New.
type Authenticator struct {
	agentToken    string
	operatorToken string
}

var (
	ErrTokenRequired     = errors.New("authn: both agent and operator tokens are required")
	ErrTokensMustDiffer  = errors.New("authn: agent and operator tokens must be different")
	ErrMissingAuthHeader = errors.New("authn: missing or malformed Authorization header")
	ErrInvalidToken      = errors.New("authn: invalid token")
	ErrRoleNotAllowed    = errors.New("authn: this role is not permitted for this endpoint")
)

// New validates and wraps the two role tokens. Fails closed: an empty or
// identical pair of tokens is a configuration error, not something this
// package silently tolerates — see amh-daemon's main.go, which refuses to
// start the API server rather than call New with weak input.
func New(agentToken, operatorToken string) (*Authenticator, error) {
	if agentToken == "" || operatorToken == "" {
		return nil, ErrTokenRequired
	}
	if agentToken == operatorToken {
		return nil, ErrTokensMustDiffer
	}
	return &Authenticator{agentToken: agentToken, operatorToken: operatorToken}, nil
}

// roleFor identifies which role, if any, a bearer token belongs to.
// Constant-time comparison so token-guessing can't be sped up by timing
// a byte-by-byte mismatch.
func (a *Authenticator) roleFor(token string) (Role, bool) {
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.operatorToken)) == 1 {
		return RoleOperator, true
	}
	if token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(a.agentToken)) == 1 {
		return RoleAgent, true
	}
	return "", false
}

func bearerToken(r *http.Request) (string, error) {
	header := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", ErrMissingAuthHeader
	}
	token := strings.TrimPrefix(header, prefix)
	if token == "" {
		return "", ErrMissingAuthHeader
	}
	return token, nil
}

// allows reports whether role is one of the explicitly allowed roles.
// Deliberately no implicit hierarchy (e.g. "operator can do anything an
// agent can") — every route lists every role it accepts, so reading a
// route's RequireRole call is the whole answer to "who can call this,"
// with no separate rule to remember. Routes both roles should reach
// (actuate, create-ticket, status) list RoleAgent, RoleOperator
// explicitly; the approve route lists RoleOperator alone, which is the
// entire security property this package exists for.
func allows(role Role, allowed []Role) bool {
	for _, r := range allowed {
		if role == r {
			return true
		}
	}
	return false
}

// RequireRole wraps a handler so it only runs for requests bearing a
// token whose role is in allowed. Returns 401 for a missing/malformed
// header or an unrecognized token, 403 for a recognized token whose role
// isn't permitted here (e.g. an agent token hitting an operator-only
// route) — distinguishing "who are you" from "you can't do that" per
// normal HTTP semantics.
func (a *Authenticator) RequireRole(next http.HandlerFunc, allowed ...Role) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := bearerToken(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)
			return
		}
		role, ok := a.roleFor(token)
		if !ok {
			http.Error(w, ErrInvalidToken.Error(), http.StatusUnauthorized)
			return
		}
		if !allows(role, allowed) {
			http.Error(w, ErrRoleNotAllowed.Error(), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	}
}
