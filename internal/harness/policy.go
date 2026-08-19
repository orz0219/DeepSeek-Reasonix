package harness

import (
	"context"
	"strings"

	"reasonix/internal/run"
)

// ConstraintPolicy adapts the run.Run constraint set into a harness Policy.
// It answers "allowed or not" by checking the action against every active
// constraint — never by deciding what the user wanted or whether the run is
// done. The old taskpolicy.TaskPolicy can be projected into a ConstraintPolicy
// via NewConstraintPolicy; building one never makes a model call.
type ConstraintPolicy struct {
	// ForbidMutation blocks every real writer.
	ForbidMutation bool
	// ForbidTests blocks verification commands.
	ForbidTests bool
	// ForbidExternal blocks push/publish/deploy-style actions.
	ForbidExternal bool
	// AllowedChecks, when non-empty, limits verification to these commands.
	AllowedChecks []string
	// PlanModeReadOnly is the explicit plan-mode read-only boundary.
	PlanModeReadOnly bool
}

// NewConstraintPolicy builds a ConstraintPolicy from a Run's constraint set.
func NewConstraintPolicy(constraints []run.Constraint) ConstraintPolicy {
	var p ConstraintPolicy
	for _, c := range constraints {
		switch c.Kind {
		case run.ConstraintForbidMutation:
			p.ForbidMutation = true
		case run.ConstraintForbidTests:
			p.ForbidTests = true
		case run.ConstraintForbidExternal:
			p.ForbidExternal = true
		case run.ConstraintAllowedChecks:
			if c.Value != "" {
				p.AllowedChecks = append(p.AllowedChecks, strings.Split(c.Value, ",")...)
			}
		case run.ConstraintPlanModeReadOnly:
			p.PlanModeReadOnly = true
		}
	}
	return p
}

// Allows implements harness.Policy. It answers whether a specific action may
// proceed under the current constraints. Tool actions that are not mutations
// or verification commands always pass.
func (p ConstraintPolicy) Allows(_ context.Context, _ *run.Run, a Action) (bool, string) {
	if p.ForbidMutation && isMutationAction(a) {
		return false, "mutation forbidden by constraint"
	}
	if p.ForbidTests && isVerificationAction(a) {
		return false, "verification commands forbidden by constraint"
	}
	if p.ForbidExternal && isExternalAction(a) {
		return false, "external actions forbidden by constraint"
	}
	if p.PlanModeReadOnly && isMutationAction(a) {
		return false, "plan mode read-only: no mutations allowed"
	}
	if len(p.AllowedChecks) > 0 && isVerificationAction(a) {
		cmd := extractCommand(a)
		if cmd != "" && !commandAllowed(cmd, p.AllowedChecks) {
			return false, "command not in allowed checks list"
		}
	}
	return true, ""
}

// isMutationAction reports whether the action mutates the workspace.
func isMutationAction(a Action) bool {
	switch strings.ToLower(a.Tool) {
	case "write", "writefile", "edit", "editfile", "multiedit",
		"deletefile", "delete_range", "delete_symbol", "movefile",
		"notebookedit":
		return true
	default:
		return false
	}
}

// isVerificationAction reports whether the action is a verification command.
func isVerificationAction(a Action) bool {
	return strings.ToLower(a.Tool) == "bash" || strings.ToLower(a.Tool) == "shell"
}

// isExternalAction reports whether the action is an external action.
func isExternalAction(a Action) bool {
	lower := strings.ToLower(a.Tool)
	return lower == "git_push" || lower == "publish" || lower == "deploy"
}

// extractCommand pulls the command string from a bash/shell action.
func extractCommand(a Action) string {
	s := string(a.Input)
	idx := strings.Index(s, `"command"`)
	if idx < 0 {
		return ""
	}
	rest := s[idx+len(`"command"`):]
	rest = strings.TrimSpace(rest)
	if len(rest) > 0 && rest[0] == ':' {
		rest = rest[1:]
	}
	rest = strings.TrimSpace(rest)
	if len(rest) < 2 || rest[0] != '"' {
		return ""
	}
	end := strings.Index(rest[1:], `"`)
	if end < 0 {
		return ""
	}
	return rest[1 : end+1]
}

// commandAllowed checks if a command is in the allowed list (prefix match).
func commandAllowed(cmd string, allowed []string) bool {
	cmd = strings.TrimSpace(cmd)
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasPrefix(cmd, a) {
			return true
		}
	}
	return false
}
