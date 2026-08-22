package extensions

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
	"syscall"
	"testing"

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

	// The dispose effect must be recorded as activation's verified inverse.
	var effectType, outcome string
	err = db.QueryRow(`SELECT effect_type, outcome FROM extension_effect WHERE extension_id = $1 AND effect_type = 'dispose'`, "amh.test/widget").Scan(&effectType, &outcome)
	if err != nil {
		t.Fatalf("query dispose effect: %v", err)
	}
	if outcome != "success" {
		t.Fatalf("expected dispose effect outcome success, got %s", outcome)
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
