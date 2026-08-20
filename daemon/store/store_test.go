package store

import (
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "amh.db")
	migrationsDir := "../../store/migrations"

	db, err := Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_action`).Scan(&n); err != nil {
		t.Fatalf("query ontology table: %v", err)
	}
	db.Close()

	// Re-open against the same file: migrations must not re-apply or error.
	db2, err := Open(dbPath, migrationsDir)
	if err != nil {
		t.Fatalf("second open (restart survival): %v", err)
	}
	defer db2.Close()

	var applied int
	if err := db2.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&applied); err != nil {
		t.Fatalf("query schema_migrations: %v", err)
	}
	if applied != 1 {
		t.Fatalf("expected exactly 1 recorded migration, got %d", applied)
	}
}
