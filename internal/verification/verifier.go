// Package verification decides whether a Run's claimed completion is
// genuinely satisfied. It is a harness core, not an attribute of the task
// contract: the model says "done", the Verifier proves it. Sources may be
// commands, tests, diffs, file existence, lint, structured checks, or manual
// approval — each is a Verifier implementation over the run's VerificationSpecs.
package verification

import (
	"context"

	"reasonix/internal/run"
)

// Claim is what the model asserted to be true: its final answer. The run
// carries the proof expectations (VerificationSpec) and, in later phases, the
// accumulated observations.
type Claim struct {
	Text string
}

// Kind classifies how a verdict was produced.
type Kind uint8

const (
	KindCommand Kind = iota
	KindTest
	KindDiff
	KindFile
	KindLint
	KindStructured
	KindManual
	KindMutation
)

// Result is the outcome of one verification pass. Passed is authoritative:
// a run with nothing to prove passes; a run with unproven expectations does
// not.
type Result struct {
	Passed   bool
	Kind     Kind
	Evidence []string // what proved it (commands, paths, receipts)
	Reason   string   // why it failed, or what stayed unproven
	// Unproven names the expectation kinds a verifier could not evaluate.
	Unproven []string
}

// Verifier proves a run's claimed completion against its VerificationSpecs.
type Verifier interface {
	Verify(ctx context.Context, run *run.Run, claim Claim) Result
}
