// Command amh-actuate is a thin CLI wrapping daemon/actuation.Execute for
// one device action, over SSH — a subprocess call, not a network RPC.
// The daemon's persistent HTTP API (daemon/api, wired in
// agents/workflows/actuate.py) is the actual daemon<->agent bridge now;
// this CLI predates that and is kept as a standalone ops tool for driving
// one actuation directly (see daemon/cmd/amh-daemon for the supervised
// long-running path). Artifact A names contracts/proto (gRPC) as the
// formal bridge contract; neither this CLI nor the HTTP API implements
// that transport.
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
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/observability"
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
	params := flag.String("params", "{}", `JSON object of named parameter values substituted into the device_action's own forward_template/read_state_template — e.g. '{"open_pct":"60"}'. Never raw shell text: the daemon renders the actual command server-side from the device_action's stored template.`)
	ticketID := flag.String("ticket-id", "", "approval_gate ticket ID (required unless the action is verified-reversible or has an approved SafetyCase)")
	insecure := flag.Bool("insecure-skip-host-key-verify", false, "TEST FIXTURE ONLY: skip host key verification")
	flag.Parse()

	if err := run(*dbPath, *migrationsDir, *deviceActionID, *host, *port, *user, *params, *ticketID, *insecure); err != nil {
		json.NewEncoder(os.Stdout).Encode(output{Error: err.Error()})
		os.Exit(1)
	}
}

func run(dbPath, migrationsDir, deviceActionID, host string, port int, user, paramsJSON, ticketID string, insecure bool) error {
	if dbPath == "" || deviceActionID == "" {
		return fmt.Errorf("--db and --device-action-id are required")
	}
	var params map[string]string
	if err := json.Unmarshal([]byte(paramsJSON), &params); err != nil {
		return fmt.Errorf("--params is not a valid JSON object: %w", err)
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

	// No exporter configured: this one-shot CLI process records a span but
	// has nowhere to ship it — a real daemon process would pass a
	// provider wired to OTEL_EXPORTER_OTLP_ENDPOINT (see .env.example).
	// ExecuteTraced is still exercised for real here, not just in tests.
	tp := observability.Init(nil)
	defer tp.Shutdown(context.Background())

	ctx := context.Background()
	result, err := actuation.ExecuteTraced(ctx, tp, db, conn, gate, deviceActionID, actuation.Command{
		Params: params,
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
