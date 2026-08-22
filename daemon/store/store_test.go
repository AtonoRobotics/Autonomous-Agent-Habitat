package store

import (
	"os"
	"path/filepath"
	"strings"
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
	want := countSQLFiles(t, migrationsDir)
	if applied != want {
		t.Fatalf("expected exactly %d recorded migrations (one per *.sql file in %s), got %d — re-opening must not re-apply or skip any", want, migrationsDir, applied)
	}
}

// countSQLFiles counts *.sql files directly rather than hardcoding a
// literal count, so this test keeps proving idempotency (not "re-applying
// migrations" and not "silently dropping one") without needing an edit
// every time a new migration file is added.
func countSQLFiles(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sql") {
			n++
		}
	}
	return n
}
