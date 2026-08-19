package harness

import (
	"context"

	"reasonix/internal/run"
	"reasonix/internal/verification"
)

// ModelDriver is the interface a harness Model adapter must implement. It is
// shaped around the harness.Decision return so the Loop stays agnostic of
// provider streaming details. The existing agent.Agent already drives a model
// through its own session + provider machinery; this adapter bridges that into
// the harness-decision shape.
type ModelDriver interface {
	// Next reads the model-facing context and returns the model's decision:
	// a final answer (IsFinal) or a tool action (ToolAction).
	Next(ctx context.Context, modelCtx ModelContext) (Decision, error)
}

// RunAwareModel wraps a ModelDriver and attaches the current Run so the
// adapter can read run-scoped state (goal, step, limits) when translating
// back to the host agent's session prompt.
type RunAwareModel struct {
	Driver ModelDriver
}

// Next implements Model by delegating to the wrapped driver.
func (m *RunAwareModel) Next(ctx context.Context, modelCtx ModelContext) (Decision, error) {
	if m == nil || m.Driver == nil {
		return Decision{}, nil
	}
	return m.Driver.Next(ctx, modelCtx)
}

// RunAwareVerifier wraps a verification.Verifier and adapts it to the
// harness-verifier shape so the Loop can route "done" claims through
// the existing verification infrastructure.
type RunAwareVerifier struct {
	Verifier verification.Verifier
}

// Verify implements verification.Verifier by delegating to the wrapped verifier.
func (v *RunAwareVerifier) Verify(ctx context.Context, r *run.Run, claim verification.Claim) verification.Result {
	if v == nil || v.Verifier == nil {
		return verification.Result{Passed: true}
	}
	return v.Verifier.Verify(ctx, r, claim)
}
