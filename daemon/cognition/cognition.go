// Package cognition supervises the Python cognition worker (the DBOS-based
// agent workflow process — see agents/workflows/dispatcher.py's run_forever)
// as a child of amh-daemon's own supervisor tree, satisfying
// docs/AMH-SPECIFICATION.md §11's self-healing ownership table:
//
//	Python cognition worker | Go supervisor; DBOS resumes durable workflow
//
// Before this package, nothing in daemon/ supervised the Python cognition
// layer at all — every existing supervisor.Child (scheduler, health, api,
// mcp, a2a) is a native Go process. A crashed cognition worker had no
// restart path; this package gives it the same OneForOne restart the rest
// of the supervisor tree already provides.
package cognition

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Worker runs Command as a supervised OS process. Deliberately not
// hardcoded to python/uv/any particular interpreter or a relative
// agents/ path — Command is an operator-configured, space-separated
// command line (the exact same convention
// daemon/extensions/launcher.go's IsolationProcess already uses for an
// arbitrary configured entrypoint), so this package, and the Go core it
// lives in, stays agnostic to how the Python cognition layer is actually
// invoked in a given deployment.
//
// An empty Command is a soft-disable, not a startup refusal: Run blocks
// until ctx is done and returns nil, the same "optional dependency,
// no-op when unconfigured" posture daemon/inference.Router.Operations
// and daemon/api.Server.Credentials already take — a habitat that hasn't
// configured a cognition worker command still starts cleanly, exactly as
// it did before this package existed.
type Worker struct {
	// Command is a space-separated command line, e.g.
	// "uv run python -m workflows.dispatcher".
	Command string
	// Dir is the working directory Command runs in (e.g. the agents/
	// directory) — empty means amh-daemon's own working directory.
	Dir string
}

// Run matches supervisor.Child.Run's contract: blocks until ctx is
// cancelled or the child's own work is done/failed. The command
// inherits amh-daemon's own environment (Go's exec.Cmd default when Env
// is left nil) — including AMH_API_BASE_URL, which main.go already
// bootstraps for exactly this "any child process needs to call back into
// the daemon's own API" reason (see main.go's doc comment on that).
//
// Any exit while ctx is still active — clean (status 0) or not — is
// reported as a non-nil error: a worker declared to run forever exiting
// early is unexpected regardless of its exit code, and only a non-nil
// error trips the supervisor's restart strategy (see supervisor.Child's
// doc comment). Only an exit caused by ctx itself being cancelled (this
// daemon shutting down) is a clean stop.
//
// cmd.Cancel sends SIGTERM (not the exec.CommandContext default SIGKILL)
// when ctx is cancelled, with a 5-second WaitDelay before a hard kill —
// the same grace period api.Server.Run already gives its own HTTP
// shutdown — giving the cognition worker a chance to run its own cleanup
// (e.g. dispatcher.py's run_forever DBOS.destroy() in its finally block)
// rather than always being killed outright.
func (w *Worker) Run(ctx context.Context) error {
	if w.Command == "" {
		<-ctx.Done()
		return nil
	}

	fields := strings.Fields(w.Command)
	if len(fields) == 0 {
		return fmt.Errorf("cognition: empty command after parsing %q", w.Command)
	}

	cmd := exec.CommandContext(ctx, fields[0], fields[1:]...)
	cmd.Dir = w.Dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second

	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cognition: worker process exited: %w", err)
	}
	return fmt.Errorf("cognition: worker process %q exited with status 0 while the daemon was still running — a worker is expected to run forever", w.Command)
}
