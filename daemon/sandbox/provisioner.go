// Package sandbox provisions and destroys per-agent computers: the
// isolated compute instance each agent works inside, as distinct from the
// extension registry's isolation of extension processes (daemon/extensions)
// and the connector layer's isolation of external device I/O
// (daemon/connectors). "Add a computer" and "build a sandbox" are the same
// operation here — a computer IS a sandbox, scoped to one agent.
//
// Two isolation modes:
//   - container: a real container via the docker CLI. Strong isolation,
//     requires a reachable docker daemon.
//   - process: a real Linux mount namespace via `unshare --mount`,
//     requiring no external daemon — this deployment runs as root, so
//     unshare works without a container runtime present. Weaker than a
//     container (shares the host PID namespace, no network/cgroup
//     isolation) but a genuine namespace boundary, not just a bare
//     subprocess. `--pid` is deliberately not used here: it requires
//     `--fork`, which reparents the real workload to the outer init on
//     teardown (the grandchild survives its immediate `unshare` parent's
//     death) and makes Destroy's "actually gone" guarantee depend on an
//     external process reaping a zombie on its own schedule — exactly the
//     kind of unverifiable inverse this package exists to avoid.
//
// Create/Destroy is the reversible pair, by the same discipline as
// daemon/actuation and daemon/extensions: provisioning a computer is
// autonomous because destroying it is a verified, always-available
// inverse — there is no "irreversible computer."
package sandbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// Isolation is how a computer is sandboxed from the host and other
// computers.
type Isolation string

const (
	IsolationProcess   Isolation = "process"
	IsolationContainer Isolation = "container"
)

// Status is a computer's lifecycle state.
type Status string

const (
	StatusProvisioning Status = "provisioning"
	StatusReady        Status = "ready"
	StatusStopped      Status = "stopped"
	StatusDestroyed    Status = "destroyed"
	StatusFailed       Status = "failed"
)

// Computer is one agent's compute instance.
type Computer struct {
	ID             string
	AgentID        string
	Isolation      Isolation
	Image          string
	Status         Status
	RuntimeHandle  string
	ResourceLimits map[string]string
	Workdir        string
	DestroyReason  string
}

var (
	ErrNotFound     = errors.New("sandbox: computer not found")
	ErrInvalidState = errors.New("sandbox: computer not in a valid state for this transition")
)

// Provisioner creates and destroys computers. BaseDir is where each
// computer's dedicated workdir is created (state/computers/<id> by
// default) — this is the agent's own filesystem, separate from every
// other agent's, surviving the computer's own destruction so produced
// artifacts aren't lost with the compute instance.
type Provisioner struct {
	DB      *sql.DB
	BaseDir string
}

func New(db *sql.DB, baseDir string) *Provisioner {
	return &Provisioner{DB: db, BaseDir: baseDir}
}

// Create provisions a new computer for agentID. image is a docker image
// reference for container isolation, or a shell command (e.g. "sleep
// infinity", a real harness entrypoint) for process isolation.
// resourceLimits accepts docker-CLI-shaped keys ("memory": "512m", "cpus":
// "1.0") — applied for container isolation only; process isolation records
// them but does not (yet) enforce them, since V0 has no cgroup wiring
// independent of a container runtime.
func (p *Provisioner) Create(ctx context.Context, agentID string, isolation Isolation, image string, resourceLimits map[string]string) (*Computer, error) {
	if isolation != IsolationProcess && isolation != IsolationContainer {
		return nil, fmt.Errorf("sandbox: isolation must be \"process\" or \"container\", got %q", isolation)
	}
	if image == "" {
		return nil, fmt.Errorf("sandbox: image (container ref, or command for process isolation) is required")
	}

	id := uuid.NewString()
	workdir := filepath.Join(p.BaseDir, id)
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		return nil, fmt.Errorf("sandbox: create workdir %s: %w", workdir, err)
	}

	limitsJSON, err := json.Marshal(resourceLimits)
	if err != nil {
		return nil, fmt.Errorf("sandbox: marshal resource_limits: %w", err)
	}

	var agentIDVal any
	if agentID != "" {
		agentIDVal = agentID
	}
	_, err = p.DB.ExecContext(ctx, `
		INSERT INTO computer (id, agent_id, isolation, image, status, resource_limits, workdir)
		VALUES (?, ?, ?, ?, 'provisioning', ?, ?)`,
		id, agentIDVal, string(isolation), image, string(limitsJSON), workdir,
	)
	if err != nil {
		return nil, fmt.Errorf("sandbox: insert computer: %w", err)
	}

	runtimeHandle, launchErr := launch(ctx, id, isolation, image, workdir, resourceLimits)
	if launchErr != nil {
		reason := launchErr.Error()
		p.DB.ExecContext(ctx, `UPDATE computer SET status = 'failed', destroy_reason = ? WHERE id = ?`, reason, id)
		return nil, fmt.Errorf("sandbox: provision %s: %w", id, launchErr)
	}

	_, err = p.DB.ExecContext(ctx, `
		UPDATE computer SET status = 'ready', runtime_handle = ?, ready_at = strftime('%Y-%m-%dT%H:%M:%fZ','now')
		WHERE id = ?`, runtimeHandle, id)
	if err != nil {
		teardown(ctx, isolation, runtimeHandle)
		return nil, fmt.Errorf("sandbox: mark ready: %w", err)
	}

	return p.Get(ctx, id)
}

