package verification

import (
	"context"
	"strconv"
	"strings"

	"reasonix/internal/run"
)

// CommandRunner executes one shell command for verification. A nil error and
// exit code 0 mean the command passed.
type CommandRunner func(ctx context.Context, command string) (exitCode int, output string, err error)

// CommandVerifier proves command-kind VerificationSpecs by running their
// commands. It reports an unproven result for any spec it cannot evaluate
// (mutation/review/manual, or a command without a concrete string), so a run
// that owes a proof never completes just because its command checks passed.
type CommandVerifier struct {
	// Runner executes commands; required.
	Runner CommandRunner
	// AllowedCommands, when non-empty, limits which commands may run.
	AllowedCommands []string
}

// Verify runs every concrete command spec and reports whether the run's
// command-level expectations are all satisfied and nothing else stayed
// unproven.
func (v *CommandVerifier) Verify(ctx context.Context, r *run.Run, claim Claim) Result {
	if v == nil || v.Runner == nil {
		return Result{Passed: false, Kind: KindCommand, Reason: "no command runner"}
	}
	var evidence []string
	var unproven []string
	for _, spec := range r.Verification {
		if spec.Kind != run.VerificationCommand || strings.TrimSpace(spec.Command) == "" {
			if spec.Kind != "" {
				unproven = append(unproven, string(spec.Kind))
			}
			continue
		}
		if !v.allowed(spec.Command) {
			unproven = append(unproven, "not_allowed:"+spec.Command)
			continue
		}
		code, _, err := v.Runner(ctx, spec.Command)
		if err != nil {
			return Result{
				Passed:   false,
				Kind:     KindCommand,
				Reason:   spec.Command + ": " + err.Error(),
				Evidence: evidence,
			}
		}
		if code != 0 {
			return Result{
				Passed:   false,
				Kind:     KindCommand,
				Reason:   spec.Command + " exited " + strconv.Itoa(code),
				Evidence: evidence,
			}
		}
		evidence = append(evidence, spec.Command)
	}
	if len(unproven) > 0 {
		return Result{Passed: false, Kind: KindCommand, Evidence: evidence, Unproven: unproven}
	}
	return Result{Passed: true, Kind: KindCommand, Evidence: evidence}
}

func (v *CommandVerifier) allowed(command string) bool {
	if v == nil || len(v.AllowedCommands) == 0 {
		return true
	}
	for _, allowed := range v.AllowedCommands {
		if strings.EqualFold(strings.TrimSpace(allowed), strings.TrimSpace(command)) {
			return true
		}
	}
	return false
}
