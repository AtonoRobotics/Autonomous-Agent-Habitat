// Command amh-actuate is a thin CLI wrapping daemon/actuation.Execute for
// one device action, over SSH. It exists as the V0 bridge that lets the
// Python agent layer (which has no direct access to the daemon's
// in-process connector registry) trigger a real device actuation as a
// DBOS step — a subprocess call, not a network RPC.
//
// This is a deliberately minimal stand-in for the daemon<->agent bridge
// Artifact A names as contracts/proto (gRPC). A CLI is enough to prove the
// V0 walking-skeleton scenario end-to-end (Artifact H, steps 1-4) across
// real process boundaries; a persistent RPC service replacing per-call
// process spawn is a follow-up task once the daemon needs to serve many
// concurrent agent-layer requests rather than one-off V0 demonstrations.
//
// SECURITY: --insecure-skip-host-key-verify exists only for the local
// fake-device test fixture (daemon/cmd/amh-fake-device) and must never be
// set against a real device — it disables the host-key check the ssh
// connector otherwise requires unconditionally.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/actuation"
	sshconn "github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/connectors/ssh"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/interlocks"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
)

type output struct {
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

func main() {
	dbPath := flag.String("db", "", "path to the shared AMH SQLite database")
	migrationsDir := flag.String("migrations", "./store/migrations", "path to store/migrations")
	deviceActionID := flag.String("device-action-id", "", "device_action.id to execute")
	host := flag.String("host", "127.0.0.1", "SSH device host")
	port := flag.Int("port", 22, "SSH device port")
	user := flag.String("user", "amh", "SSH user")
	forward := flag.String("forward", "", "forward shell command")
	readState := flag.String("read-state", "", "read-state shell command (required for verified-reversible actions)")
	ticketID := flag.String("ticket-id", "", "approval_gate ticket ID (required unless the action is verified-reversible or has an approved SafetyCase)")
	insecure := flag.Bool("insecure-skip-host-key-verify", false, "TEST FIXTURE ONLY: skip host key verification")
	flag.Parse()

	if err := run(*dbPath, *migrationsDir, *deviceActionID, *host, *port, *user, *forward, *readState, *ticketID, *insecure); err != nil {
		json.NewEncoder(os.Stdout).Encode(output{Error: err.Error()})
		os.Exit(1)
	}
}

func run(dbPath, migrationsDir, deviceActionID, host string, port int, user, forward, readState, ticketID string, insecure bool) error {
	if dbPath == "" || deviceActionID == "" || forward == "" {
		return fmt.Errorf("--db, --device-action-id, and --forward are required")
	}

	db, err := store.Open(dbPath, migrationsDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer db.Close()

	signer, err := ephemeralSigner()
	if err != nil {
		return fmt.Errorf("generate ephemeral key: %w", err)
	}

	if !insecure {
		return fmt.Errorf("host key pinning is required outside test fixtures; pass --insecure-skip-host-key-verify only against amh-fake-device")
	}
	hostKeyCB := ssh.InsecureIgnoreHostKey()

	conn, err := sshconn.New(sshconn.Config{
		Host:      host,
		Port:      port,
		User:      user,
		Signer:    signer,
		HostKeyCB: hostKeyCB,
	})
	if err != nil {
		return fmt.Errorf("new ssh connector: %w", err)
	}

	gate := interlocks.New(db)
	var ticket *interlocks.Ticket
	if ticketID != "" {
		ticket = &interlocks.Ticket{ID: ticketID}
	}

	ctx := context.Background()
	result, err := actuation.Execute(ctx, db, conn, gate, deviceActionID, actuation.Command{
		Forward:   forward,
		ReadState: readState,
	}, ticket)
	if err != nil {
		return err
	}

	return json.NewEncoder(os.Stdout).Encode(output{Result: result})
}

func ephemeralSigner() (ssh.Signer, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	return ssh.NewSignerFromKey(key)
}
