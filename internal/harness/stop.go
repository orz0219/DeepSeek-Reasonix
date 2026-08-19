package harness

import (
	"context"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/run"
)

// shouldStop reports whether the loop must halt before another model round:
// the context was cancelled, the run reached a terminal state, or a configured
// budget (steps or wall clock) is exhausted.
func (l *Loop) shouldStop(ctx context.Context, r *run.Run) bool {
	if ctx.Err() != nil {
		return true
	}
	if r.Status.Terminal() {
		return true
	}
	if r.MaxSteps > 0 && r.Step >= r.MaxSteps {
		return true
	}
	if r.Limits.MaxWallClock > 0 && !r.StartedAt.IsZero() && time.Since(r.StartedAt) >= r.Limits.MaxWallClock {
		return true
	}
	return false
}

// stop lands the run after shouldStop returned true. A budget exhaustion
// pauses the run (resumable); cancellation marks it cancelled; a terminal
// status already set by the loop is left untouched.
func (l *Loop) stop(ctx context.Context, r *run.Run) error {
	switch {
	case ctx.Err() != nil:
		r.Cancel()
		l.emitRun(event.RunCancelled, r, ctx.Err().Error())
		return ctx.Err()
	case r.Status.Terminal():
		return nil
	case r.MaxSteps > 0 && r.Step >= r.MaxSteps:
		r.Pause()
		r.Reason = "max_steps"
		l.emitRun(event.RunPaused, r, "max_steps")
		return nil
	case r.Limits.MaxWallClock > 0 && !r.StartedAt.IsZero() && time.Since(r.StartedAt) >= r.Limits.MaxWallClock:
		r.Pause()
		r.Reason = "max_wall_clock"
		l.emitRun(event.RunPaused, r, "max_wall_clock")
		return nil
	default:
		return nil
	}
}
