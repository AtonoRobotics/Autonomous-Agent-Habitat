package cognition

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRun_UnconfiguredCommand_BlocksUntilCancelledThenReturnsNil(t *testing.T) {
	w := &Worker{}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("expected Run to block while unconfigured, got early return: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected nil on cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return after cancellation")
	}
}

func TestRun_LongRunningProcess_KilledCleanlyOnCancellation(t *testing.T) {
	w := &Worker{Command: "sleep 300"}
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("expected the sleep process to still be running, got early return: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected a cancellation-driven exit to report nil (a clean stop, not a crash), got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("Run did not return after cancellation — process may not have been killed")
	}
}

func TestRun_ProcessExitsCleanlyWhileStillRunning_IsReportedAsAnError(t *testing.T) {
	// A worker that's supposed to run forever exiting on its own — even
	// with status 0 — must be reported as an error so the supervisor's
	// restart strategy actually kicks in, not silently treated as "done."
	w := &Worker{Command: "true"}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatalf("expected an error when the worker process exits unexpectedly, got nil")
	}
}

func TestRun_ProcessExitsWithFailure_PropagatesTheError(t *testing.T) {
	w := &Worker{Command: "false"}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatalf("expected an error when the worker process exits non-zero")
	}
}

func TestRun_NonexistentCommand_ReturnsAnError(t *testing.T) {
	w := &Worker{Command: "/no/such/executable-amh-cognition-test"}
	err := w.Run(context.Background())
	if err == nil {
		t.Fatalf("expected an error for a command that cannot even start")
	}
}

func TestRun_UsesConfiguredWorkingDirectory(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")

	w := &Worker{Command: "sh -c pwd>marker", Dir: dir}
	if err := w.Run(context.Background()); err == nil {
		t.Fatalf("expected the 'exits cleanly while running' error even for a real successful command")
	}

	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected the command to have run inside Dir and written a marker file: %v", err)
	}
	gotDir, err := filepath.EvalSymlinks(string(got[:len(got)-1])) // strip trailing newline from `pwd`
	if err != nil {
		t.Fatalf("resolve pwd output: %v", err)
	}
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve want dir: %v", err)
	}
	if gotDir != wantDir {
		t.Fatalf("expected the command to run in %q, ran in %q", wantDir, gotDir)
	}
}

func TestRun_EmptyCommandString_IsANoOpNotAnEmptyExecError(t *testing.T) {
	// Distinguish a genuinely-unset Command (soft-disabled, blocks
	// forever) from a Command that parses to zero fields some other way
	// (e.g. all whitespace) — the latter is a real misconfiguration and
	// should error immediately rather than silently blocking forever.
	w := &Worker{Command: "   "}
	if err := w.Run(context.Background()); err == nil {
		t.Fatalf("expected an error for a whitespace-only command")
	}
}
