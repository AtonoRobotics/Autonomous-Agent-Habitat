package backup

import (
	"bytes"
	"context"
	"testing"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store/storetest"
)

func TestBackupThenRestore_RoundTrips(t *testing.T) {
	db, _, schema := storetest.OpenScoped(t, "../../store/migrations")
	b := New(storetest.AdminURL())
	b.Schema = schema

	if _, err := db.Exec(`INSERT INTO account (id, provider, display_name, status) VALUES ('acct-1', 'test-provider', 'Test Account', 'active')`); err != nil {
		t.Fatalf("seed row: %v", err)
	}

	var snapshot bytes.Buffer
	if err := b.Backup(context.Background(), &snapshot); err != nil {
		t.Fatalf("Backup: %v", err)
	}
	if snapshot.Len() == 0 {
		t.Fatalf("expected a non-empty backup")
	}

	if _, err := db.Exec(`DELETE FROM account WHERE id = 'acct-1'`); err != nil {
		t.Fatalf("delete seeded row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO account (id, provider, display_name, status) VALUES ('acct-2', 'test-provider', 'Post-Backup Account', 'active')`); err != nil {
		t.Fatalf("insert a row that only exists after the backup: %v", err)
	}

	if err := b.Restore(context.Background(), bytes.NewReader(snapshot.Bytes())); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	var restoredCount, postBackupCount int
	if err := db.QueryRow(`SELECT COUNT(*) FROM account WHERE id = 'acct-1'`).Scan(&restoredCount); err != nil {
		t.Fatalf("query restored row: %v", err)
	}
	if restoredCount != 1 {
		t.Fatalf("expected the pre-backup row to come back after restore, got count=%d", restoredCount)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM account WHERE id = 'acct-2'`).Scan(&postBackupCount); err != nil {
		t.Fatalf("query post-backup row: %v", err)
	}
	if postBackupCount != 0 {
		t.Fatalf("expected the row inserted after the backup to be gone after restore, got count=%d", postBackupCount)
	}
}

func TestBackup_FailsClosedForUnreachableDatabase(t *testing.T) {
	b := New("postgresql://nobody:nothing@127.0.0.1:1/does-not-exist")
	var buf bytes.Buffer
	if err := b.Backup(context.Background(), &buf); err == nil {
		t.Fatalf("expected Backup to fail against an unreachable database")
	}
}

func TestRestore_FailsClosedOnCorruptSnapshot(t *testing.T) {
	_, _, schema := storetest.OpenScoped(t, "../../store/migrations")
	b := New(storetest.AdminURL())
	b.Schema = schema

	if err := b.Restore(context.Background(), bytes.NewReader([]byte("not a real pg_dump archive"))); err == nil {
		t.Fatalf("expected Restore to fail on a corrupt snapshot")
	}
}
