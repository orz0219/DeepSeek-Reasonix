package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"reasonix/internal/event"
	"reasonix/internal/jobs"
	"reasonix/internal/planmode"
)

// maxToolOutputBytes caps a single tool result before it goes into the model's
// context. ~32KB is roughly 8K tokens — enough for a full file read or a busy
// grep, while preventing one accidental "read this 5 MB log" from blowing the
// window before the next compaction runs.
const maxToolOutputBytes = 32 * 1024

const maxEmptyFinalBlocks = 3

// maxStreamRecoveries is the number of body-phase stream retries after the
// initial sampling attempt (Codex-aligned default: 1 + 5 = 6 attempts total).
const maxStreamRecoveries = 5
const maxSamplingAttempts = maxStreamRecoveries + 1
const maxExecutorHandoffNudges = 1

const defaultReasoningByteLimit = 128 * 1024

const finishReasonClientReasoningLimit = "client_reasoning_limit"

var errReasoningByteLimitExceeded = errors.New("reasoning output exceeded client byte limit")

// DeliveryRuntimeMarker is the delivery-mode contract block appended to user
// turns (withTurnPreferences). Exported as the single source of truth for the
// byte-exact suffix strip in preview derivation and for cross-package tests;
// its text is cache-frozen — changing it breaks steer replay matching and the
// prefix stability of every live delivery session.
const DeliveryRuntimeMarker = `<delivery-runtime>
This session is in delivery-first mode. Before any state-changing tool call,
establish concrete, verifiable acceptance criteria with todo_write. After the
change, inspect the result, run relevant verification, and sign off each step
with complete_step citing the successful verification command. The host enforces
these gates and will reject mutation or finalization when evidence is missing.
</delivery-runtime>`

// Renderer redraws the assistant's final-answer text as styled output. It is
// applied only after a turn's text stream completes, so the user sees raw
// markdown stream live, then a single redraw replaces it with formatted
// output. The renderer is intentionally interface-shaped so the agent stays
// independent of the cli's markdown library choice. Consumed by TextSink.
type Renderer interface {
	Render(text string) string
}

// Asker puts structured multiple-choice questions to the user and blocks for the
// answers. The agent consults it for the `ask` tool. It is interface-shaped so
// the agent stays independent of the frontend; a nil asker means no interactive
// user (headless runs), where `ask` returns a "decide for yourself" result. The
// interactive frontends wire the controller in as the Asker.
type Asker interface {
	Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error)
}

// callContextKey carries the executing tool call's identity into Execute.
type callContextKey struct{}
type parentSessionContextKey struct{}
type subagentDepthContextKey struct{}
type userImagesContextKey struct{}

// callContext is the per-call context a tool can read. parentID is the call being
// executed and sink is the agent's event sink (the `task` tool uses both to nest
// a sub-agent's events under this call); asker lets the `ask` tool reach the user.
type callContext struct {
	parentID string
	sink     event.Sink
	asker    Asker
	planMode bool
}

// withCallContext stamps ctx with the executing call's ID, the agent's sink, and
// the asker. executeOne sets this before every Execute; `task` reads it (via
// CallContext) to nest sub-agent events, and `ask` reads the asker to prompt.
// The plan-mode flag is mirrored onto the leaf planmode key so tools that must
// not import this package (for example internal/tool/builtin) can still read it.
func withCallContext(ctx context.Context, parentID string, sink event.Sink, asker Asker, planMode bool) context.Context {
	ctx = planmode.WithActive(ctx, planMode)
	return context.WithValue(ctx, callContextKey{}, callContext{parentID: parentID, sink: sink, asker: asker, planMode: planMode})
}

// WithToolCallContext stamps ctx as a host-initiated top-level tool call.
// Normal model-selected tools receive this context from executeOne; controller
// entry points that deliberately invoke the same tool machinery (for example a
// user typing /<subagent-skill>) use this exported wrapper so nested sub-agent
// activity still reaches the parent event stream and plan-mode policy remains
// visible to the invoked runner.
func WithToolCallContext(ctx context.Context, parentID string, sink event.Sink, asker Asker, planMode bool) context.Context {
	return withCallContext(ctx, parentID, sink, asker, planMode)
}

// CallContext returns the executing call's ID, the agent's sink, and the asker,
// if the context was set by an agent's executeOne. ok is false for a plain
// context (headless tool tests, calls made outside the run loop).
func CallContext(ctx context.Context) (parentID string, sink event.Sink, asker Asker, ok bool) {
	cc, ok := ctx.Value(callContextKey{}).(callContext)
	if !ok {
		return "", nil, nil, false
	}
	return cc.parentID, cc.sink, cc.asker, true
}

