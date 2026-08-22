package sandbox

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	dir := t.TempDir()
	db, err := store.Open(filepath.Join(dir, "amh.db"), "../../store/migrations")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func seedAgent(t *testing.T, db *sql.DB, id string) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO agent (id, kind) VALUES (?, 'worker')`, id); err != nil {
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
