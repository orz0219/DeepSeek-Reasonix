package hook

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Payload is the JSON envelope written to a hook's stdin.
type Payload struct {
	Event            Event           `json:"event"`
	SessionID        string          `json:"sessionId,omitempty"`
	Cwd              string          `json:"cwd"`
	ToolName         string          `json:"toolName,omitempty"`
	ToolArgs         json.RawMessage `json:"toolArgs,omitempty"`
	Subject          string          `json:"subject,omitempty"`
	ToolResult       string          `json:"toolResult,omitempty"`
	Prompt           string          `json:"prompt,omitempty"`
	LastAssistant    string          `json:"lastAssistantText,omitempty"`
	Turn             int             `json:"turn,omitempty"`
	Message          string          `json:"message,omitempty"`   // Notification: what needs attention
	Trigger          string          `json:"trigger,omitempty"`   // PreCompact: "auto" | "manual"
	Reasoning        string          `json:"reasoning,omitempty"` // PostLLMCall: the model's raw reasoning text
	Error            string          `json:"error,omitempty"`
	Source           string          `json:"source,omitempty"`
	Reason           string          `json:"reason,omitempty"`
	NotificationType string          `json:"notificationType,omitempty"`
	IsInterrupt      bool            `json:"isInterrupt,omitempty"`
}

// Decision is a single hook invocation's verdict.
type Decision string

// Outcome records one hook invocation.
type Outcome struct {
	Hook      ResolvedHook
	Decision  Decision
	ExitCode  int // -1 when unknown (killed / spawn error)
	Stdout    string
	Stderr    string
	TimedOut  bool
	Truncated bool
	Duration  time.Duration
}

// Report aggregates the outcomes of running an event's hooks.
type Report struct {
	Event    Event
	Outcomes []Outcome
	Blocked  bool // at least one outcome blocked (only meaningful on gating events)
	// Allowed is set when a Claude-imported PermissionRequest hook returned an
	// explicit JSON "allow" decision on exit 0 (see claudeJSONAllow) — the
	// caller should treat this as an auto-approval instead of prompting.
	Allowed bool
}

// HookOutput is the parsed, model-facing part of a successful hook stdout.
type HookOutput struct {
	AdditionalContext string
	// Deny and DenyReason carry a Claude-style JSON deny decision returned on
	// exit 0: hookSpecificOutput.permissionDecision for PreToolUse,
	// hookSpecificOutput.decision.behavior for PermissionRequest, or a
	// top-level decision:"block" for UserPromptSubmit. Claude hooks commonly
	// deny this way instead of exiting 2; see
	// https://code.claude.com/docs/en/hooks.
	Deny       bool
	DenyReason string
	// Allow carries a Claude PermissionRequest "allow" decision
	// (hookSpecificOutput.decision.behavior == "allow"): the hook answers the
	// permission dialog on the user's behalf instead of only observing it.
	Allow bool
}

type hookJSONOutput struct {
	// Decision and Reason are UserPromptSubmit's (and Stop/SubagentStop's)
	// top-level deny shape: {"decision":"block","reason":"..."}.
	Decision           string `json:"decision"`
	Reason             string `json:"reason"`
	HookSpecificOutput struct {
		HookEventName            Event  `json:"hookEventName"`
		AdditionalContext        string `json:"additionalContext"`
		PermissionDecision       string `json:"permissionDecision"`
		PermissionDecisionReason string `json:"permissionDecisionReason"`
		Decision                 struct {
			Behavior string `json:"behavior"`
		} `json:"decision"`
	} `json:"hookSpecificOutput"`
}

// ParseOutput extracts hook-specific context from stdout. Plain text is accepted
// for SessionStart compatibility; JSON output must identify the current event.
func ParseOutput(event Event, stdout string) (HookOutput, []string) {
	stdout = strings.TrimSpace(stdout)
	if stdout == "" {
		return HookOutput{}, nil
	}
	if !strings.HasPrefix(stdout, "{") {
		if event == SessionStart {
			return HookOutput{AdditionalContext: stdout}, nil
		}
		return HookOutput{}, nil
	}
	var parsed hookJSONOutput
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return HookOutput{}, []string{fmt.Sprintf("hook %s returned invalid JSON stdout: %v", event, err)}
	}
	spec := parsed.HookSpecificOutput
	topLevelDeny := event == UserPromptSubmit && strings.EqualFold(parsed.Decision, "block")
	deny := strings.EqualFold(spec.PermissionDecision, "deny") || strings.EqualFold(spec.Decision.Behavior, "deny") || topLevelDeny
	allow := event == PermissionRequest && strings.EqualFold(spec.Decision.Behavior, "allow")
	if spec.HookEventName == "" && strings.TrimSpace(spec.AdditionalContext) == "" && !deny && !allow {
		return HookOutput{}, nil
	}
	if spec.HookEventName != "" && spec.HookEventName != event {
		return HookOutput{}, []string{fmt.Sprintf("hook output event %q does not match current event %q", spec.HookEventName, event)}
	}
	out := HookOutput{AdditionalContext: strings.TrimSpace(spec.AdditionalContext)}
	if deny {
		out.Deny = true
		reason := spec.PermissionDecisionReason
		if topLevelDeny {
			reason = parsed.Reason
		}
		out.DenyReason = strings.TrimSpace(reason)
	}
	out.Allow = allow
	return out, nil
}

// decideOutcome maps a spawn result to a verdict for hook h.
func decideOutcome(h ResolvedHook, r SpawnResult) Decision {
	blocking := IsBlocking(h.Event) || claudePermissionBlocking(h)
	switch {
	case r.SpawnErr != nil:
		return DecisionError
	case r.TimedOut:
		if blocking {
			return DecisionBlock
		}
		return DecisionWarn
	case r.ExitCode == 0:
		return DecisionPass
	case r.ExitCode == 2 && blocking:
		return DecisionBlock
	default:
		return DecisionWarn
	}
}

// claudeJSONDeny reports whether a Claude-format hook's exit-0 stdout still
// carries a JSON deny decision (see HookOutput.Deny). Reasonix must honor it
// for the events it claims Claude hook compatibility for, or a plugin's
// "block this dangerous command" hook silently no-ops whenever the script
// signals deny via JSON instead of exit code 2. UserPromptSubmit uses a
// top-level decision:"block" instead of PreToolUse/PermissionRequest's
// hookSpecificOutput shape; ParseOutput handles both.
func claudeJSONDeny(event Event, stdout string) (bool, string) {
	if event != PreToolUse && event != PermissionRequest && event != UserPromptSubmit {
		return false, ""
	}
	out, _ := ParseOutput(event, stdout)
	return out.Deny, out.DenyReason
}

// claudeJSONAllow reports whether a Claude-format PermissionRequest hook's
// exit-0 stdout carries an explicit "allow" decision
// (hookSpecificOutput.decision.behavior == "allow"): the hook answers the
// permission dialog on the user's behalf, same as an exit-2 deny preempts it.
func claudeJSONAllow(event Event, stdout string) bool {
	if event != PermissionRequest {
		return false
	}
	out, _ := ParseOutput(event, stdout)
	return out.Allow
}