// PlanModeFromContext reports whether the tool call is executing during the
// plan-first workflow. Tools may use it for phase-specific behavior, but it is
// not a permission or read-only boundary.
func PlanModeFromContext(ctx context.Context) bool {
	cc, ok := ctx.Value(callContextKey{}).(callContext)
	return ok && cc.planMode
}

// withAgentContext establishes the agent-owned workflow capabilities for a
// model round and for tool availability checks. Missing capabilities shadow
// inherited values so child agents cannot reach parent Goal, Jobs, or memory
// state accidentally.
func (a *Agent) withAgentContext(ctx context.Context) context.Context {
	if a == nil {
		return ctx
	}
	if a.svc.jobs != nil {
		ctx = jobs.WithManager(ctx, a.svc.jobs)
	} else {
		ctx = jobs.WithoutManager(ctx)
	}
	return planmode.WithActive(ctx, a.planMode.Load())
}

// WithParentSession stamps the active parent session ID onto a turn context so
// persisted sub-agents can record and enforce their owning conversation.
func WithParentSession(ctx context.Context, parentSession string) context.Context {
	return context.WithValue(ctx, parentSessionContextKey{}, strings.TrimSpace(parentSession))
}

// ParentSession returns the active parent session ID carried by a turn context.
func ParentSession(ctx context.Context) string {
	parentSession, _ := ctx.Value(parentSessionContextKey{}).(string)
	return strings.TrimSpace(parentSession)
}

// WithSubagentDepth carries the current subagent depth through nested tool calls.
// The root agent runs at depth 0; each spawned subagent increments by one.
func WithSubagentDepth(ctx context.Context, depth int) context.Context {
	if depth < 0 {
		depth = 0
	}
	return context.WithValue(ctx, subagentDepthContextKey{}, depth)
}

// SubagentDepth returns the current subagent depth carried by a turn context.
func SubagentDepth(ctx context.Context) int {
	depth, _ := ctx.Value(subagentDepthContextKey{}).(int)
	if depth < 0 {
		return 0
	}
	return depth
}

// WithUserImages carries the data URLs of images the user attached to this turn,
// resolved by the controller (which owns attachments) since the agent must not
// depend on it. Run embeds them on the user message; the provider sends them only
// when the model is vision-capable.
func WithUserImages(ctx context.Context, images []string) context.Context {
	return context.WithValue(ctx, userImagesContextKey{}, images)
}

func userImages(ctx context.Context) []string {
	images, _ := ctx.Value(userImagesContextKey{}).([]string)
	return images
}

// Gate decides, per tool call, whether it may run. The agent consults it at
// execute time after any explicit planning-phase opt-out. It is interface-shaped so the agent
// stays independent of the permission package and of how "ask" is resolved
// (silently in headless runs, interactively in the chat TUI). A nil gate means
// no gating — every call runs, preserving behaviour for callers that don't wire
// one in. reason is fed back to the model when allow is false; a non-nil err
// (e.g. ctx cancelled awaiting approval) is treated as a block for that call.
type Gate interface {
	Check(ctx context.Context, toolName string, args json.RawMessage, readOnly bool) (allow bool, reason string, err error)
}

// ExplicitDenyGate exposes the only global permission decision that applies to
// an already-authorized MCP server. Installing or approving a server is the
// user's authorization boundary; ordinary ask/fallback posture must not add a
// second per-call prompt, while explicit deny rules remain authoritative.
type ExplicitDenyGate interface {
	ExplicitlyDenies(toolName string, args json.RawMessage) bool
}

const PlanModeReadOnlyCommandApprovalTool = "plan_mode_read_only_command"

// PlanModeReadOnlyTrustRequest describes a bash command that is safe enough to
// ask the user to accept as read-only during planning. Command is the concrete
// attempted command and Prefix is the reusable prefix to trust.
type PlanModeReadOnlyTrustRequest struct {
	ToolName string
	Command  string
	Prefix   string
	Args     json.RawMessage
}

// PlanModeReadOnlyTrustGate is the legacy Plan bash trust bridge. It remains in
// the internal API for controller compatibility, but ordinary Plan execution no
// longer invokes it; bash calls use the normal permission gate.
type PlanModeReadOnlyTrustGate interface {
	CheckPlanModeReadOnlyTrust(ctx context.Context, req PlanModeReadOnlyTrustRequest) (allow bool, reason string, err error)
}

const DefaultMaxSubagentDepth = 2

// NormalizeMaxSubagentDepth applies the public config contract: values below 1
// preserve the old single-delegation boundary.
func NormalizeMaxSubagentDepth(depth int) int {
	if depth < 1 {
		return 1
	}
	return depth
}
