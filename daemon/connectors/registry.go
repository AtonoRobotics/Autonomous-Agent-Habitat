// Package connectors resolves a device_action's connector row into a live
// Actuator, so the daemon's actuation API (daemon/api) can serve requests
// without the caller knowing anything about SSH, host keys, or private
// key material — that all lives in connector.config, per
// docs/AMH-SPECIFICATION.md Artifact C (`Connector{type, auth, config}`).
package connectors

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"

	sshconn "github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors/ssh"
)

// SSHConnectorConfig is the shape of connector.config for type='ssh'.
//
// PrivateKeyPath, not the key material itself, is what's stored — secrets
// live on disk (or a real secret manager, post-V0), never inline in the
// SQLite row. HostKeyAuthorizedKey pins the device's host key in
// authorized_keys format (e.g. "ssh-rsa AAAA..."); InsecureSkipHostKeyVerify
// exists only for test fixtures (daemon/cmd/amh-fake-device) and is
// refused unless explicitly set — see resolveHostKeyCallback.
type SSHConnectorConfig struct {
	Host                      string `json:"host"`
	Port                      int    `json:"port"`
	User                      string `json:"user"`
	PrivateKeyPath            string `json:"private_key_path"`
	HostKeyAuthorizedKey      string `json:"host_key_authorized_key,omitempty"`
	InsecureSkipHostKeyVerify bool   `json:"insecure_skip_host_key_verify,omitempty"`
}

var ErrUnsupportedConnectorType = errors.New("connectors: unsupported connector type")

// Registry resolves device_action -> device -> connector -> live Actuator
// on every call. It deliberately does not cache SSH connections: each
// actuation already opens its own session in the actuation kernel
// (one-session-per-actuation is the existing model, unchanged by moving
// this server-side), so there is no long-lived connection state to manage
// or leak.
type Registry struct {
	DB *sql.DB
}

func NewRegistry(db *sql.DB) *Registry {
	return &Registry{DB: db}
}

// ResolveActuator looks up the connector for the device that owns
// deviceActionID and builds a live Actuator for it.
func (r *Registry) ResolveActuator(ctx context.Context, deviceActionID string) (*sshconn.Connector, error) {
	var connectorType, configJSON string
	err := r.DB.QueryRowContext(ctx, `
		SELECT c.type, c.config
		FROM device_action da
		JOIN device d ON d.id = da.device_id
		JOIN connector c ON c.id = d.connector_id
		WHERE da.id = ?`, deviceActionID,
	).Scan(&connectorType, &configJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("connectors: no connector found for device_action %s", deviceActionID)
	}
	if err != nil {
		return nil, fmt.Errorf("connectors: resolve %s: %w", deviceActionID, err)
	}

	switch connectorType {
	case "ssh":
		return buildSSHConnector(configJSON)
	default:
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedConnectorType, connectorType)
	}
}

func buildSSHConnector(configJSON string) (*sshconn.Connector, error) {
	var cfg SSHConnectorConfig
	if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
		return nil, fmt.Errorf("connectors: parse ssh config: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("connectors: ssh config missing host")
	}

	signer, err := loadSigner(cfg.PrivateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("connectors: load private key: %w", err)
	}

	hostKeyCB, err := resolveHostKeyCallback(cfg)
	if err != nil {
		return nil, err
	}

	return sshconn.New(sshconn.Config{
		Host:      cfg.Host,
		Port:      cfg.Port,
		User:      cfg.User,
		Signer:    signer,
		HostKeyCB: hostKeyCB,
	})
}

func loadSigner(privateKeyPath string) (ssh.Signer, error) {
	if privateKeyPath == "" {
		return nil, fmt.Errorf("private_key_path is required")
	}
	keyBytes, err := os.ReadFile(privateKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", privateKeyPath, err)
	}
	return ssh.ParsePrivateKey(keyBytes)
}

func resolveHostKeyCallback(cfg SSHConnectorConfig) (ssh.HostKeyCallback, error) {
	if cfg.HostKeyAuthorizedKey != "" {
		pubKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(cfg.HostKeyAuthorizedKey))
		if err != nil {
			return nil, fmt.Errorf("connectors: parse host_key_authorized_key: %w", err)
		}
		return ssh.FixedHostKey(pubKey), nil
	}
	if cfg.InsecureSkipHostKeyVerify {
		return ssh.InsecureIgnoreHostKey(), nil
	}
	return nil, fmt.Errorf("connectors: connector.config has neither host_key_authorized_key nor insecure_skip_host_key_verify — refusing to connect without host key verification")
}
