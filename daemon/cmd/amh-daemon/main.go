// Command amh-daemon is the AMH Go control-plane process: supervisor tree,
// scheduler, health/watchdog, connector runtime, device I/O. Runs as a
// systemd service (Linux) or Windows Service. See
// docs/AMH-SPECIFICATION.md §1 and Artifact A.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/api"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/authn"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/health"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/observability"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/scheduler"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/store"
	"github.com/AtonoRobotics/Autonomous-Agent-Habitat/daemon/supervisor"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dbURL := getenv("DATABASE_URL", "sqlite:./state/amh.db")
	dbPath := dbURL
	const sqlitePrefix = "sqlite:"
	if len(dbURL) > len(sqlitePrefix) && dbURL[:len(sqlitePrefix)] == sqlitePrefix {
		dbPath = dbURL[len(sqlitePrefix):]
	}
	migrationsDir := getenv("AMH_MIGRATIONS_DIR", "./store/migrations")
	host := getenv("AMH_DAEMON_HOST", "127.0.0.1")
	port := getenv("AMH_DAEMON_PORT", "8080")
	apiPort := getenv("AMH_API_PORT", "8090")
	tickMs, err := strconv.Atoi(getenv("HABITAT_ROUTINE_TICK_MS", "60000"))
	if err != nil {
		log.Error("invalid HABITAT_ROUTINE_TICK_MS", "error", err)
		os.Exit(1)
	}

	db, err := store.Open(dbPath, migrationsDir)
	if err != nil {
		log.Error("failed to open store", "error", err)
		os.Exit(1)
	}
	defer db.Close()
	log.Info("store ready", "path", dbPath, "migrations", migrationsDir)

	sched := scheduler.New(time.Duration(tickMs)*time.Millisecond, log)
	sched.AddRoutine(func(ctx context.Context, tick time.Time) {
		log.Debug("scheduler tick", "at", tick)
	})

	healthSrv := health.New(host+":"+port, db, log)

	// No exporter wired yet: spans are recorded but not shipped anywhere
	// until an OTLP exporter is added (OTEL_EXPORTER_OTLP_ENDPOINT is
	// already reserved for this in .env.example). Recording without
	// exporting is a valid V0 default — see daemon/observability's doc.
	tp := observability.Init(nil)
	defer tp.Shutdown(context.Background())

	// Fail closed: the API server enforces two distinct role tokens (§12,
	// §14.7's anti-reward-hacking discipline — an agent must not be able
	// to approve its own ApprovalGate ticket), and there is no
	// unauthenticated fallback mode. If either token is unset, refuse to
	// start rather than run the actuation/approval API open to anyone who
	// can reach the port. See daemon/authn's doc comment and
	// .env.example for how to configure these.
	agentToken := os.Getenv("AMH_API_AGENT_TOKEN")
	operatorToken := os.Getenv("AMH_API_OPERATOR_TOKEN")
	auth, err := authn.New(agentToken, operatorToken)
	if err != nil {
		log.Error("refusing to start: API auth is not configured", "error", err,
			"hint", "set AMH_API_AGENT_TOKEN and AMH_API_OPERATOR_TOKEN to two distinct secrets")
		os.Exit(1)
	}

	apiSrv := api.New(host+":"+apiPort, db, tp, auth, log)

	sup := supervisor.New("amh-daemon", supervisor.OneForOne, 5, time.Minute, log)
	sup.Add(supervisor.Child{Name: "scheduler", Run: sched.Run})
	sup.Add(supervisor.Child{Name: "health", Run: healthSrv.Run})
	sup.Add(supervisor.Child{Name: "api", Run: apiSrv.Run})

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