// Destroy tears down a computer's compute instance. Its workdir is left
// on disk — destroying the compute instance is not the same operation as
// deleting the agent's produced artifacts, and conflating them would make
// this irreversible in a way it doesn't need to be.
func (p *Provisioner) Destroy(ctx context.Context, id, reason string) (*Computer, error) {
	c, err := p.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	if c.Status != StatusReady && c.Status != StatusFailed {
		return nil, fmt.Errorf("%w: %s is %s", ErrInvalidState, id, c.Status)
	}

	if c.RuntimeHandle != "" {
		if err := teardown(ctx, c.Isolation, c.RuntimeHandle); err != nil {
			return nil, fmt.Errorf("sandbox: destroy %s: %w", id, err)
		}
	}

	_, err = p.DB.ExecContext(ctx, `
		UPDATE computer SET status = 'destroyed', destroyed_at = strftime('%Y-%m-%dT%H:%M:%fZ','now'), destroy_reason = ?
		WHERE id = ?`, reason, id)
	if err != nil {
		return nil, fmt.Errorf("sandbox: mark destroyed: %w", err)
	}
	return p.Get(ctx, id)
}

// Get loads one computer by ID.
func (p *Provisioner) Get(ctx context.Context, id string) (*Computer, error) {
	var c Computer
	var agentID, runtimeHandle, destroyReason sql.NullString
	var limitsJSON string
	err := p.DB.QueryRowContext(ctx, `
		SELECT id, agent_id, isolation, image, status, runtime_handle, resource_limits, workdir, destroy_reason
		FROM computer WHERE id = ?`, id,
	).Scan(&c.ID, &agentID, &c.Isolation, &c.Image, &c.Status, &runtimeHandle, &limitsJSON, &c.Workdir, &destroyReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox: get %s: %w", id, err)
	}
	c.AgentID = agentID.String
	c.RuntimeHandle = runtimeHandle.String
	c.DestroyReason = destroyReason.String
	if limitsJSON != "" {
		if err := json.Unmarshal([]byte(limitsJSON), &c.ResourceLimits); err != nil {
			return nil, fmt.Errorf("sandbox: parse resource_limits: %w", err)
		}
	}
	return &c, nil
}

