package store

import (
	"database/sql"
	"errors"
	"testing"
)

// mostRecentAppliedMigration re-derives "most recent" the same way
// Rollback does (ORDER BY filename DESC), rather than hardcoding a
// specific migration's filename — a fragile assumption that breaks every
// time a new migration file is added.
func mostRecentAppliedMigration(t *testing.T, db *sql.DB) string {
	t.Helper()
	var name string
	if err := db.QueryRow(`SELECT filename FROM schema_migrations ORDER BY filename DESC LIMIT 1`).Scan(&name); err != nil {
		t.Fatalf("query most recent applied migration: %v", err)
	}
	return name
}

func TestRollback_ReversesMostRecentMigration(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	want := mostRecentAppliedMigration(t, db)
	wantCount := countSQLFiles(t, migrationsDir)

	rolledBack, err := Rollback(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if rolledBack != want {
		t.Fatalf("expected to roll back %s (the most recently applied), got %q", want, rolledBack)
	}

	var applied int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if applied != wantCount-1 {
		t.Fatalf("expected %d applied migrations after rolling back one of %d, got %d", wantCount-1, wantCount, applied)
	}
}

func TestRollback_ThenReapplyForward_IsClean(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	rolledBackName := mostRecentAppliedMigration(t, db)
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

	var applied int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = $1`, rolledBackName).Scan(&applied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected %s to be re-applied and recorded exactly once, got %d", rolledBackName, applied)
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
