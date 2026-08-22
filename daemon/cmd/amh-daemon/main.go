// Command amh-daemon is the AMH Go control-plane process: supervisor tree,
// scheduler, health/watchdog, extension lifecycle, and the HTTP/MCP
// control-plane surfaces. Runs as a systemd service (Linux) or Windows
// Service. See docs/AMH-SPECIFICATION.md §1 and Artifact A.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/a2a"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/api"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/credentials"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/health"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/mcp"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/observability"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/operations"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/policy"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/scheduler"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/supervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbURL := getenv("DATABASE_URL", "postgresql://postgres:postgres@127.0.0.1:5432/amh")
	migrationsDir := getenv("AMH_MIGRATIONS_DIR", "./store/migrations")
	host := getenv("AMH_DAEMON_HOST", "127.0.0.1")
	port := getenv("AMH_DAEMON_PORT", "8080")
	apiPort := getenv("AMH_API_PORT", "8090")

	// -rollback-migration is a maintenance operation, not a runtime one:
	// it rolls back the N most recently applied migrations (store.Rollback,
	// one at a time, most-recent-first) against DATABASE_URL/
	// AMH_MIGRATIONS_DIR, then exits without starting the supervisor tree
	// or touching a live schema an already-running daemon still depends
	// on. Run it against a stopped daemon.
	rollbackSteps := flag.Int("rollback-migration", 0, "roll back this many of the most recently applied migrations, then exit, without starting the daemon")
	flag.Parse()
	if *rollbackSteps > 0 {
		for i := 0; i < *rollbackSteps; i++ {
			name, err := store.Rollback(dbURL, migrationsDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "rollback step %d/%d failed: %v\n", i+1, *rollbackSteps, err)
				os.Exit(1)
			}
			fmt.Fprintf(os.Stdout, "rolled back %s\n", name)
		}
		return
	}

	// Every process-isolation extension the registry launches
	// (daemon/extensions) inherits this daemon's environment via plain
	// exec.Command — see that package's doc comment on why it is
	// deliberately not exec.CommandContext. Setting AMH_API_BASE_URL here,
	// once, is what lets ANY such extension (not just the control-plane
	// UI) call back into its own admin API without each one needing a
	// separate, extension-specific way to discover the daemon's address.
	if os.Getenv("AMH_API_BASE_URL") == "" {
		os.Setenv("AMH_API_BASE_URL", "http://"+host+":"+apiPort)
	}

	tickMs, err := strconv.Atoi(getenv("HABITAT_ROUTINE_TICK_MS", "60000"))
	if err != nil {
		log.Error("invalid HABITAT_ROUTINE_TICK_MS", "error", err)
		os.Exit(1)
	}

	db, err := store.Open(dbURL, migrationsDir)
	if err != nil {
		log.Error("failed to open store", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("store ready", "migrations", migrationsDir)

	// Acceptance invariant #2 (§15): a crash between DISPATCHED and
	// OBSERVED must not silently lose the effect — it enters
	// reconciliation and can remain OUTCOME_UNKNOWN. Run this before
	// anything else touches operations, so restart is what actually
	// makes that true, not merely what the code claims. See
	// daemon/operations.ReconcileInterrupted's doc comment.
	if reconciled, err := operations.New(db, policy.New(db)).ReconcileInterrupted(context.Background()); err != nil {
		log.Error("failed to reconcile interrupted effects on startup", "error", err)
		os.Exit(1)
	} else if len(reconciled) > 0 {
		log.Warn("marked interrupted effects outcome_unknown on startup", "count", len(reconciled))
	}

	sched := scheduler.New(time.Duration(tickMs)*time.Millisecond, log)
	sched.AddRoutine(func(ctx context.Context, tick time.Time) {
		log.Debug("scheduler tick", "at", tick)
	})

	healthSrv := health.New(host+":"+port, db, log)

	// No exporter wired yet: spans are recorded but not shipped anywhere
	// until an OTLP exporter is added (OTEL_EXPORTER_OTLP_ENDPOINT is
	// already reserved for this in .env.example). Recording without
	// exporting is the correct default until that's configured — see
	// daemon/observability's doc.
	tp := observability.Init(nil)
	defer tp.Shutdown(context.Background())

	// Fail closed: the API server enforces two distinct role tokens (§12),
	// and there is no unauthenticated fallback mode. If either token is
	// unset, refuse to start rather than run the control-plane API open to
	// anyone who can reach the port. See daemon/authn's doc comment and
	// .env.example for how to configure these.
	agentToken := os.Getenv("AMH_API_AGENT_TOKEN")
	operatorToken := os.Getenv("AMH_API_OPERATOR_TOKEN")
	auth, err := authn.New(agentToken, operatorToken)
	if err != nil {
		log.Error("refusing to start: API auth is not configured", "error", err,
			"hint", "set AMH_API_AGENT_TOKEN and AMH_API_OPERATOR_TOKEN to two distinct secrets")
		os.Exit(1)
	}

	// AMH_CREDENTIAL_KEY gates only the account/credential admin routes
	// (daemon/api/controlplane.go's credentialsUnavailable), not daemon
	// startup — unlike the two RBAC tokens above, a habitat with no
	// external accounts to authenticate yet has no reason to refuse to
	// run. See daemon/credentials's doc comment for how to generate one.
	var creds *credentials.Store
	if key, err := credentials.LoadKeyFromEnv(); err == nil {
		creds, err = credentials.New(db, key)
		if err != nil {
			log.Error("refusing to enable credential store: invalid AMH_CREDENTIAL_KEY", "error", err)
			os.Exit(1)
		}
	} else {
		log.Warn("AMH_CREDENTIAL_KEY not set: account/credential control-plane routes are disabled", "hint", "generate one with: openssl rand -base64 32")
	}

	sandboxBaseDir := getenv("AMH_SANDBOX_DIR", "./state/computers")

	// Unsigned manifests are admitted by default (see
	// daemon/extensions.Registry.RequireSignatures's doc comment) — set
	// this to require every Discover to carry a signature that verifies
	// against a key already registered via
	// POST /v1/extensions/trusted-keys.
	requireSignatures := getenv("AMH_EXTENSIONS_REQUIRE_SIGNATURES", "false") == "true"
	apiSrv := api.New(host+":"+apiPort, db, dbURL, tp, auth, log, sandboxBaseDir, creds, requireSignatures)

	mcpPort := getenv("AMH_MCP_PORT", "8093")
	mcpSrv := mcp.New(host+":"+mcpPort, db, tp, auth, log)

	a2aPort := getenv("AMH_A2A_PORT", "8094")
	a2aPublicURL := getenv("AMH_A2A_PUBLIC_URL", "http://"+host+":"+a2aPort)
	a2aSrv := a2a.New(host+":"+a2aPort, a2aPublicURL, a2a.NewStore(db), tp, auth, log)

	sup := supervisor.New("amh-daemon", supervisor.OneForOne, 5, time.Minute, log)
	sup.Add(supervisor.Child{Name: "scheduler", Run: sched.Run})
	sup.Add(supervisor.Child{Name: "health", Run: healthSrv.Run})
	sup.Add(supervisor.Child{Name: "api", Run: apiSrv.Run})
	sup.Add(supervisor.Child{Name: "mcp", Run: mcpSrv.Run})
	sup.Add(supervisor.Child{Name: "a2a", Run: a2aSrv.Run})

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	log.Info("amh-daemon starting")
	if err := sup.Run(ctx); err != nil {
		log.Error("supervisor exited with error", "error", err)
		os.Exit(1)
	}
	log.Info("amh-daemon stopped")
}

func getenv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}
