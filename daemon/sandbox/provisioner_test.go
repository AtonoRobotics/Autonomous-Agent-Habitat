package sandbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	return storetest.Open(t, "../../store/migrations")
}

func seedAgent(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO agent (id, kind) VALUES ($1, 'worker')`, id); err != nil {
		t.Fatalf("seed agent: %v", err)
	}
}

func TestCreate_ProcessIsolation_LaunchesRealNamespacedProcess(t *testing.T) {
	db := testDB(t)
	seedAgent(t, db, "agent-1")
	seedAgent(t, db, "agent-2")
	p := New(db, t.TempDir())
	ctx := context.Background()

	c, err := p.Create(ctx, "agent-1", IsolationProcess, "sleep 300", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if c.Status != StatusReady {
		t.Fatalf("expected ready, got %s", c.Status)
	}
	if c.RuntimeHandle == "" {
		t.Fatalf("expected a runtime handle")
	}
	if _, err := os.Stat(c.Workdir); err != nil {
		t.Fatalf("expected workdir to exist: %v", err)
	}

	// The computer's workdir must be private, agent-specific storage.
	other, err := p.Create(ctx, "agent-2", IsolationProcess, "sleep 300", nil)
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if other.Workdir == c.Workdir {
		t.Fatalf("expected distinct workdirs per computer")
	}
	p.Destroy(ctx, other.ID, "test cleanup")

	destroyed, err := p.Destroy(ctx, c.ID, "test done")
	if err != nil {
		t.Fatalf("Destroy: %v", err)
	}
	if destroyed.Status != StatusDestroyed {
		t.Fatalf("expected destroyed, got %s", destroyed.Status)
	}
	if _, err := os.Stat(c.Workdir); err != nil {
		t.Fatalf("expected workdir to survive Destroy (artifacts outlive the compute instance): %v", err)
	}
}

// TestCreate_ProcessIsolation_HardensFilesystemAndDropsPrivileges guards
// against the real gap PR #3's review found: a process-isolated computer
// used to run its target command with the daemon's own full root
// identity and an unrestricted view of the host filesystem — `unshare
// --mount` alone gives a new mount namespace, not a sandboxed one. This
// proves both halves of the fix hold in a real launched process, not
// just in the script's own text: the command executes as an unprivileged
// UID (never 0), and a write that would otherwise succeed for root
// (writing into /tmp, world-writable, on the same filesystem as / in
// this test environment) is refused once the mount namespace's root is
// read-only — while the computer's own workdir remains fully writable.
func TestCreate_ProcessIsolation_HardensFilesystemAndDropsPrivileges(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("this test proves root-level access is actually restricted — meaningless unless the test process itself runs as root, matching this package's documented deployment assumption")
	}

	db := testDB(t)
	seedAgent(t, db, "agent-1")
	// Deliberately not t.TempDir(): its parent directories are created
	// 0700, which would block the unprivileged "nobody" identity from
	// even traversing down to baseDir regardless of baseDir's own mode —
	// a permission-chain problem this test needs to avoid, not exercise.
	// os.MkdirTemp("", ...) creates directly under os.TempDir() (/tmp),
	// which is itself world-traversable.
	baseDir, err := os.MkdirTemp("", "amh-sandbox-hardening-test-")
	if err != nil {
		t.Fatalf("create base dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(baseDir) })
	if err := os.Chmod(baseDir, 0o755); err != nil {
		t.Fatalf("chmod base dir: %v", err)
	}

	probePath := filepath.Join(baseDir, "probe.sh")
	outsideTarget := filepath.Join(os.TempDir(), "amh-sandbox-hardening-probe-"+t.Name())
	os.Remove(outsideTarget) // best-effort: a leftover from a prior failed run must not produce a false pass
	script := "#!/bin/sh\n" +
		`id -u > "$PWD/uid.txt"` + "\n" +
		`if echo blocked > ` + outsideTarget + ` 2>/dev/null; then echo WROTE > "$PWD/write-result.txt"; else echo BLOCKED > "$PWD/write-result.txt"; fi` + "\n" +
		`echo ready > "$PWD/ready.txt"` + "\n" +
		`exec sleep 300` + "\n"
	if err := os.WriteFile(probePath, []byte(script), 0o755); err != nil {
		t.Fatalf("write probe script: %v", err)
	}
	t.Cleanup(func() { os.Remove(outsideTarget) })

	p := New(db, baseDir)
	ctx := context.Background()
	c, err := p.Create(ctx, "agent-1", IsolationProcess, probePath, nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { p.Destroy(ctx, c.ID, "test cleanup") })

	readyPath := filepath.Join(c.Workdir, "ready.txt")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("probe script never finished (no %s within 5s)", readyPath)
		}
		time.Sleep(20 * time.Millisecond)
	}

	uidBytes, err := os.ReadFile(filepath.Join(c.Workdir, "uid.txt"))
	if err != nil {
		t.Fatalf("read uid.txt: %v", err)
	}
	if got := strings.TrimSpace(string(uidBytes)); got != unprivilegedUID {
		t.Fatalf("expected the target command to run as uid %s (nobody), got %q", unprivilegedUID, got)
	}

	writeResult, err := os.ReadFile(filepath.Join(c.Workdir, "write-result.txt"))
	if err != nil {
		t.Fatalf("read write-result.txt: %v", err)
	}
	if got := strings.TrimSpace(string(writeResult)); got != "BLOCKED" {
		t.Fatalf("expected the write outside the workdir to be blocked by the read-only remount, got %q", got)
	}
	if _, err := os.Stat(outsideTarget); err == nil {
		t.Fatalf("the target command actually wrote %s on the host — isolation did not hold", outsideTarget)
	}
}

