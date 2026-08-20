package supervisor

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestOneForOneRestartsOnlyCrashedChild(t *testing.T) {
	var starts, otherStarts atomic.Int32

	sup := New("test", OneForOne, 5, time.Second, nil)
	sup.Add(Child{Name: "crasher", Run: func(ctx context.Context) error {
		n := starts.Add(1)
		if n == 1 {
			return errors.New("boom")
		}
		<-ctx.Done()
		return nil
	}})
	sup.Add(Child{Name: "stable", Run: func(ctx context.Context) error {
		otherStarts.Add(1)
		<-ctx.Done()
		return nil
	}})

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = sup.Run(ctx)

	if got := starts.Load(); got < 2 {
		t.Fatalf("expected crasher to restart at least once, got %d starts", got)
	}
	if got := otherStarts.Load(); got != 1 {
		t.Fatalf("expected stable child to start exactly once under OneForOne, got %d", got)
	}
}

func TestRestartIntensityExceededStopsRestarting(t *testing.T) {
	var starts atomic.Int32
	sup := New("test", OneForOne, 2, time.Minute, nil)
	sup.Add(Child{Name: "always-crashes", Run: func(ctx context.Context) error {
		starts.Add(1)
		return errors.New("boom")
	}})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := sup.Run(ctx)

	if !errors.Is(err, ErrRestartIntensityExceeded) {
		t.Fatalf("expected ErrRestartIntensityExceeded, got %v", err)
	}
	// MaxRestarts=2 means: 1 initial start + up to 2 restarts = 3 starts max.
	if got := starts.Load(); got > 3 {
		t.Fatalf("expected at most 3 starts before giving up, got %d", got)
	}
}
