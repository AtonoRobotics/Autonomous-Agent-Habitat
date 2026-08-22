// Package store owns the daemon's PostgreSQL handle and migration
// application. See docs/AMH-SPECIFICATION.md §1 (decision 4: "Postgres is
// authoritative persistent state") and §3.3.
//
// Migrations live in store/migrations/ at the repo root — the single
// canonical DDL source shared by the Go daemon and the Python agent layer
// (both connect to the same Postgres cluster). They are read from disk
// rather than go:embed'd, since embed cannot reach outside this package's
// own directory tree; deployment packaging must ship store/migrations/
// alongside the amh-daemon binary (see deploy/).
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open connects to the PostgreSQL database named by dbURL (a standard
// postgres:// or postgresql:// connection string — a target schema, if
// any, is part of that URL, e.g. via ?options=-c%20search_path%3Dfoo) and
// applies any *.sql files from migrationsDir that haven't run yet, in
// filename order. Applied migrations are tracked in a schema_migrations
// table. Unlike SQLite, the target database itself must already exist —
// Postgres cannot create a database over a connection to that same
// database; provisioning it (or the schema within it) is the caller's job.
func Open(dbURL, migrationsDir string) (*sql.DB, error) {
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	if err := migrate(db, migrationsDir); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return db, nil
}

func migrate(db *sql.DB, dir string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (
		filename TEXT PRIMARY KEY,
		applied_at TEXT NOT NULL DEFAULT (to_char(clock_timestamp() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
	)`); err != nil {
		return err
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read migrations dir %s: %w", dir, err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".sql" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		var applied int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE filename = $1`, name).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		sqlBytes, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.Exec(string(sqlBytes)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (filename) VALUES ($1)`, name); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}
