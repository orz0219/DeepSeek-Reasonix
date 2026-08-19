package run

// Builder accumulates a RunSpec from host signals. It is the structural half
// of intent derivation: the builder records what the host decided, it does not
// classify input itself (that lives in internal/taskcontract for now).
type Builder struct {
	spec RunSpec
}

// NewBuilder starts an empty RunSpec.
func NewBuilder() *Builder { return &Builder{} }

// WithGoal sets the run's goal text.
func (b *Builder) WithGoal(goal string) *Builder {
	if b != nil {
		b.spec.Goal = goal
	}
	return b
}

// WithIntent records the one-time derived intent hint.
func (b *Builder) WithIntent(hint IntentHint) *Builder {
	if b != nil {
		b.spec.Intent = hint
	}
	return b
}

// AddConstraint appends one host boundary.
func (b *Builder) AddConstraint(c Constraint) *Builder {
	if b != nil {
		b.spec.Constraints = append(b.spec.Constraints, c)
	}
	return b
}

// AddVerification appends one expected acceptance proof.
func (b *Builder) AddVerification(v VerificationSpec) *Builder {
	if b != nil {
		b.spec.Verification = append(b.spec.Verification, v)
	}
	return b
}

// WithLimits sets the run bounds.
func (b *Builder) WithLimits(l Limits) *Builder {
	if b != nil {
		b.spec.Limits = l
	}
	return b
}

// Build returns the accumulated spec.
func (b *Builder) Build() RunSpec {
	if b == nil {
		return RunSpec{}
	}
	return b.spec
}
