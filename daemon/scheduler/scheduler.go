// Package scheduler is the one kernel-owned ticker that fires routines,
// deadlines, and triggers. V0 slice: a single interval ticker; cron
// expressions and event-driven triggers are deferred (see
// docs/AMH-SPECIFICATION.md §1, HABITAT_ROUTINE_TICK_MS).
package scheduler

import (
	"context"
	"log/slog"
	"time"
)

// RoutineFunc is invoked on every tick. It must not block past the next
// tick interval; long work should be handed off to a goroutine/workflow.
type RoutineFunc func(ctx context.Context, tick time.Time)

type Scheduler struct {
	Interval time.Duration
	Log      *slog.Logger
	routines []RoutineFunc
}

func New(interval time.Duration, log *slog.Logger) *Scheduler {
	if log == nil {
		log = slog.Default()
	}
	return &Scheduler{Interval: interval, Log: log}
}

func (s *Scheduler) AddRoutine(fn RoutineFunc) {
	s.routines = append(s.routines, fn)
}

// Run blocks, firing all registered routines on every tick, until ctx is
// cancelled. Matches the supervisor.Child.Run signature.
func (s *Scheduler) Run(ctx context.Context) error {
	if s.Interval <= 0 {
		s.Interval = 60 * time.Second
	}
	ticker := time.NewTicker(s.Interval)
	defer ticker.Stop()
	s.Log.Info("scheduler: started", "interval", s.Interval)
	for {
		select {
		case <-ctx.Done():
			return nil
		case tick := <-ticker.C:
			for _, fn := range s.routines {
				fn(ctx, tick)
			}
		}
	}
}
