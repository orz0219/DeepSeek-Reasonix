package harness

import (
	"context"

	"reasonix/internal/run"
)

// ModelContext is what the model currently needs to know: the goal, where the
// run stands, and the observations produced so far. It is a projection — a
// model-facing view of the run and its execution history, never a database.
type ModelContext struct {
	Goal         string
	Step         int
	MaxSteps     int
	Observations []Observation
}

// ContextManager builds the model-facing context from the run and folds new
// observations into the working memory for the next round.
type ContextManager interface {
	Build(ctx context.Context, r *run.Run) ModelContext
	AppendObservation(ctx context.Context, r *run.Run, obs Observation)
}

// MemoryContext is a minimal ContextManager that accumulates observations in
// memory. It is the reference projection and the default for tests; production
// replaces it with the session context manager in later phases.
type MemoryContext struct {
	observations []Observation
}

// Build projects the run + accumulated observations into a ModelContext.
func (m *MemoryContext) Build(ctx context.Context, r *run.Run) ModelContext {
	if m == nil {
		return ModelContext{}
	}
	return ModelContext{
		Goal:         r.Goal,
		Step:         r.Step,
		MaxSteps:     r.MaxSteps,
		Observations: append([]Observation(nil), m.observations...),
	}
}

// AppendObservation records one observation into working memory.
func (m *MemoryContext) AppendObservation(ctx context.Context, r *run.Run, obs Observation) {
	if m != nil {
		m.observations = append(m.observations, obs)
	}
}
