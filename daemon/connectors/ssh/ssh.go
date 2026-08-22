// Package ssh implements the SSH device connector: golang.org/x/crypto/ssh
// for device control, per docs/AMH-SPECIFICATION.md §1 and §12.
//
// A device action is a shell command template rendered with the action's
// arguments and run over one SSH session; state reads are a second shell
// command whose stdout is parsed as the state value. This is
// intentionally generic — the greenhouse-vent example in
// contracts/manifests/connector.manifest.yaml runs `vent-ctl` commands,
// but any device exposing a CLI over SSH fits the same shape.
package ssh

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config describes how to reach one SSH-controlled device host.
type Config struct {
	Host           string
	Port           int
	User           string
	Signer         ssh.Signer // private key auth; the only auth method this connector implements
	HostKeyCB      ssh.HostKeyCallback
	DialTimeout    time.Duration
	CommandTimeout time.Duration
}

// Connector is a single SSH-reachable device host. It implements the
// generic Actuator shape consumed by the daemon/actuation package: Invoke
// runs a rendered command and returns its stdout; ReadState runs a
// separate read-only command and returns its stdout as the current value.
type Connector struct {
	cfg Config
}

func New(cfg Config) (*Connector, error) {
	if cfg.Host == "" {
		return nil, fmt.Errorf("ssh: Host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.User == "" {
		cfg.User = "amh"
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("ssh: Signer is required (key-based auth only)")
	}
	if cfg.HostKeyCB == nil {
		return nil, fmt.Errorf("ssh: HostKeyCB is required — refusing InsecureIgnoreHostKey by default")
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.CommandTimeout == 0 {
		cfg.CommandTimeout = 30 * time.Second
	}
	return &Connector{cfg: cfg}, nil
}

// RunShell executes one shell command over a fresh SSH session and returns
// trimmed stdout. Each call opens its own session (no long-lived shell
// state), matching the stateless-per-actuation model in Artifact F.
func (c *Connector) RunShell(ctx context.Context, command string) (string, error) {
	clientCfg := &ssh.ClientConfig{
		User:            c.cfg.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(c.cfg.Signer)},
		HostKeyCallback: c.cfg.HostKeyCB,
		Timeout:         c.cfg.DialTimeout,
	}

	addr := fmt.Sprintf("%s:%d", c.cfg.Host, c.cfg.Port)
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.DialTimeout)
	defer cancel()

	client, err := dialContext(dialCtx, addr, clientCfg)
	if err != nil {
		return "", fmt.Errorf("ssh: dial %s: %w", addr, err)
	}
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return "", fmt.Errorf("ssh: new session: %w", err)
	}
	defer session.Close()

	var stdout, stderr bytes.Buffer
	session.Stdout = &stdout
	session.Stderr = &stderr

	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case err := <-done:
		if err != nil {
			return "", fmt.Errorf("ssh: command %q failed: %w (stderr: %s)", command, err, stderr.String())
		}
		return strings.TrimSpace(stdout.String()), nil
	}
}

// dialContext wraps ssh.Dial with context cancellation, since the stdlib
// ssh package has no native context-aware dial.
func dialContext(ctx context.Context, addr string, cfg *ssh.ClientConfig) (*ssh.Client, error) {
	type result struct {
		client *ssh.Client
		err    error
	}
	ch := make(chan result, 1)
	go func() {
		client, err := ssh.Dial("tcp", addr, cfg)
		ch <- result{client, err}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case r := <-ch:
		return r.client, r.err
	}
}
