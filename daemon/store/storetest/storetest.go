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

// AdminURL returns the resolved connection URL Open/OpenScoped provision
// test schemas against (AMH_TEST_DATABASE_URL if set, else the local
// default), with no schema-scoping "options" query parameter applied. A
// tool that scopes to one schema through its own flag rather than a libpq
// "options=-c search_path=..." URI parameter — pg_dump/pg_restore's
// --schema, in particular (see daemon/backup's tests) — needs this
// unscoped form paired with the schema name OpenScoped also returns:
// libpq's own URI conninfo parser (unlike pgx's) does not decode a "+" in
// a query value back to a space, so the scoped URL Open/OpenScoped hand to
// database/sql cannot be reused as-is with a CLI tool that links libpq.
func AdminURL() string {
	if v := os.Getenv("AMH_TEST_DATABASE_URL"); v != "" {
		return v
	}
	return defaultAdminURL
}

// Open provisions a fresh schema in the shared test Postgres instance,
// applies store/migrations/*.sql (resolved relative to the repo root via
// migrationsDir), and returns a *sql.DB scoped to that schema. The schema
// is dropped when the test completes.
func Open(t *testing.T, migrationsDir string) *sql.DB {
	t.Helper()
	db, _, _ := open(t, migrationsDir)
	return db
}

// OpenScoped is Open, but also returns the schema-scoped connection URL
// and bare schema name — for tests (daemon/backup's, in particular) that
// need to drive something like pg_dump/pg_restore directly against the
// same isolated schema Open's *sql.DB is connected to, rather than only
// through database/sql.
func OpenScoped(t *testing.T, migrationsDir string) (db *sql.DB, scopedURL, schema string) {
	t.Helper()
	return open(t, migrationsDir)
}

func open(t *testing.T, migrationsDir string) (db *sql.DB, scopedURL, schema string) {
	t.Helper()

	adminURL := AdminURL()

	admin, err := sql.Open("pgx", adminURL)
	if err != nil {
		t.Fatalf("storetest: open admin connection: %v", err)
	}
	defer admin.Close()

	schema = fmt.Sprintf("test_%d_%d", os.Getpid(), rand.Int63())
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

	scopedURL, err = withSchema(adminURL, schema)
	if err != nil {
		t.Fatalf("storetest: build scoped URL: %v", err)
	}

	db, err = store.Open(scopedURL, migrationsDir)
	if err != nil {
		t.Fatalf("storetest: open+migrate schema %s: %v", schema, err)
	}
	t.Cleanup(func() { db.Close() })
	return db, scopedURL, schema
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
