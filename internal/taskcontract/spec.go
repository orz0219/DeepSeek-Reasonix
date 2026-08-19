package taskcontract

import (
	"reasonix/internal/run"
	"reasonix/internal/taskintent"
)

// ToRunSpec projects the contract's descriptive parts into the harness run
// specification. It deliberately excludes everything execution-shaped —
// Requirement.Status, Check.Status, Evidence, the mutation epoch, and the todo
// view never enter a RunSpec. The RunSpec only describes what the run is
// supposed to accomplish and how the host will know.
func (c *Contract) ToRunSpec(goal string) run.RunSpec {
	if c == nil {
		return run.RunSpec{Goal: goal}
	}
	spec := run.RunSpec{Goal: goal, Intent: intentHint(c.Intent)}
	if len(c.Scope.Paths) > 0 {
		spec.Constraints = append(spec.Constraints, run.Constraint{
			Kind: run.ConstraintScopePaths, Paths: c.Scope.Paths,
		})
	}
	switch c.Risk {
	case RiskHigh:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintHighRisk})
	case RiskMedium:
		spec.Constraints = append(spec.Constraints, run.Constraint{Kind: run.ConstraintMediumRisk})
	}
	for _, check := range c.Checks {
		spec.Verification = append(spec.Verification, checkVerification(check))
	}
	return spec
}

func checkVerification(check Check) run.VerificationSpec {
	if check.Kind == CheckMutation {
		return run.VerificationSpec{Kind: run.VerificationMutation, Source: "contract"}
	}
	return run.VerificationSpec{Kind: run.VerificationCommand, Command: check.Command, Source: "contract"}
}

// BuildRunSpec turns raw user input into a RunSpec in one pass: intent is
// classified exactly once and never enters a persisted lifecycle. This is the
// pure input -> RunSpec path; the policy layer folds host constraints and
// limits in afterward.
func BuildRunSpec(input string) run.RunSpec {
	spec := run.RunSpec{Goal: input, Intent: intentHint(taskintent.Classify(input))}
	if spec.Intent == run.IntentMutation || spec.Intent == run.IntentPersistent {
		// The ask IS the evidence (matches Atomic semantics): a successful
		// mutation satisfies it.
		spec.Verification = append(spec.Verification, run.VerificationSpec{
			Kind: run.VerificationMutation, Source: "input",
		})
	}
	return spec
}

func intentHint(i taskintent.Intent) run.IntentHint {
	switch i {
	case taskintent.Conversation:
		return run.IntentConversation
	case taskintent.Advisory:
		return run.IntentAdvisory
	case taskintent.ObservableRead:
		return run.IntentObservableRead
	case taskintent.Mutation:
		return run.IntentMutation
	case taskintent.PersistentAction:
		return run.IntentPersistent
	default:
		return run.IntentUnknown
	}
}
