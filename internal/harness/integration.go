package harness

import (
	"fmt"

	"reasonix/internal/run"
	"reasonix/internal/taskcontract"
	"reasonix/internal/taskpolicy"
)

// BuildRunFromContract projects a taskcontract.Contract into a run.Run. This is
// the Phase 1 bridge: the existing task layer remains authoritative while the
// harness gains visibility into every execution lifecycle. Building a Run from
// a contract never makes a model call.
func BuildRunFromContract(contract *taskcontract.Contract, goal string, maxSteps int) *run.Run {
	if contract == nil {
		spec := taskcontract.BuildRunSpec(goal)
		spec.Limits.MaxSteps = maxSteps
		return run.New(spec)
	}
	spec := contract.ToRunSpec(goal)
	spec.Limits.MaxSteps = maxSteps
	return run.New(spec)
}

// BuildRunFromPolicyAndInput derives a RunSpec from raw input and the frozen
// policy in one pass. Intent is classified exactly once and never enters a
// persisted lifecycle. The caller starts the Run with r.Start() when the
// harness loop begins.
func BuildRunFromPolicyAndInput(input string, policy taskpolicy.TaskPolicy, maxSteps int) *run.Run {
	spec := taskcontract.BuildRunSpec(input)
	// Fold the policy's constraints into the spec.
	switch policy.Risk {
	case taskpolicy.RiskHigh:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintHighRisk})
	case taskpolicy.RiskMedium:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintMediumRisk})
	}
	if policy.Constraints.ForbidMutation {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidMutation})
	}
	if policy.Constraints.ForbidTests {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidTests})
	}
	if policy.Constraints.ForbidExternal {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidExternal})
	}
	if policy.Constraints.PlanModeReadOnly {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintPlanModeReadOnly})
	}
	if len(policy.Constraints.AllowedChecks) > 0 {
		spec.Constraints = append(spec.Constraints, run.Constraint{
			Kind: run.ConstraintAllowedChecks,
			Value: joinChecks(policy.Constraints.AllowedChecks),
		})
	}
	spec.Limits.MaxSteps = maxSteps
	return run.New(spec)
}

// BuildPolicyFromRunSpec derives a ConstraintPolicy from a Run's constraints.
// This lets the harness Loop check actions against the same boundary set the
// old taskpolicy.TaskPolicy carried.
func BuildPolicyFromRunSpec(r *run.Run) ConstraintPolicy {
	if r == nil {
		return ConstraintPolicy{}
	}
	return NewConstraintPolicy(r.Constraints)
}

// RunSummary returns a one-line summary of a Run's state for logging and
// diagnostics. It never exposes requirement text or evidence content.
func RunSummary(r *run.Run) string {
	if r == nil {
		return "nil"
	}
	return fmt.Sprintf("run=%s status=%s step=%d/%d reason=%s",
		r.ID, r.Status, r.Step, r.MaxSteps, r.Reason)
}

func joinChecks(checks []string) string {
	if len(checks) == 0 {
		return ""
	}
	out := checks[0]
	for _, c := range checks[1:] {
		out += "," + c
	}
	return out
}
