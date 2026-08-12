package permission

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/shellparse"
)

// ExplicitlyDenies reports only configured deny-rule matches. It deliberately
// excludes the fallback Mode so installing or explicitly authorizing an MCP
// server remains the final allow decision.
func (p Policy) ExplicitlyDenies(toolName string, args json.RawMessage) bool {
	subjects := Subjects(args)
	if len(subjects) == 0 {
		subjects = []string{""}
	}
	for _, subject := range subjects {
		if matchAnyRaw(p.Deny, toolName, subject) {
			return true
		}
	}
	return false
}

// Approver resolves an Ask decision interactively. Implementations live in the
// front-end (the chat TUI); a non-interactive run passes a nil Approver, which
// the Gate treats as "allow" to preserve autonomous behaviour.
type Approver interface {
	// Approve asks the user about a pending call. It returns whether to allow
	// it and whether to remember that choice as a new rule. A non-nil err (e.g.
	// the context was cancelled while waiting) aborts the turn.
	Approve(ctx context.Context, toolName, subject string, args json.RawMessage) (allow, remember bool, err error)
}

// ReasonedApprover is the optional extension used by frontends that can return
// a denial reason to feed back to the model.
type ReasonedApprover interface {
	ApproveWithReason(ctx context.Context, toolName, subject string, args json.RawMessage) (allow, remember bool, reason string, err error)
}

// PolicyReasonedApprover receives the explicit permission-rule provenance that
// caused an Ask decision. Frontends can display it without duplicating Policy
// matching logic; older Approver implementations remain source-compatible.
type PolicyReasonedApprover interface {
	ApproveWithPolicyReason(ctx context.Context, toolName, subject string, args json.RawMessage, policyReason string) (allow, remember bool, reason string, err error)
}

// Gate is what the agent consults at execute time: a Policy plus an optional
// Approver. It satisfies the agent's Gate interface structurally.
type Gate struct {
	Policy   Policy
	Approver Approver

	// OnRemember, when set, is invoked with a new allow rule the user chose to
	// remember (e.g. "Bash(go build)"), so the front-end can persist it.
	OnRemember func(rule string)
}

// NewGate wires a Policy to an Approver (nil for non-interactive use).
func NewGate(p Policy, a Approver) *Gate { return &Gate{Policy: p, Approver: a} }

// Check decides whether a tool call may run. It is the method the agent's Gate
// interface expects. A denied or refused call returns allow=false with a short
// reason the agent feeds back to the model.
func (g *Gate) Check(ctx context.Context, toolName string, args json.RawMessage, readOnly bool) (bool, string, error) {
	if toolName == "bash" && !readOnly {
		if BashCommandIsReadOnly(args) {
			readOnly = true
		}
	}
	decision := g.Policy.Decide(toolName, readOnly, args)
	ruleReason := ""
	if rule, ok := g.Policy.MatchedRule(toolName, decision, args); ok {
		ruleReason = fmt.Sprintf("Matched permission rule: %s %s", decision, rule)
	}
	switch decision {
	case Deny:
		reason := "denied by permission policy — this tool/command is on the deny list. Do not retry it; choose another approach or stop and explain."
		if ruleReason != "" {
			reason = ruleReason + "\n" + reason
		}
		return false, reason, nil
	case Ask:
		if g.Approver == nil {
			return true, "", nil
		}
		subject := Subject(args)
		allow, remember, approverReason, err := g.approve(ctx, toolName, subject, args, ruleReason)
		if err != nil {
			return false, "approval aborted", err
		}
		if !allow {
			reason := "the user declined this tool call — do not retry it; ask how they would like to proceed or choose another approach."
			if approverReason != "" {
				reason = approverReason
			}
			return false, reason, nil
		}
		if remember && g.OnRemember != nil {

			g.OnRemember(toolName)

			if rule, ok := ParseRule(toolName); ok {
				g.Policy.Allow = append(g.Policy.Allow, rule)
			}
		}
		return true, "", nil
	default:
		return true, "", nil
	}
}