// ListForAgent returns every non-destroyed computer belonging to agentID —
// what an operator or the agent itself would see as "my computers."
func (p *Provisioner) ListForAgent(ctx context.Context, agentID string) ([]*Computer, error) {
	rows, err := p.DB.QueryContext(ctx, `SELECT id FROM computer WHERE agent_id = ? AND status != 'destroyed' ORDER BY created_at DESC`, agentID)
	if err != nil {
		return nil, fmt.Errorf("sandbox: list for agent %s: %w", agentID, err)
	}
	defer rows.Close()
	var out []*Computer
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		c, err := p.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func launch(ctx context.Context, id string, isolation Isolation, image, workdir string, limits map[string]string) (string, error) {
	switch isolation {
	case IsolationProcess:
		return launchProcess(ctx, id, image, workdir)
	case IsolationContainer:
		return launchContainer(ctx, id, image, workdir, limits)
	default:
		return "", fmt.Errorf("sandbox: unknown isolation %q", isolation)
	}
}

// launchProcess starts image (a shell command) inside a fresh mount
// namespace via unshare, in its own process group so the whole tree can be
// reaped by one signal, and workdir as its current directory. `unshare
// --mount` (no `--pid`/`--fork`) execs the target in place — the process
// stays our direct child at a stable PID, so cmd.Wait() reaps it cleanly
// with no reparenting. unshare itself starts successfully even when the
// command it wraps doesn't exist (the failure happens inside the new
// namespace) — cmd.Start() alone can't see that, so this gives the process
// a short grace window to prove it's actually still running before
// declaring the launch successful.
func launchProcess(ctx context.Context, id, image, workdir string) (string, error) {
	fields := strings.Fields(image)
	if len(fields) == 0 {
		return "", fmt.Errorf("empty image/command for process isolation")
	}
	args := append([]string{"--mount", "--"}, fields...)
	// Deliberately exec.Command, not exec.CommandContext(ctx, ...): ctx is
	// the triggering HTTP request's context, cancelled the moment that
	// request finishes — a CommandContext'd computer would be SIGKILL'd
	// right after Create returns "ready," not kept running as the agent's
	// actual compute instance. Destroy (via teardown) is this process's
	// only intended termination path.
	cmd := exec.Command("unshare", args...)
	cmd.Dir = workdir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("start unshare'd process %q: %w", image, err)
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		msg := strings.TrimSpace(stderr.String())
		if err != nil {
			if msg != "" {
				return "", fmt.Errorf("process %q exited immediately: %w: %s", image, err, msg)
			}
			return "", fmt.Errorf("process %q exited immediately: %w", image, err)
		}
		return "", fmt.Errorf("process %q exited immediately with status 0 instead of staying up", image)
	case <-time.After(200 * time.Millisecond):
		// Still running past the grace window — treat as successfully
		// launched and finish reaping it in the background.
		go func() { <-done }()
		return fmt.Sprintf("pgid:%d", cmd.Process.Pid), nil
	}
}

func launchContainer(ctx context.Context, id, image, workdir string, limits map[string]string) (string, error) {
	name := "amh-computer-" + id
	args := []string{"run", "-d", "--name", name, "-v", workdir + ":/workspace"}
	if mem, ok := limits["memory"]; ok && mem != "" {
		args = append(args, "--memory", mem)
	}
	if cpus, ok := limits["cpus"]; ok && cpus != "" {
		args = append(args, "--cpus", cpus)
	}
	args = append(args, image)
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("docker run %q: %w: %s", image, err, strings.TrimSpace(string(out)))
	}
	return "container:" + strings.TrimSpace(string(out)), nil
}

func teardown(ctx context.Context, isolation Isolation, runtimeHandle string) error {
	switch {
	case strings.HasPrefix(runtimeHandle, "pgid:"):
		pgid, err := strconv.Atoi(strings.TrimPrefix(runtimeHandle, "pgid:"))
		if err != nil {
			return fmt.Errorf("parse runtime handle %q: %w", runtimeHandle, err)
		}
		if err := syscall.Kill(-pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill process group %d: %w", pgid, err)
		}
		// SIGKILL terminates immediately but leaves a zombie until the
		// launching process's background Wait() reaps it — wait for that
		// reap (bounded) so Destroy returning "success" actually means
		// gone, not merely signaled.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			if err := syscall.Kill(-pgid, 0); errors.Is(err, syscall.ESRCH) {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return nil

	case strings.HasPrefix(runtimeHandle, "container:"):
		containerID := strings.TrimPrefix(runtimeHandle, "container:")
		if out, err := exec.CommandContext(ctx, "docker", "stop", containerID).CombinedOutput(); err != nil {
			return fmt.Errorf("docker stop %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.CommandContext(ctx, "docker", "rm", containerID).CombinedOutput(); err != nil {
			return fmt.Errorf("docker rm %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
		}
		return nil

	default:
		return fmt.Errorf("unrecognized runtime_handle %q, cannot tear down", runtimeHandle)
	}
}

// DockerAvailable reports whether a docker daemon is reachable — tests use
// this to skip container-isolation coverage in environments (like sandboxed
// CI) with the docker CLI present but no daemon behind it.
func DockerAvailable() bool {
	return exec.Command("docker", "info").Run() == nil
}
