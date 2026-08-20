// Package health exposes the daemon's watchdog HTTP surface: /healthz
// (process is up) and /readyz (store is reachable). See
// docs/AMH-SPECIFICATION.md §11.
package health

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"
)

type Server struct {
	Addr string
	DB   *sql.DB
	Log  *slog.Logger

	srv *http.Server
}

func New(addr string, db *sql.DB, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	return &Server{Addr: addr, DB: db, Log: log}
}

// Run blocks, serving health endpoints, until ctx is cancelled. Matches the
// supervisor.Child.Run signature.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		pingCtx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.DB.PingContext(pingCtx); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			json.NewEncoder(w).Encode(map[string]string{"status": "unready", "error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})

	s.srv = &http.Server{Addr: s.Addr, Handler: mux}

	errCh := make(chan error, 1)
	go func() {
		s.Log.Info("health: listening", "addr", s.Addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}
