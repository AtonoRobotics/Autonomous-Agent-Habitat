package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNew_RejectsEmptyOrIdenticalTokens(t *testing.T) {
	if _, err := New("", "operator-token"); err != ErrTokenRequired {
		t.Fatalf("expected ErrTokenRequired for empty agent token, got %v", err)
	}
	if _, err := New("agent-token", ""); err != ErrTokenRequired {
		t.Fatalf("expected ErrTokenRequired for empty operator token, got %v", err)
	}
	if _, err := New("same-token", "same-token"); err != ErrTokensMustDiffer {
		t.Fatalf("expected ErrTokensMustDiffer, got %v", err)
	}
	if _, err := New("agent-token", "operator-token"); err != nil {
		t.Fatalf("expected valid construction to succeed, got %v", err)
	}
}

func newTestAuth(t *testing.T) *Authenticator {
	t.Helper()
	auth, err := New("agent-secret", "operator-secret")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return auth
}

func handlerOK(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func doRequest(t *testing.T, handler http.HandlerFunc, bearerToken string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/whatever", nil)
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	rec := httptest.NewRecorder()
	handler(rec, req)
	return rec
}

func TestRequireRole_MissingHeaderIs401(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleAgent, RoleOperator)
	rec := doRequest(t, handler, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestRequireRole_UnknownTokenIs401(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleAgent, RoleOperator)
	rec := doRequest(t, handler, "not-a-real-token")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestRequireRole_AgentAllowedOnAgentRoute(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleAgent, RoleOperator)
	rec := doRequest(t, handler, "agent-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

func TestRequireRole_OperatorAllowedOnAgentRoute(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleAgent, RoleOperator)
	rec := doRequest(t, handler, "operator-secret")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
}

// This is the actual security property the package exists for: an agent
// token must be mechanically refused on an operator-only route (the
// approve endpoint), not merely discouraged by convention.
func TestRequireRole_AgentTokenRejectedOnOperatorOnlyRoute(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleOperator)

	agentRec := doRequest(t, handler, "agent-secret")
	if agentRec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for agent token on operator-only route, got %d", agentRec.Code)
	}

	operatorRec := doRequest(t, handler, "operator-secret")
	if operatorRec.Code != http.StatusOK {
		t.Fatalf("expected 200 for operator token on operator-only route, got %d", operatorRec.Code)
	}
}

func TestRequireRole_MalformedHeaderIs401(t *testing.T) {
	auth := newTestAuth(t)
	handler := auth.RequireRole(handlerOK, RoleAgent, RoleOperator)

	req := httptest.NewRequest(http.MethodPost, "/whatever", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // wrong scheme
	rec := httptest.NewRecorder()
	handler(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for non-Bearer scheme, got %d", rec.Code)
	}
}
