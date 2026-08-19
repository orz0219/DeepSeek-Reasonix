package run

import "time"

// IntentHint is a derived routing/behavior hint about what the user asked for.
// It is computed once from the input and never enters a persisted lifecycle.
type IntentHint string

const (
	IntentUnknown        IntentHint = ""
	IntentConversation   IntentHint = "conversation"
	IntentAdvisory       IntentHint = "advisory"
	IntentObservableRead IntentHint = "observable_read"
	IntentMutation       IntentHint = "mutation"
	IntentPersistent     IntentHint = "persistent_action"
)

// ConstraintKind names a host boundary a Run must honor.
type ConstraintKind string

const (
	ConstraintForbidMutation   ConstraintKind = "forbid_mutation"
	ConstraintForbidTests      ConstraintKind = "forbid_tests"
	ConstraintForbidExternal   ConstraintKind = "forbid_external"
	ConstraintAllowedChecks    ConstraintKind = "allowed_checks"
	ConstraintPlanModeReadOnly ConstraintKind = "plan_mode_read_only"
	ConstraintScopePaths       ConstraintKind = "scope_paths"
	ConstraintMediumRisk       ConstraintKind = "medium_risk"
	ConstraintHighRisk         ConstraintKind = "high_risk"
)

// Constraint is one host boundary the run must honor.
type Constraint struct {
	Kind  ConstraintKind
	Value string   // e.g. an allowed command; "" when Paths carry the boundary
	Paths []string // concrete targets for ConstraintScopePaths
}

// VerificationKind selects how an acceptance proof is produced.
type VerificationKind string

const (
	VerificationCommand  VerificationKind = "command"
	VerificationMutation VerificationKind = "mutation"
	VerificationReview   VerificationKind = "review"
	VerificationManual   VerificationKind = "manual"
)

// VerificationSpec is one expected acceptance proof. For VerificationCommand
// an empty Command accepts any verification-classified command.
type VerificationSpec struct {
	Kind    VerificationKind
	Command string
	Source  string // where the expectation came from (plan, contract, policy)
}

// Limits bounds one Run. Zero values mean unbounded.
type Limits struct {
	MaxSteps     int
	MaxTokens    int64
	MaxCostCents int64
	MaxWallClock time.Duration
}

// RunSpec is the descriptive contract one Run executes against: what to
// accomplish (Goal), how it was classified (Intent), what it may not do
// (Constraints), what must be proven (Verification), and what bounds it
// (Limits). It never carries execution state — no statuses, no evidence, no
// todos. Building a RunSpec must not make a model call.
type RunSpec struct {
	Goal         string
	Intent       IntentHint
	Constraints  []Constraint
	Verification []VerificationSpec
	Limits       Limits
}
