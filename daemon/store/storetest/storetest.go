// Package storetest provisions an isolated PostgreSQL schema per test —
// the Postgres equivalent of the old "one temp SQLite file per test"
// pattern every daemon package's tests used. It's a separate package
// (rather than living directly in daemon/store) so importing it — and its
// "testing" dependency — doesn't reach into the production amh-daemon
// binary, which only imports daemon/store itself.
//
// Connects to AMH_TEST_DATABASE_URL if set (a URL with no schema/database
// segment baked in — this package appends its own), otherwise to a local
// default suitable for this repo's dev/CI environment. Each test gets its
// own schema, created fresh and dropped on cleanup, so tests never see
// another test's rows without needing per-test database creation (heavier,
// and CREATE DATABASE can't run inside a transaction).
package storetest

import (
	"database/sql"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

const defaultAdminURL = "postgresql://postgres:postgres@127.0.0.1:5432/postgres"

// Open provisions a fresh schema in the shared test Postgres instance,
// applies store/migrations/*.sql (resolved relative to the repo root via
// migrationsDir), and returns a *sql.DB scoped to that schema. The schema
// is dropped when the test completes.
func Open(t *testing.T, migrationsDir string) *sql.DB {
	t.Helper()

	adminURL := os.Getenv("AMH_TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = defaultAdminURL
	}

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("storetest: open admin connection: %v", err)
	}
	defer admin.Close()

	schema := fmt.Sprintf("test_%d_%d", os.Getpid(), rand.Int63())
	if _, err := admin.Exec(fmt.Sprintf(`CREATE SCHEMA %q`, schema)); err != nil {
		t.Fatalf("storetest: create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		cleanup, err := sql.Open("pgx", adminURL)
		if err != nil {
			return
		}
		defer cleanup.Close()
		cleanup.Exec(fmt.Sprintf(`DROP SCHEMA IF EXISTS %q CASCADE`, schema))
	})

	scopedURL, err := withSchema(adminURL, schema)
	if err != nil {
		t.Fatalf("storetest: build scoped URL: %v", err)
	}

	db, err := store.Open(scopedURL, migrationsDir)
	if err != nil {
		t.Fatalf("storetest: open+migrate schema %s: %v", schema, err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// withSchema appends a search_path option to a Postgres connection URL so
// every connection made through it defaults to the given schema without
// needing "schema." prefixes throughout the codebase's SQL.
func withSchema(rawURL, schema string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse %q: %w", rawURL, err)
	}
	q := u.Query()
	q.Set("options", fmt.Sprintf("-c search_path=%s", schema))
	u.RawQuery = q.Encode()
	return u.String(), nil
}
