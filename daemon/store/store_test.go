package store

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"testing"
)

const testAdminURL = "postgresql://postgres:postgres@127.0.0.1:5432/postgres"

// freshSchemaURL provisions a fresh Postgres schema directly (this test
// exercises Open itself, so it can't depend on storetest — that package
// imports store, which would cycle) and returns a connection URL scoped to
// it via search_path, cleaned up when the test completes.
func freshSchemaURL(t *testing.T) string {
	t.Helper()
	adminURL := os.Getenv("AMH_TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = testAdminURL
	}
	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("test_%d_%d", os.Getpid(), rand.Int63())
	if _, err := admin.Exec(fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		cleanup.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
	})

	u, err := url.Parse(adminURL)
	if err != nil {
		t.Fatalf("parse admin URL: %v", err)
	}
	q := u.Query()
	q.Set("options", fmt.Sprintf("-c search_path=%s", schema))
	u.RawQuery = q.Encode()
	return u.String()
}

func TestOpenAppliesMigrationsAndIsIdempotent(t *testing.T) {
	dbURL := freshSchemaURL(t)
	migrationsDir := "../../store/migrations"

	db, err := Open(dbURL, migrationsDir)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM device_action`).Scan(&n); err != nil {
		t.Fatalf("query ontology table: %v", err)
	}
	db.Close()

	// Re-open against the same schema: migrations must not re-apply or error.
	db2, err := Open(dbURL, migrationsDir)
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
