package extensions

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// launcher starts and stops one extension instance according to its
// declared isolation. It is the only place that touches an OS process or
// the docker CLI directly — Registry never does.
type launcher struct {
	mu        sync.Mutex
	processes map[string]*exec.Cmd // runtime key -> live process, isolation=process only
}

func newLauncher() *launcher {
	return &launcher{processes: make(map[string]*exec.Cmd)}
}

// runtimeKey identifies one launched instance for later teardown.
func runtimeKey(id, version string) string {
	return id + "@" + version
}

// launch starts the extension per its isolation and returns an opaque
// runtime handle recorded on the extension row (PID for process, container
// ID for container, a fixed marker for in_process). wasm has no runtime in
// this environment — Activate refuses it explicitly rather than pretending
// to launch it (see Registry.Activate).
func (l *launcher) launch(ctx context.Context, id, version string, spec Spec) (string, error) {
	switch spec.Isolation {
	case IsolationInProcess:
		return "in_process:" + runtimeKey(id, version), nil

	case IsolationProcess:
		fields := strings.Fields(spec.Entrypoint)
		if len(fields) == 0 {
			return "", fmt.Errorf("extensions: empty entrypoint for process isolation")
		}
		// Deliberately exec.Command, not exec.CommandContext(ctx, ...):
		// ctx here is the triggering HTTP request's context, which is
		// cancelled the moment that request finishes — an
		// exec.CommandContext'd process would be SIGKILL'd right after
		// Activate returns "success," not kept running as the extension's
		// actual long-lived instance. teardown() (via Registry.Dispose)
		// is this process's only intended termination path.
		cmd := exec.Command(fields[0], fields[1:]...)
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			return "", fmt.Errorf("extensions: start process entrypoint %q: %w", spec.Entrypoint, err)
		}

		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()

		select {
		case err := <-done:
			msg := strings.TrimSpace(stderr.String())
			if err != nil {
				if msg != "" {
					return "", fmt.Errorf("extensions: process %q exited immediately: %w: %s", spec.Entrypoint, err, msg)
				}
				return "", fmt.Errorf("extensions: process %q exited immediately: %w", spec.Entrypoint, err)
			}
			return "", fmt.Errorf("extensions: process %q exited immediately with status 0 instead of staying up", spec.Entrypoint)
		case <-time.After(200 * time.Millisecond):
			l.mu.Lock()
			l.processes[runtimeKey(id, version)] = cmd
			l.mu.Unlock()
			go func() { <-done }() // finish reaping once torn down
			return fmt.Sprintf("pid:%d", cmd.Process.Pid), nil
		}

	case IsolationContainer:
		name := "amh-ext-" + sanitizeContainerName(runtimeKey(id, version))
		out, err := exec.CommandContext(ctx, "docker", "run", "-d", "--name", name, spec.Entrypoint).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("extensions: docker run %q: %w: %s", spec.Entrypoint, err, strings.TrimSpace(string(out)))
		}
		containerID := strings.TrimSpace(string(out))
		return "container:" + containerID, nil

	case IsolationWasm:
		return "", fmt.Errorf("extensions: isolation \"wasm\" has no runtime in this deployment yet — refusing to silently no-op an activation")

	default:
		return "", fmt.Errorf("extensions: unknown isolation %q", spec.Isolation)
	}
}

// teardown stops whatever launch started, using the recorded runtime
// handle (not in-memory state alone) — this must work even after a
// daemon restart, since extension state is durable in SQLite but launcher
// process handles are not.
func (l *launcher) teardown(ctx context.Context, id, version, runtimeHandle string) error {
	switch {
	case strings.HasPrefix(runtimeHandle, "in_process:"):
		return nil

	case strings.HasPrefix(runtimeHandle, "pid:"):
		key := runtimeKey(id, version)
		l.mu.Lock()
		cmd, ok := l.processes[key]
		delete(l.processes, key)
		l.mu.Unlock()
		if ok && cmd.Process != nil {
			if err := cmd.Process.Kill(); err != nil && !strings.Contains(err.Error(), "process already finished") {
				return fmt.Errorf("extensions: kill process %s: %w", runtimeHandle, err)
			}
			cmd.Wait()
			return nil
		}
		return fmt.Errorf("extensions: no live process handle for %s (runtime_handle=%s) — likely a daemon restart; process may need manual cleanup", key, runtimeHandle)

	case strings.HasPrefix(runtimeHandle, "container:"):
		containerID := strings.TrimPrefix(runtimeHandle, "container:")
		if out, err := exec.CommandContext(ctx, "docker", "stop", containerID).CombinedOutput(); err != nil {
			return fmt.Errorf("extensions: docker stop %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.CommandContext(ctx, "docker", "rm", containerID).CombinedOutput(); err != nil {
			return fmt.Errorf("extensions: docker rm %s: %w: %s", containerID, err, strings.TrimSpace(string(out)))
		}
		return nil

	default:
		return fmt.Errorf("extensions: unrecognized runtime_handle %q, cannot tear down", runtimeHandle)
	}
}

func sanitizeContainerName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
