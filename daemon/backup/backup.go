// Package backup gives the daemon a real, operator-triggered backup and
// restore capability against its own PostgreSQL store (§14: "backup/
// restore, upgrade/rollback, corruption recovery, resource exhaustion, and
// soak acceptance" — this package is the first, concrete slice of that
// list; the rest remain a documented, undone gap — see README).
//
// It shells out to pg_dump/pg_restore, the same client binaries any
// PostgreSQL deployment already ships alongside itself, rather than
// reimplementing Postgres's own dump format. A snapshot is Postgres's
// custom archive format (-Fc): compressed, and what pg_restore expects.
package backup

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
)

// Backer performs backup/restore against one PostgreSQL connection URL.
// Schema, if set, scopes both pg_dump and pg_restore to a single schema
// (--schema) rather than the whole database — the production daemon
// leaves this empty (it owns the whole database it's configured to use),
// but it's what lets this package's own tests run against one isolated
// schema in a shared test Postgres instance without also dumping or
// clobbering every other test's schema alongside it.
type Backer struct {
	DBURL  string
	Schema string
}

func New(dbURL string) *Backer {
	return &Backer{DBURL: dbURL}
}

// Backup streams a pg_dump custom-format snapshot to w.
func (b *Backer) Backup(ctx context.Context, w io.Writer) error {
	args := []string{"-Fc"}
	if b.Schema != "" {
		args = append(args, "--schema", b.Schema)
	}
	args = append(args, b.DBURL)

	cmd := exec.CommandContext(ctx, "pg_dump", args...)
	cmd.Stdout = w
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("backup: pg_dump: %w: %s", err, stderr.String())
	}
	return nil
}

// Restore applies a pg_dump custom-format snapshot read from r, replacing
// whatever the target schema (or database, if Schema is unset) currently
// holds. --single-transaction makes this atomic — either the entire
// restore commits, or none of it does, rather than leaving the target
// half-dropped, half-recreated on failure. This is inherently destructive
// to the target's current contents; it is the caller's job to only invoke
// it against a target whose existing contents are meant to be replaced.
func (b *Backer) Restore(ctx context.Context, r io.Reader) error {
	args := []string{"--clean", "--if-exists", "--no-owner", "--single-transaction"}
	if b.Schema != "" {
		args = append(args, "--schema", b.Schema)
	}
	args = append(args, "-d", b.DBURL)

	cmd := exec.CommandContext(ctx, "pg_restore", args...)
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("restore: pg_restore: %w: %s", err, stderr.String())
	}
	return nil
}
