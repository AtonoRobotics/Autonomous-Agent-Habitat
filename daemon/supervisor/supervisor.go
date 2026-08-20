// Package supervisor implements an OTP-style supervision tree: children run
// under a restart strategy with a restart-intensity limit, so a crashing
// child is isolated from the rest of the daemon. See
// docs/AMH-SPECIFICATION.md §11 ("Structural resilience").
package supervisor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// Strategy selects which siblings restart when one child exits.
type Strategy int

const (
	// OneForOne restarts only the child that exited.
	OneForOne Strategy = iota
	// OneForAll restarts every child when any one exits.
	OneForAll
	// RestForOne restarts the exited child and every child started after it.
	RestForOne
)

// Child is a supervised unit of work. Run must block until ctx is
// cancelled or the child's own work is done/failed; a non-nil error is
// treated as a crash and triggers the restart strategy.
type Child struct {
	Name string
	Run  func(ctx context.Context) error
}

// Supervisor runs a fixed set of children under a restart strategy with a
// bounded restart intensity (maxRestarts within window) — matching OTP's
// restart-intensity limit so a persistently crashing child eventually
// escalates instead of spinning forever.
type Supervisor struct {
	Name         string
	Strategy     Strategy
	MaxRestarts  int
	Window       time.Duration
	Log          *slog.Logger
	children     []Child
	mu           sync.Mutex
	restartsAt   []time.Time
}

func New(name string, strategy Strategy, maxRestarts int, window time.Duration, log *slog.Logger) *Supervisor {
	if log == nil {
		log = slog.Default()
	}
	return &Supervisor{
		Name:        name,
		Strategy:    strategy,
		MaxRestarts: maxRestarts,
		Window:      window,
		Log:         log,
	}
}

func (s *Supervisor) Add(c Child) {
	s.children = append(s.children, c)
}

// ErrRestartIntensityExceeded is returned when children crash more than
// MaxRestarts times within Window — the supervisor gives up rather than
// restart-loop forever, escalating to whatever supervises it.
var ErrRestartIntensityExceeded = errors.New("supervisor: restart intensity exceeded")

// Run starts all children and blocks until ctx is cancelled or the restart
// intensity limit is exceeded.
func (s *Supervisor) Run(ctx context.Context) error {
	if len(s.children) == 0 {
		<-ctx.Done()
		return nil
	}

	type exit struct {
		idx int
		err error
	}
	exits := make(chan exit, len(s.children))
	childCtxs := make([]context.Context, len(s.children))
	cancels := make([]context.CancelFunc, len(s.children))

	start := func(i int) {
		cctx, cancel := context.WithCancel(ctx)
		childCtxs[i] = cctx
		cancels[i] = cancel
		go func() {
			err := s.children[i].Run(cctx)
			select {
			case exits <- exit{idx: i, err: err}:
			case <-ctx.Done():
			}
		}()
	}

	for i := range s.children {
		start(i)
	}

	for {
		select {
		case <-ctx.Done():
			for _, cancel := range cancels {
				if cancel != nil {
					cancel()
				}
			}
			return nil
		case e := <-exits:
			if e.err == nil {
				// Clean exit: don't restart, just stop tracking this slot.
				s.Log.Info("supervisor: child exited cleanly", "supervisor", s.Name, "child", s.children[e.idx].Name)
				continue
			}
			s.Log.Error("supervisor: child crashed", "supervisor", s.Name, "child", s.children[e.idx].Name, "error", e.err)
			if !s.allowRestart() {
				for _, cancel := range cancels {
					if cancel != nil {
						cancel()
					}
				}
				return fmt.Errorf("%w: %s", ErrRestartIntensityExceeded, s.Name)
			}
			switch s.Strategy {
			case OneForOne:
				start(e.idx)
			case OneForAll:
				for i, cancel := range cancels {
					if cancel != nil {
						cancel()
					}
					start(i)
				}
			case RestForOne:
				for i := e.idx; i < len(s.children); i++ {
					if cancels[i] != nil {
						cancels[i]()
					}
					start(i)
				}
			}
		}
	}
}

func (s *Supervisor) allowRestart() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-s.Window)
	kept := s.restartsAt[:0]
	for _, t := range s.restartsAt {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	s.restartsAt = kept
	if len(s.restartsAt) >= s.MaxRestarts {
		return false
	}
	s.restartsAt = append(s.restartsAt, now)
	return true
}