func TestDestroy_ActuallyKillsTheProcessTree(t *testing.T) {
	db := testDB(t)
	seedAgent(t, db, "agent-1")
	p := New(db, t.TempDir())
	ctx := context.Background()

	c, err := p.Create(ctx, "agent-1", IsolationProcess, "sleep 300", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var pgid int
	if _, err := scanPgid(c.RuntimeHandle, &pgid); err != nil {
		t.Fatalf("parse runtime handle %q: %v", c.RuntimeHandle, err)
	}
	if err := syscall.Kill(-pgid, 0); err != nil {
		t.Fatalf("expected process group %d to be alive: %v", pgid, err)
	}

	if _, err := p.Destroy(ctx, c.ID, "kill check"); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if err := syscall.Kill(-pgid, 0); err == nil {
		t.Fatalf("expected process group %d to be dead after Destroy", pgid)
	}
}

func TestCreate_RejectsInvalidIsolationAndEmptyImage(t *testing.T) {
	db := testDB(t)
	p := New(db, t.TempDir())
	ctx := context.Background()

	if _, err := p.Create(ctx, "agent-1", "vm", "sleep 1", nil); err == nil {
		t.Fatalf("expected an error for an unsupported isolation value")
	}
	if _, err := p.Create(ctx, "agent-1", IsolationProcess, "", nil); err == nil {
		t.Fatalf("expected an error for an empty image/command")
	}
}

func TestCreate_FailedLaunchRecordsFailedStatus(t *testing.T) {
	db := testDB(t)
	seedAgent(t, db, "agent-1")
	p := New(db, t.TempDir())
	ctx := context.Background()

	if _, err := p.Create(ctx, "agent-1", IsolationProcess, "/no/such/binary-amh-sandbox-test", nil); err == nil {
		t.Fatalf("expected launch of a nonexistent binary to fail")
	}

	rows, err := db.Query(`SELECT status, destroy_reason FROM computer WHERE image = '/no/such/binary-amh-sandbox-test'`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expected a computer row to have been recorded even on launch failure")
	}
	var status, reason string
	rows.Scan(&status, &reason)
	if status != "failed" || reason == "" {
		t.Fatalf("expected status=failed with a reason, got status=%s reason=%q", status, reason)
	}
}

func TestDestroy_RefusesUnknownComputer(t *testing.T) {
	db := testDB(t)
	p := New(db, t.TempDir())
	if _, err := p.Destroy(context.Background(), "no-such-id", "x"); err == nil {
		t.Fatalf("expected an error destroying an unknown computer")
	}
}

func TestListForAgent_ExcludesDestroyed(t *testing.T) {
	db := testDB(t)
	seedAgent(t, db, "agent-1")
	p := New(db, t.TempDir())
	ctx := context.Background()

	a, _ := p.Create(ctx, "agent-1", IsolationProcess, "sleep 300", nil)
	b, _ := p.Create(ctx, "agent-1", IsolationProcess, "sleep 300", nil)
	p.Destroy(ctx, a.ID, "done")

	list, err := p.ListForAgent(ctx, "agent-1")
	if err != nil {
		t.Fatalf("ListForAgent: %v", err)
	}
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("expected exactly the non-destroyed computer %s, got %+v", b.ID, list)
	}
}

func scanPgid(handle string, out *int) (bool, error) {
	const prefix = "pgid:"
	n := 0
	for _, c := range handle[len(prefix):] {
		n = n*10 + int(c-'0')
	}
	*out = n
	return true, nil
}
