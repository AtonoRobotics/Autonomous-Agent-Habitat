package extensions

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func baseManifest(id, version string) Manifest {
	return Manifest{
		APIVersion: "amh/v1",
		Kind:       "Extension",
		Metadata: Metadata{
			ID:        id,
			Name:      "Test Extension",
			Version:   version,
			Publisher: "amh-tests",
		},
		Spec: Spec{
			Entrypoint: "true",
			Isolation:  IsolationInProcess,
			Provides:   []CapabilityRef{},
			Requires:   []Requirement{},
			Compatibility: Compatibility{
				AMHCore: ">=0.1.0",
			},
		},
	}
}

func TestDiscover_ValidatesAndPersists(t *testing.T) {
	db := testDB(t)
	reg := New(db)

	m := baseManifest("amh.test/widget", "1.0.0")
	ext, err := reg.Discover(context.Background(), m)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if ext.Status != StatusDiscovered {
		t.Fatalf("expected status discovered, got %s", ext.Status)
	}

	// Re-discovering the identical manifest is idempotent.
	again, err := reg.Discover(context.Background(), m)
	if err != nil {
		t.Fatalf("re-Discover of identical manifest should be idempotent: %v", err)
	}
	if again.ManifestDigest != ext.ManifestDigest {
		t.Fatalf("expected same digest on idempotent re-discover")
	}
}

func TestDiscover_RejectsInvalidManifest(t *testing.T) {
	db := testDB(t)
	reg := New(db)

	bad := baseManifest("amh.test/widget", "1.0.0")
	bad.APIVersion = "amh/v2"
	if _, err := reg.Discover(context.Background(), bad); err == nil {
		t.Fatalf("expected an error for a bad apiVersion")
	}

	bad2 := baseManifest("Not A Valid Id", "1.0.0")
	if _, err := reg.Discover(context.Background(), bad2); err == nil {
		t.Fatalf("expected an error for an invalid namespaced id")
	}
}

func TestDiscover_RefusesChangedManifestAtSameVersion(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	m2 := m
	m2.Metadata.Description = "a different manifest body"
	if _, err := reg.Discover(ctx, m2); err == nil {
		t.Fatalf("expected re-discovering a changed manifest at the same id/version to be refused")
	}
}

