package store

import (
	"database/sql"
	"errors"
	"testing"
)

func TestRollback_ReversesMostRecentMigration(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// trusted_signing_key is 0005's table — it must exist before rollback
	// and be gone after, and the down-migration's own DROP TABLE is the
	// only thing that could remove it (nothing else in this test does).
	if _, err := db.Exec(`SELECT 1 FROM trusted_signing_key LIMIT 0`); err != nil {
		t.Fatalf("expected trusted_signing_key to exist before rollback: %v", err)
	}

	rolledBack, err := Rollback(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack != "0005_extension_trust.sql" {
		t.Fatalf("expected to roll back 0005_extension_trust.sql, got %q", rolledBack)
	}

	if _, err := db.Exec(`SELECT 1 FROM trusted_signing_key LIMIT 0`); err == nil {
		t.Fatalf("expected trusted_signing_key to be gone after rollback")
	}

	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = '0005_extension_trust.sql'`).Scan(&applied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if applied != 0 {
		t.Fatalf("expected 0005_extension_trust.sql to be unrecorded after rollback, still found %d row(s)", applied)
	}
}

func TestRollback_ThenReapplyForward_IsClean(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()

	if _, err := Rollback(dbURL, migrationsDir); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	// Re-opening must re-apply exactly the migration that was rolled back,
	// not error and not skip it.
	db2, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("re-Open after rollback: %v", err)
	}
	defer db2.Close()

	if _, err := db2.Exec(`SELECT 1 FROM trusted_signing_key LIMIT 0`); err != nil {
		t.Fatalf("expected trusted_signing_key to exist again after re-applying forward: %v", err)
	}
}

func TestRollback_AllTheWayDown_LeavesAnEmptySchema(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()

	for i := 0; i < countSQLFiles(t, migrationsDir); i++ {
		if _, err := Rollback(dbURL, migrationsDir); err != nil {
			t.Fatalf("Rollback step %d: %v", i, err)
		}
	}

	if _, err := Rollback(dbURL, migrationsDir); !errors.Is(err, ErrNoMigrationsApplied) {
		t.Fatalf("expected ErrNoMigrationsApplied once every migration is rolled back, got %v", err)
	}

	// schema_migrations itself is created by migrate(), not a tracked
	// migration file, so it — and it alone — should still be there, empty.
	admin, err := sql.Open("pgx", dbURL)
	if err != nil {
		t.Fatalf("open verification connection: %v", err)
	}
	defer admin.Close()
	var tableCount int
	if err := admin.QueryRow(`SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = current_schema() AND table_name != 'schema_migrations'`).Scan(&tableCount); err != nil {
		t.Fatalf("count remaining tables: %v", err)
	}
	if tableCount != 0 {
		t.Fatalf("expected no tables left besides schema_migrations after rolling back everything, found %d", tableCount)
	}
}

func TestRollback_NoMigrationsApplied(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	// Create the schema_migrations table (via Open) but immediately roll
	// back everything, then try once more against the now-empty table.
	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	db.Close()
	for i := 0; i < countSQLFiles(t, migrationsDir); i++ {
		if _, err := Rollback(dbURL, migrationsDir); err != nil {
			t.Fatalf("Rollback step %d: %v", i, err)
		}
	}

	if _, err := Rollback(dbURL, migrationsDir); !errors.Is(err, ErrNoMigrationsApplied) {
		t.Fatalf("expected ErrNoMigrationsApplied, got %v", err)
	}
}