// ExplicitlyDenies reports whether an explicit deny rule matches. Authorized
// MCP servers use this narrow view so install-time authorization is not
// followed by redundant per-call approval prompts.
func (g *Gate) ExplicitlyDenies(toolName string, args json.RawMessage) bool {
	return g.Policy.ExplicitlyDenies(toolName, args)
}

func (g *Gate) approve(ctx context.Context, toolName, subject string, args json.RawMessage, policyReason string) (bool, bool, string, error) {
	if a, ok := g.Approver.(PolicyReasonedApprover); ok {
		return a.ApproveWithPolicyReason(ctx, toolName, subject, args, policyReason)
	}
	if a, ok := g.Approver.(ReasonedApprover); ok {
		return a.ApproveWithReason(ctx, toolName, subject, args)
	}
	allow, remember, err := g.Approver.Approve(ctx, toolName, subject, args)
	return allow, remember, "", err
}

// rememberRule builds the rule string persisted when the user picks "always
// allow". Bash commands prefer a safe command prefix (e.g. go test:*) so
// "always allow" covers similar invocations with different arguments. File
// mutation tools are remembered tool-wide ("Edit") so approving one file edit
// covers all files. Other tools are remembered by tool name. Deny and ask rules keep their higher precedence.
func rememberRule(toolName, subject string) string {
	return RememberRuleForScope(toolName, subject)
}

// RememberRuleForScope builds the rule string persisted when the user chooses
// an always-allow option. Bash commands prefer a safe prefix (go test:*) so
// similar invocations (different search terms, different test packages) match;
// when no safe prefix can be extracted the exact command is used. File
// mutation tools are always remembered tool-wide (Edit). Other tools use their
// bare tool name. Deny rules still take precedence on every call.
func RememberRuleForScope(toolName, subject string) string {
	subject = strings.TrimSpace(subject)
	if subject != "" && toolName == "bash" {
		if pattern := BashCommandPrefix(subject); pattern != "" {
			return "Bash(" + pattern + ")"
		}
		return "Bash=" + subject
	}
	if IsFileMutationTool(toolName) {
		return "Edit"
	}
	return toolName
}

// SessionGrantKey returns the in-memory rule for "allow this session". Bash
// prefers a command prefix when one is available, falling back to the exact
// command when unsafe. File mutation tools share a single Edit grant.
func SessionGrantKey(toolName, subject string) string {
	return SessionGrantRuleForScope(toolName, subject)
}

// SessionGrantRuleForScope returns the in-memory rule for a session grant.
// Bash prefers a command prefix when one is available; file mutation tools
// share a single Edit grant; all other tools return the bare tool name.
func SessionGrantRuleForScope(toolName, subject string) string {
	subject = strings.TrimSpace(subject)
	if toolName == "bash" && subject != "" {
		if pattern := BashCommandPrefix(subject); pattern != "" {
			return "Bash(" + pattern + ")"
		}
		return "Bash=" + subject
	}
	if IsFileMutationTool(toolName) {
		return "Edit"
	}
	return toolName
}

// BashCommandPrefix returns a conservative prefix rule for "similar command"
// approvals. It avoids shell syntax and keeps the prefix at command-word
// boundaries, so approving "go test ./..." grants "go test:*" rather than a
// broader "go *".
func BashCommandPrefix(subject string) string {
	cmd := strings.TrimSpace(subject)
	if cmd == "" || containsShellSyntax(cmd) || bashSubjectRequiresExactRule(cmd) {
		return ""
	}
	if BashDangerWarning(cmd) != "" {
		return ""
	}
	fields, malformed := shellparse.StaticFields(cmd)
	if malformed != "" {
		return ""
	}
	if len(fields) < 2 {
		return ""
	}
	base := strings.ToLower(fields[0])
	if isPackageManagerRun(base) && len(fields) >= 3 && strings.ToLower(fields[1]) == "run" {
		return fields[0] + " " + fields[1] + " " + fields[2] + ":*"
	}
	return fields[0] + " " + fields[1] + ":*"
}
