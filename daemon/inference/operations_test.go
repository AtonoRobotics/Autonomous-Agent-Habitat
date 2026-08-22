package inference

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
)

func TestComplete_Success_CreatesConfirmedOperation(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"tracked answer"}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})

	ops := operations.New(db, policy.New(db))
	router := New(creds)
	router.Operations = ops

	result, err := router.Complete(context.Background(), Request{
		Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "tracked answer" {
		t.Fatalf("expected the real answer, got %q", result)
	}

	effects := listEffectsByType(t, ops, "amh.core/inference.complete")
	if len(effects) != 1 {
		t.Fatalf("expected exactly one tracked effect, got %d", len(effects))
	}
	if effects[0].State != operations.StateConfirmed {
		t.Fatalf("expected confirmed, got %s", effects[0].State)
	}
	if effects[0].OwnerExtensionID != "amh.core/inference" {
		t.Fatalf("expected amh.core/inference as owner, got %q", effects[0].OwnerExtensionID)
	}
}

func TestComplete_ProviderFailure_CreatesFailedOperation(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"boom"}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})

	ops := operations.New(db, policy.New(db))
	router := New(creds)
	router.Operations = ops

	_, err := router.Complete(context.Background(), Request{
		Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatalf("expected an error from the failing fake provider")
	}

	effects := listEffectsByType(t, ops, "amh.core/inference.complete")
	if len(effects) != 1 {
		t.Fatalf("expected exactly one tracked effect, got %d", len(effects))
	}
	if effects[0].State != operations.StateFailed {
		t.Fatalf("expected failed, got %s", effects[0].State)
	}
	if effects[0].ErrorCode != "PROVIDER_CALL_FAILED" || effects[0].ErrorMessage == "" {
		t.Fatalf("expected error detail recorded, got %+v", effects[0])
	}
}

func TestComplete_NoOperationsConfigured_StillWorksUntracked(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"content":[{"type":"text","text":"untracked answer"}]}`))
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "anthropic", map[string]string{"kind": "anthropic", "api_key": "sk-ant-test", "base_url": fake.URL})

	router := New(creds) // router.Operations left nil
	result, err := router.Complete(context.Background(), Request{
		Provider: "anthropic", Model: "claude-sonnet-5", Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if result != "untracked answer" {
		t.Fatalf("expected the real answer even without tracking, got %q", result)
	}
}

func TestEmbed_Success_CreatesConfirmedOperation(t *testing.T) {
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"embedding": []float32{0.1, 0.2, 0.3}, "index": 0}},
		})
	}))
	defer fake.Close()

	db := testDB(t)
	creds := testCredentials(t, db)
	registerProviderAccount(t, creds, "openai_compatible", map[string]string{"kind": "openai_compatible", "api_key": "key", "base_url": fake.URL})

	ops := operations.New(db, policy.New(db))
	router := New(creds)
	router.Operations = ops

	if _, err := router.Embed(context.Background(), EmbedRequest{Provider: "openai_compatible", Model: "text-embedding-3", Input: []string{"hi"}}); err != nil {
		t.Fatalf("Embed: %v", err)
	}

	effects := listEffectsByType(t, ops, "amh.core/inference.embed")
	if len(effects) != 1 || effects[0].State != operations.StateConfirmed {
		t.Fatalf("expected exactly one confirmed effect, got %+v", effects)
	}
}

// listEffectsByType scans every distinct operation_id this test package's
// tests could plausibly have created and returns the effects matching
// effectType — operations.Engine has no "list all" method (by design,
// effects are looked up by operation_id, not enumerated), so tests find
// theirs by querying the one DB directly.
func listEffectsByType(t *testing.T, ops *operations.Engine, effectType string) []*operations.Effect {
	t.Helper()
	rows, err := ops.DB.Query(`SELECT effect_id FROM effect_record WHERE effect_type = $1`, effectType)
	if err != nil {
		t.Fatalf("query effect_record: %v", err)
	}
	defer rows.Close()
	var out []*operations.Effect
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan effect_id: %v", err)
		}
		eff, err := ops.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("Get %s: %v", id, err)
		}
		out = append(out, eff)
	}
	return out
}
