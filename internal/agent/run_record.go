package agent

import (
	"strings"

	"reasonix/internal/run"
	"reasonix/internal/taskpolicy"
)

// foldPolicyIntoRunSpec folds the frozen turn policy's constraints and risk
// into a RunSpec. It never re-derives intent — intent is computed exactly once
// in taskcontract.BuildRunSpec.
func foldPolicyIntoRunSpec(spec *run.RunSpec, p taskpolicy.TaskPolicy) {
	if p.Constraints.ForbidMutation {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidMutation})
	}
	if p.Constraints.ForbidTests {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidTests})
	}
	if p.Constraints.ForbidExternal {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintForbidExternal})
	}
	if len(p.Constraints.AllowedChecks) > 0 {
		spec.Constraints = append(spec.Constraints, run.Constraint{
			Kind: run.ConstraintAllowedChecks, Value: strings.Join(p.Constraints.AllowedChecks, ","),
		})
	}
	if p.Constraints.PlanModeReadOnly {
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintPlanModeReadOnly})
	}
	switch p.Risk {
	case taskpolicy.RiskHigh:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintHighRisk})
	case taskpolicy.RiskMedium:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintMediumRisk})
	}
}
