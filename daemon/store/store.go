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
//
// Open only ever applies migrations forward, in filename order — Rollback
// (§14: "upgrade/rollback") is the separate, explicit reverse operation,
// using each migration's paired down-migration file under
// store/migrations/down/ (same filename, opposite direction). A migration
// with no down file simply can't be rolled back through this mechanism;
// see Rollback's doc comment.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	// ErrNoMigrationsApplied means Rollback was called against a database
	// with nothing recorded in schema_migrations to roll back.
	ErrNoMigrationsApplied = errors.New("store: no migrations are recorded as applied")

	// ErrNoDownMigration means the most recently applied migration has no
	// paired file under migrationsDir/down — Rollback fails closed rather
	// than silently doing nothing, or rolling back some other migration
	// instead of the one actually most recent.
	ErrNoDownMigration = errors.New("store: most recently applied migration has no paired down-migration file")
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

// Rollback reverses exactly the most recently applied migration recorded
// in schema_migrations, using its paired down-migration file under
// migrationsDir/down — e.g. up-migration migrationsDir/0001_init.sql
// pairs with migrationsDir/down/0001_init.sql (§14: "upgrade/rollback").
// It fails closed with ErrNoDownMigration if that file doesn't exist,
// rather than silently doing nothing or rolling back some other
// migration than the one actually most recent. Call it repeatedly (once
// per returned name) to roll back more than one migration; there is
// deliberately no "roll back N at once" variant — each call re-derives
// "most recent" from the database's own current state, so a caller can't
// roll back a migration that was never actually applied.
//
// Unlike Open, Rollback does not also apply pending forward migrations
// first — it is a maintenance operation run against a stopped or
// otherwise quiesced daemon (see cmd/amh-daemon's -rollback-migration
// flag), not something the running admin API exposes live.
func Rollback(dbURL, migrationsDir string) (rolledBack string, err error) {
	db, err := sql.Open("pgx", dbURL)
	if err != nil {
		return "", fmt.Errorf("store: open for rollback: %w", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return "", fmt.Errorf("store: ping for rollback: %w", err)
	}

	var name string
	err = db.QueryRow(`SELECT filename FROM schema_migrations ORDER BY filename DESC LIMIT 1`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNoMigrationsApplied
	}
	if err != nil {
		return "", fmt.Errorf("store: find most recently applied migration: %w", err)
	}

	downPath := filepath.Join(migrationsDir, "down", name)
	downSQL, err := os.ReadFile(downPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("%w: %s", ErrNoDownMigration, name)
		}
		return "", fmt.Errorf("store: read down migration for %s: %w", name, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return "", fmt.Errorf("store: begin rollback tx for %s: %w", name, err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(string(downSQL)); err != nil {
		return "", fmt.Errorf("store: apply down migration for %s: %w", name, err)
	}
	if _, err := tx.Exec(`DELETE FROM schema_migrations WHERE filename = $1`, name); err != nil {
		return "", fmt.Errorf("store: unrecord migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("store: commit rollback of %s: %w", name, err)
	}
	return name, nil
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