func TestActivateThenDispose_InProcess_RoundTrips(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	active, err := reg.Activate(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if active.Status != StatusActive {
		t.Fatalf("expected active, got %s", active.Status)
	}
	if active.RuntimeHandle == "" {
		t.Fatalf("expected a recorded runtime handle")
	}

	quiescing, err := reg.Quiesce(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if quiescing.Status != StatusQuiescing {
		t.Fatalf("expected quiescing, got %s", quiescing.Status)
	}

	disposed, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	if disposed.Status != StatusDisposed {
		t.Fatalf("expected disposed, got %s", disposed.Status)
	}

	// A disposed extension can be reactivated — reversibility runs both ways.
	reactivated, err := reg.Activate(ctx, "amh.test/widget", "1.0.0")
	if err != nil {
		t.Fatalf("expected reactivation of a disposed extension to succeed: %v", err)
	}
	if reactivated.Status != StatusActive {
		t.Fatalf("expected active after reactivation, got %s", reactivated.Status)
	}
}

func TestActivateThenDispose_WithOperationsWired_RecordsConfirmedEffects(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	pol := policy.New(db)
	ops := operations.New(db, pol)
	reg.Operations = ops
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if _, err := reg.Activate(ctx, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	activateEffects, err := effectsByType(ctx, ops, db, "extension_activate")
	if err != nil {
		t.Fatalf("query activate effects: %v", err)
	}
	if len(activateEffects) != 1 {
		t.Fatalf("expected exactly one extension_activate effect, got %d", len(activateEffects))
	}
	if activateEffects[0].OwnerExtensionID != "amh.core/extensions" {
		t.Fatalf("expected owner_extension_id amh.core/extensions, got %s", activateEffects[0].OwnerExtensionID)
	}
	if activateEffects[0].State != operations.StateConfirmed {
		t.Fatalf("expected activate effect state confirmed, got %s", activateEffects[0].State)
	}

	if _, err := reg.Quiesce(ctx, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("Quiesce: %v", err)
	}
	if _, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0"); err != nil {
		t.Fatalf("Dispose: %v", err)
	}
	disposeEffects, err := effectsByType(ctx, ops, db, "extension_dispose")
	if err != nil {
		t.Fatalf("query dispose effects: %v", err)
	}
	if len(disposeEffects) != 1 {
		t.Fatalf("expected exactly one extension_dispose effect, got %d", len(disposeEffects))
	}
	if disposeEffects[0].State != operations.StateConfirmed {
		t.Fatalf("expected dispose effect state confirmed, got %s", disposeEffects[0].State)
	}
}

func TestActivate_WithOperationsWired_FailedLaunchRecordsFailedEffect(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ops := operations.New(db, policy.New(db))
	reg.Operations = ops
	ctx := context.Background()

	m := baseManifest("amh.test/broken", "1.0.0")
	m.Spec.Isolation = IsolationProcess
	m.Spec.Entrypoint = "/no/such/executable-amh-test"
	reg.Discover(ctx, m)

	if _, err := reg.Activate(ctx, "amh.test/broken", "1.0.0"); err == nil {
		t.Fatalf("expected activation of a nonexistent entrypoint to fail")
	}

	effects, err := effectsByType(ctx, ops, db, "extension_activate")
	if err != nil {
		t.Fatalf("query activate effects: %v", err)
	}
	if len(effects) != 1 {
		t.Fatalf("expected exactly one extension_activate effect, got %d", len(effects))
	}
	if effects[0].State != operations.StateFailed {
		t.Fatalf("expected activate effect state failed, got %s", effects[0].State)
	}
	if effects[0].ErrorCode == "" {
		t.Fatalf("expected a recorded error code on the failed effect")
	}
}

// effectsByType finds every effect_record row of effectType.
// Registry.Operations/operations.Engine give no "list all" method (by
// design: daemon/operations has no domain knowledge of what an
// operation_id means to its caller), so this test helper reads
// effect_record directly, the same way daemon/inference's own
// operations_test.go does for its analogous assertions. Each test using
// this runs against its own fresh schema (storetest.Open), so filtering
// by effect_type alone — without also needing an extension_id, which
// effect_record does not store (only its payload's digest) — is
// sufficient to isolate that test's own effects.
func effectsByType(ctx context.Context, ops *operations.Engine, db *sql.DB, effectType string) ([]*operations.Effect, error) {
	rows, err := db.QueryContext(ctx, `SELECT effect_id FROM effect_record WHERE effect_type = $1`, effectType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []*operations.Effect
	for _, id := range ids {
		eff, err := ops.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, eff)
	}
	return out, nil
}

func TestActivate_RefusesMissingRequirement(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/consumer", "1.0.0")
	m.Spec.Requires = []Requirement{{Capability: "amh.test/producer-cap", VersionRange: ">=1.0.0", Optional: false}}
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	if _, err := reg.Activate(ctx, "amh.test/consumer", "1.0.0"); err == nil {
		t.Fatalf("expected Activate to fail with no provider active for the required capability")
	}

	ext, err := reg.Get(ctx, "amh.test/consumer", "1.0.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ext.Status != StatusDiscovered {
		t.Fatalf("a failed dependency check must not mutate status; got %s", ext.Status)
	}
}

func TestActivate_SucceedsOnceDependencyIsActive(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	producer := baseManifest("amh.test/producer", "1.0.0")
	producer.Spec.Provides = []CapabilityRef{{ID: "amh.test/producer-cap", Version: "1.2.0"}}
	if _, err := reg.Discover(ctx, producer); err != nil {
		t.Fatalf("Discover producer: %v", err)
	}

	consumer := baseManifest("amh.test/consumer", "1.0.0")
	consumer.Spec.Requires = []Requirement{{Capability: "amh.test/producer-cap", VersionRange: ">=1.0.0 <2.0.0", Optional: false}}
	if _, err := reg.Discover(ctx, consumer); err != nil {
		t.Fatalf("Discover consumer: %v", err)
	}

	if _, err := reg.Activate(ctx, "amh.test/consumer", "1.0.0"); err == nil {
		t.Fatalf("expected activation to fail before the producer is active")
	}

	if _, err := reg.Activate(ctx, "amh.test/producer", "1.0.0"); err != nil {
		t.Fatalf("Activate producer: %v", err)
	}

	if _, err := reg.Activate(ctx, "amh.test/consumer", "1.0.0"); err != nil {
		t.Fatalf("Activate consumer once producer is active: %v", err)
	}
}

func TestQuiesce_RefusesWhileActiveDependentExists(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	producer := baseManifest("amh.test/producer", "1.0.0")
	producer.Spec.Provides = []CapabilityRef{{ID: "amh.test/producer-cap", Version: "1.0.0"}}
	reg.Discover(ctx, producer)
	reg.Activate(ctx, "amh.test/producer", "1.0.0")

	consumer := baseManifest("amh.test/consumer", "1.0.0")
	consumer.Spec.Requires = []Requirement{{Capability: "amh.test/producer-cap", VersionRange: ">=1.0.0", Optional: false}}
	reg.Discover(ctx, consumer)
	if _, err := reg.Activate(ctx, "amh.test/consumer", "1.0.0"); err != nil {
		t.Fatalf("Activate consumer: %v", err)
	}

	if _, err := reg.Quiesce(ctx, "amh.test/producer", "1.0.0"); err == nil {
		t.Fatalf("expected Quiesce to be refused while an active dependent needs this capability")
	}

	// Once the dependent is quiesced+disposed, the producer can quiesce.
	reg.Quiesce(ctx, "amh.test/consumer", "1.0.0")
	reg.Dispose(ctx, "amh.test/consumer", "1.0.0")
	if _, err := reg.Quiesce(ctx, "amh.test/producer", "1.0.0"); err != nil {
		t.Fatalf("expected Quiesce to succeed once the dependent is disposed: %v", err)
	}
}

func TestActivate_ProcessIsolation_RealProcessReceivesAWorkingCapabilityToken(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	scriptFile := filepath.Join(dir, "echo-token.sh")
	// launcher.go's IsolationProcess entrypoint parsing (strings.Fields)
	// has no shell-quoting awareness, so a real script file — a single,
	// space-free path as the entrypoint — is the reliable way to run
	// more than one shell statement here. Must survive launch()'s 200ms
	// "did it exit immediately" check, so write the real env var this
	// process actually received, then sleep — "exec sleep" so the sleep
	// process replaces this script's own process image rather than
	// running as its child: teardown() below kills exactly the PID
	// launch() recorded, and a plain (non-exec'd) "sleep 300" as a
	// separate child process would survive that kill as an orphan still
	// holding the stderr pipe open, hanging cmd.Wait() forever — the
	// same reason the existing sleep-300-as-entrypoint test never wraps
	// it in a shell at all.
	script := "#!/bin/sh\nprintenv AMH_EXTENSION_TOKEN > " + tokenFile + "\nexec sleep 300\n"
	if err := os.WriteFile(scriptFile, []byte(script), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	m := baseManifest("amh.test/token-widget", "1.0.0")
	m.Spec.Isolation = IsolationProcess
	m.Spec.Entrypoint = scriptFile
	if _, err := reg.Discover(ctx, m); err != nil {
		t.Fatalf("Discover: %v", err)
	}

	active, err := reg.Activate(ctx, "amh.test/token-widget", "1.0.0")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	pid, err := parsePID(active.RuntimeHandle)
	if err != nil {
		t.Fatalf("parse runtime handle %q: %v", active.RuntimeHandle, err)
	}
	defer syscall.Kill(pid, syscall.SIGKILL)

	var raw []byte
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = os.ReadFile(tokenFile)
		if err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(raw) == 0 {
		t.Fatalf("expected the real launched process to have written its AMH_EXTENSION_TOKEN env var to %s: %v", tokenFile, err)
	}
	token := strings.TrimSpace(string(raw))

	id, ok, err := reg.VerifyCapabilityToken(ctx, token)
	if err != nil {
		t.Fatalf("VerifyCapabilityToken: %v", err)
	}
	if !ok || id != "amh.test/token-widget" {
		t.Fatalf("expected the real process's own token to verify as amh.test/token-widget, got id=%q ok=%v", id, ok)
	}

	reg.Quiesce(ctx, "amh.test/token-widget", "1.0.0")
	if _, err := reg.Dispose(ctx, "amh.test/token-widget", "1.0.0"); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	if _, ok, err := reg.VerifyCapabilityToken(ctx, token); err != nil || ok {
		t.Fatalf("expected the token to stop verifying after Dispose: ok=%v err=%v", ok, err)
	}
}

func TestActivate_ProcessIsolation_LaunchesAndKillsRealOSProcess(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/proc-widget", "1.0.0")
	m.Spec.Isolation = IsolationProcess
	m.Spec.Entrypoint = "sleep 300"
	reg.Discover(ctx, m)

	active, err := reg.Activate(ctx, "amh.test/proc-widget", "1.0.0")
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	pid, err := parsePID(active.RuntimeHandle)
	if err != nil {
		t.Fatalf("parse runtime handle %q: %v", active.RuntimeHandle, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("expected process %d to be alive after Activate: %v", pid, err)
	}

	reg.Quiesce(ctx, "amh.test/proc-widget", "1.0.0")
	if _, err := reg.Dispose(ctx, "amh.test/proc-widget", "1.0.0"); err != nil {
		t.Fatalf("Dispose: %v", err)
	}

	if err := syscall.Kill(pid, 0); err == nil {
		t.Fatalf("expected process %d to be dead after Dispose", pid)
	}
}

func TestActivate_FailedLaunchRollsBackToFailedState(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/broken", "1.0.0")
	m.Spec.Isolation = IsolationProcess
	m.Spec.Entrypoint = "/no/such/executable-amh-test"
	reg.Discover(ctx, m)

	if _, err := reg.Activate(ctx, "amh.test/broken", "1.0.0"); err == nil {
		t.Fatalf("expected activation of a nonexistent entrypoint to fail")
	}

	ext, err := reg.Get(ctx, "amh.test/broken", "1.0.0")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ext.Status != StatusFailed {
		t.Fatalf("expected status failed after a launch error, got %s", ext.Status)
	}
	if ext.StatusReason == "" {
		t.Fatalf("expected a recorded status reason")
	}

	// A failed extension is retryable: fix nothing here (still broken),
	// but a subsequent Activate call must be permitted to attempt again
	// rather than being stuck.
	if _, err := reg.Activate(ctx, "amh.test/broken", "1.0.0"); err == nil {
		t.Fatalf("expected the retry to still fail (entrypoint is still missing)")
	}
}

func TestDispose_RefusesWithoutQuiesceFirst(t *testing.T) {
	db := testDB(t)
	reg := New(db)
	ctx := context.Background()

	m := baseManifest("amh.test/widget", "1.0.0")
	reg.Discover(ctx, m)
	reg.Activate(ctx, "amh.test/widget", "1.0.0")

	if _, err := reg.Dispose(ctx, "amh.test/widget", "1.0.0"); err == nil {
		t.Fatalf("expected Dispose to be refused on an active (not quiescing) extension")
	}
}

func TestSemverRanges(t *testing.T) {
	cases := []struct {
		version, rangeExpr string
		want               bool
	}{
		{"1.2.3", "1.2.3", true},
		{"1.2.3", "1.2.4", false},
		{"1.5.0", ">=1.0.0", true},
		{"0.9.0", ">=1.0.0", false},
		{"1.5.0", ">=1.0.0 <2.0.0", true},
		{"2.0.0", ">=1.0.0 <2.0.0", false},
		{"1.5.0", "^1.2.0", true},
		{"2.0.0", "^1.2.0", false},
		{"0.5.3", "^0.5.0", true},
		{"0.6.0", "^0.5.0", false},
	}
	for _, c := range cases {
		got, err := satisfiesRange(c.version, c.rangeExpr)
		if err != nil {
			t.Fatalf("satisfiesRange(%q, %q): %v", c.version, c.rangeExpr, err)
		}
		if got != c.want {
			t.Errorf("satisfiesRange(%q, %q) = %v, want %v", c.version, c.rangeExpr, got, c.want)
		}
	}
}

// parsePID extracts the numeric PID from a "pid:<n>" runtime handle.
func parsePID(handle string) (int, error) {
	return strconv.Atoi(strings.TrimPrefix(handle, "pid:"))
}
