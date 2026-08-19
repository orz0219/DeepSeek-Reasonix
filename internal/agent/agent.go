package agent

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"

	"reasonix/internal/ablation"
	"reasonix/internal/capability"
	"reasonix/internal/checkpoint"
	"reasonix/internal/diff"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/instruction"
	"reasonix/internal/memory"
	"reasonix/internal/nilutil"
	"reasonix/internal/plancontract"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// maxToolOutputBytes caps a single tool result before it goes into the model's
// context. ~32KB is roughly 8K tokens — enough for a full file read or a busy
// grep, while preventing one accidental "read this 5 MB log" from blowing the
// window before the next compaction runs.

// maxStreamRecoveries is the number of body-phase stream retries after the
// initial sampling attempt (Codex-aligned default: 1 + 5 = 6 attempts total).

// DeliveryRuntimeMarker is the delivery-mode contract block appended to user
// turns (withTurnPreferences). Exported as the single source of truth for the
// byte-exact suffix strip in preview derivation and for cross-package tests;
// its text is cache-frozen — changing it breaks steer replay matching and the
// prefix stability of every live delivery session.

// Renderer redraws the assistant's final-answer text as styled output. It is
// applied only after a turn's text stream completes, so the user sees raw
// markdown stream live, then a single redraw replaces it with formatted
// output. The renderer is intentionally interface-shaped so the agent stays
// independent of the cli's markdown library choice. Consumed by TextSink.

// Asker puts structured multiple-choice questions to the user and blocks for the
// answers. The agent consults it for the `ask` tool. It is interface-shaped so
// the agent stays independent of the frontend; a nil asker means no interactive
// user (headless runs), where `ask` returns a "decide for yourself" result. The
// interactive frontends wire the controller in as the Asker.

// callContextKey carries the executing tool call's identity into Execute.

// callContext is the per-call context a tool can read. parentID is the call being
// executed and sink is the agent's event sink (the `task` tool uses both to nest
// a sub-agent's events under this call); asker lets the `ask` tool reach the user.

// withCallContext stamps ctx with the executing call's ID, the agent's sink, and
// the asker. executeOne sets this before every Execute; `task` reads it (via
// CallContext) to nest sub-agent events, and `ask` reads the asker to prompt.
// The plan-mode flag is mirrored onto the leaf planmode key so tools that must
// not import this package (for example internal/tool/builtin) can still read it.

// WithToolCallContext stamps ctx as a host-initiated top-level tool call.
// Normal model-selected tools receive this context from executeOne; controller
// entry points that deliberately invoke the same tool machinery (for example a
// user typing /<subagent-skill>) use this exported wrapper so nested sub-agent
// activity still reaches the parent event stream and plan-mode policy remains
// visible to the invoked runner.

// CallContext returns the executing call's ID, the agent's sink, and the asker,
// if the context was set by an agent's executeOne. ok is false for a plain
// context (headless tool tests, calls made outside the run loop).

// PlanModeFromContext reports whether the tool call is executing during the
// plan-first workflow. Tools may use it for phase-specific behavior, but it is
// not a permission or read-only boundary.

// withAgentContext establishes the agent-owned workflow capabilities for a
// model round and for tool availability checks. Missing capabilities shadow
// inherited values so child agents cannot reach parent Goal, Jobs, or memory
// state accidentally.

// WithParentSession stamps the active parent session ID onto a turn context so
// persisted sub-agents can record and enforce their owning conversation.

// ParentSession returns the active parent session ID carried by a turn context.

// WithSubagentDepth carries the current subagent depth through nested tool calls.
// The root agent runs at depth 0; each spawned subagent increments by one.

// SubagentDepth returns the current subagent depth carried by a turn context.

// WithUserImages carries the data URLs of images the user attached to this turn,
// resolved by the controller (which owns attachments) since the agent must not
// depend on it. Run embeds them on the user message; the provider sends them only
// when the model is vision-capable.

// Gate decides, per tool call, whether it may run. The agent consults it at
// execute time after any explicit planning-phase opt-out. It is interface-shaped so the agent
// stays independent of the permission package and of how "ask" is resolved
// (silently in headless runs, interactively in the chat TUI). A nil gate means
// no gating — every call runs, preserving behaviour for callers that don't wire
// one in. reason is fed back to the model when allow is false; a non-nil err
// (e.g. ctx cancelled awaiting approval) is treated as a block for that call.

// ExplicitDenyGate exposes the only global permission decision that applies to
// an already-authorized MCP server. Installing or approving a server is the
// user's authorization boundary; ordinary ask/fallback posture must not add a
// second per-call prompt, while explicit deny rules remain authoritative.

// PlanModeReadOnlyTrustRequest describes a bash command that is safe enough to
// ask the user to accept as read-only during planning. Command is the concrete
// attempted command and Prefix is the reusable prefix to trust.

// PlanModeReadOnlyTrustGate is the legacy Plan bash trust bridge. It remains in
// the internal API for controller compatibility, but ordinary Plan execution no
// longer invokes it; bash calls use the normal permission gate.

// NormalizeMaxSubagentDepth applies the public config contract: values below 1
// preserve the old single-delegation boundary.

// ToolHooks fires user-configured shell hooks around each tool call. PreToolUse
// runs before the call and may block it (block=true; message is the reason fed
// back to the model); PostToolUse runs after and only surfaces output to the
// user (it can't block). It is interface-shaped so the agent stays independent
// of the hook package — a nil hooks field disables hook firing entirely.

// PostLLMCall fires after each model turn completes (streaming finishes)
// but before reasoning_content is stored. It returns the (possibly
// translated) reasoning string — the original when no hook is configured.
// HasPostLLMCall reports whether such a hook exists, so the agent keeps
// streaming reasoning live when none is wired up.

// SubagentStop fires when a `task` sub-agent finishes (foreground). PreCompact
// fires just before a compaction pass and returns extra summary guidance (its
// hooks' stdout) to fold into the summary prompt; "" when no hook contributes.

// Agent drives a single task: a Provider, a tool Registry, and a Session wired
// into the main loop.
type Agent struct {
	agentConfig
	// svc are the collaborators this agent talks to; see services.go.
	svc agentServices
	// sess is the state one conversation owns; SetSession restarts it. See
	// sessionstate.go.
	sess sessionRuntime
	// executorHandoffGuard is enabled by Coordinator only for the executor agent.
	executorHandoffGuard bool
	responseLanguage     atomic.Value // string: auto|zh|en
	reasoningLanguage    atomic.Value // string: auto|zh|en

	requireVisibleFinal bool // internal callers require final Content

	// unwrittenResolve is the resolve watermark a failed state write still owes.
	// It outlives the conversation, which is why it is not in sessionRuntime.
	unwrittenResolve unwrittenResolve

	// planMode enables planning workflow instructions and explicit phase opt-outs.
	// It does not replace the permission or sandbox boundary. The system prompt and
	// tool list never change with the toggle, preserving the provider-cache prefix.
	planMode atomic.Bool

	// readOnlyExecution is a construction-time defense for planner/research
	// agents. Unlike planMode it is not a collaboration toggle: it remains on
	// for the agent's lifetime and validates proxy calls after resolution.
	readOnlyExecution bool

	// mutationDependencyBarrier is set for the remainder of a provider tool
	// batch after any mutating call fails or is blocked. executeOne re-checks
	// it after proxy resolution so use_capability cannot bypass the barrier by
	// advertising schema-level ReadOnly()==true. Parallel read-only segments
	// never set it. Cleared at the start of each executeBatch.
	mutationDependencyBarrier atomic.Bool

	// plannerMCPExecution relaxes the strict read-only MCP boundary for the
	// two-model Planner only: authorized, non-destructive MCP targets may run
	// through use_capability even without readOnlyHint. Ordinary writers, bash,
	// and destructive MCP stay blocked. Strict read-only sub-agents leave this
	// false and still require readOnlyHint.
	plannerMCPExecution bool

	// recovery is who this agent is to the shared gate above.
	recovery recoveryIdentity

	// writeWorkspaceRoot is the workspace used to normalize parent write
	// reservations when writeScheduler is set.

	// steerQueue holds mid-turn guidance admitted while the agent is running.
	// Entries keep a durable inbox item ID plus a loader so full bodies are not
	// retained in the agent heap beyond need. Cache miss for the next API call
	// is unavoidable but limited to one call — the prefix stays stable otherwise.
	steerMu       sync.Mutex
	steerQueue    []steerEntry
	steerConsumed bool
	// steerRunActive is true while Run is executing. Steer only queues while
	// it is set; once the turn's exit flush has drained the queue, later
	// steers are rejected so the caller can deliver them as a regular turn
	// instead of leaving them in a queue no loop will ever consume.
	steerRunActive bool

	// task is the state shared by every Run continuing one delivery scope: the
	// receipt ledger complete_step validates citations against, the spend that
	// outlives a single Run, and the guards keyed to the task rather than the
	// turn. See taskstate.go.
	task taskRuntime

	planContract *plancontract.Plan // approved plan this turn executes, if any

	// hostAdvanceSeq guarantees unique tool IDs across turns: every
	// emitTodoState call increments it so the frontend always sees a fresh
	// dispatch even when the same panel index is signed off in different turns.
	hostAdvanceSeq atomic.Int64
	// lastTrace holds the most recent per-call trace for benchmarking.
	lastTrace atomic.Pointer[toolCallTrace]

	// projectChecks are structured project instructions that complete_step can
	// verify against same-turn bash receipts after a write-backed completion.
	projectChecks []instruction.VerifyCheck

	// toolContextBase is a pre-built context carrying session-level values
	// (ledger, jobs, sandbox, memory, etc.) so prepareToolExecution only adds
	// per-call values. Rebuilt when session-level state changes.
	toolContextBase context.Context

	// deliveryProfile enables the runtime-enforced delivery contract. The stable
	// profile prompt explains intent; this is host state and never enters the
	// provider-cached prefix. The scope ID and checkpoint it works against live
	// in task; the per-turn expectations live in turn.
	// When agentPreset is set, deliveryProfile is derived for baseline Delivery
	// and may be elevated per-turn by TaskPolicy (e.g. Light high-risk).
	deliveryProfile bool

	// agentPreset is the session role setting. Atomic so SetAgentPreset can
	// update subsequent turns without rebuilding the agent.
	agentPreset atomic.Value // string light|balanced|delivery

	// turn is the state of the Run currently executing; beginRunTurn replaces
	// it wholesale. See turnruntime.go.
	turn turnRuntime

	// ablation names the subsystems a benchmark arm switched off. The zero value
	// is the control arm.
	ablation ablation.Set

	// pending is what an external caller arms before the next Run; see
	// turnruntime.go.
	pending pendingTurn

	// capabilityLedger tracks require/prefer outcomes for this user turn only.
	// Never serialized into prompts or session state.
	capabilityLedger *capability.Ledger
	// capabilityAudit accumulates non-persisted routing/proxy counters.
	capabilityAudit *capability.Audit
	// capabilityGate is the turn's gate memory across final-answer retries.
	capabilityGate capabilityGateState

	// subagentDepth tracks the current agent's nesting depth. maxSubagentDepth
	// caps delegation; when reached, recursive agent/skill tools are excluded.

	// Context management keeps the canonical transcript immutable and installs
	// at most one provider-visible checkpoint each time compactRatio is crossed.
	keepPolicy             KeepPolicy
	strictAlternatingRoles bool // coalesce adjacent user turns on provider request copies
	// activeTurnCreatedAt identifies the real/synthetic user message that began
	// the currently running turn. Compaction may rewrite older history while a
	// tool loop is active, but it must keep this message and everything after it
	// verbatim so cancellation/crash recovery can retain completed tool pairs.
	activeTurnCreatedAt atomic.Int64
}

// KeepPolicy is a bitmask controlling which messages are preserved beyond the
// recent tail during compaction.

// SetPlanMode toggles the plan-first workflow flag. Ordinary calls still use
// Permissions/Sandbox; only explicit phase opt-outs are refused. The system
// prompt and tool schemas stay untouched, while the caller supplies the
// model-facing Marker in a user turn.
func (a *Agent) SetPlanMode(v bool) { a.planMode.Store(v) }

// SetTools replaces the agent's tool registry. The next API call picks up the
// new tool schema; tools already cached in the provider prefix are unaffected
// until the prefix is invalidated. Safe to call between turns.
func (a *Agent) SetTools(tools *tool.Registry) {
	if a == nil {
		return
	}
	a.svc.tools = tools
}

// SetReasoningLanguage updates the visible reasoning language preference for
// subsequent user-role messages emitted by this agent.
func (a *Agent) SetReasoningLanguage(lang string) {
	if a == nil {
		return
	}
	a.reasoningLanguage.Store(NormalizeReasoningLanguage(lang))
}

// SetResponseLanguage updates the final-answer language preference for
// subsequent user-role messages emitted by this agent.
func (a *Agent) SetResponseLanguage(lang string) {
	if a == nil {
		return
	}
	a.responseLanguage.Store(NormalizeResponseLanguage(lang))
}

// SetGate installs the per-call permission gate. Used by interactive CLI sessions to swap the
// headless gate built in setup for an interactive one that prompts the user;
// nil disables gating. Safe to call before the run loop starts.
func (a *Agent) SetGate(g Gate) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.svc.gate = g
}

// SetExtensions installs the extension dispatcher after construction. Boot
// uses it because sidecars — and therefore the dispatcher — only exist after
// snapshot assembly, which runs after the agent is built. Safe to call before
// the run loop starts; nil disables interception.
func (a *Agent) SetExtensions(d *dispatch.Dispatcher) {
	if a == nil {
		return
	}
	a.svc.extensions = d
}

// SetRecoveryGate installs Auto Guard. Safe to call before the run loop starts;
// nil disables its checks.
func (a *Agent) SetRecoveryGate(g RecoveryGate) {
	if a == nil {
		return
	}
	if nilutil.IsNil(g) {
		g = nil
	}
	a.svc.recoveryGate = g
}

// SetRecoveryIdentity sets the agent/task labels used on recovery cards.
func (a *Agent) SetRecoveryIdentity(agentID, taskID string) {
	if a == nil {
		return
	}
	a.recovery.agentID = strings.TrimSpace(agentID)
	a.recovery.taskID = strings.TrimSpace(taskID)
}

// RecoveryGate returns the attached Auto Guard (may be nil).
func (a *Agent) RecoveryGate() RecoveryGate {
	if a == nil {
		return nil
	}
	return a.svc.recoveryGate
}

// SetPlanModeReadOnlyTrustGate retains the legacy confirmation bridge for old
// controller/session data. Main Plan execution no longer calls it.
func (a *Agent) SetPlanModeReadOnlyTrustGate(g PlanModeReadOnlyTrustGate) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.svc.planTrust = g
}

// SetSandboxEscapeApprover installs the optional one-shot approval path used by
// the bash tool when an enforced OS sandbox fails to start.
func (a *Agent) SetSandboxEscapeApprover(g sandbox.EscapeApprover) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.svc.sandboxEscape = g
}

// SetConfigWriteApprover installs the optional per-write approval path used by
// the file tools when a target is a Reasonix-managed config file outside the
// workspace write roots.
func (a *Agent) SetConfigWriteApprover(g tool.ConfigWriteApprover) {
	if nilutil.IsNil(g) {
		g = nil
	}
	a.svc.configWrite = g
}

func (a *Agent) withTurnPreferences(input string) string {
	if a == nil {
		return input
	}
	responseLang := "auto"
	if v := a.responseLanguage.Load(); v != nil {
		if s, ok := v.(string); ok {
			responseLang = s
		}
	}
	input = WithResponseLanguage(input, responseLang)

	lang := "auto"
	if v := a.reasoningLanguage.Load(); v != nil {
		if s, ok := v.(string); ok {
			lang = s
		}
	}
	input = WithReasoningLanguage(input, lang)
	// Role settings no longer inject a stable delivery-runtime system-like
	// marker. Per-turn <execution-policy> carries the frozen role setting.
	return input
}

// SetAsker installs the asker the `ask` tool uses to question the user.
// Interactive frontends wire one in; headless runs leave it nil.
func (a *Agent) SetAsker(as Asker) { a.svc.asker = as }

// SetMemoryQueue installs the sink the remember/forget tools use to apply a
// memory change in the current session. The controller wires itself in.
func (a *Agent) SetMemoryQueue(q memory.Queue) { a.svc.memQueue = q }

// SetPreEditHook installs the pre-edit snapshot hook (see onPreEdit). The
// controller wires it to its per-session checkpoint store; nil disables capture.
// Prefer SetMutationObserver for v2 capture (before+after fingerprints).
func (a *Agent) SetPreEditHook(fn func(diff.Change)) { a.svc.preEdit = fn }

// SetMutationObserver installs the unified mutation observer. When set, it
// supersedes onPreEdit for capture and also records after-mutation fingerprints.
// When a task tool is already registered it inherits the observer for sub-agents.
func (a *Agent) SetMutationObserver(obs *checkpoint.MutationObserver) {
	a.svc.mutationObserver = obs
	if a.svc.tools == nil || obs == nil {
		return
	}
	if t, ok := a.svc.tools.Get("task"); ok {
		if task, ok := t.(*TaskTool); ok {
			task.WithMutationObserver(obs)
		}
	}
}

// MutationObserver returns the installed observer (may be nil).
func (a *Agent) MutationObserver() *checkpoint.MutationObserver {
	if a == nil {
		return nil
	}
	return a.svc.mutationObserver
}

// Session returns the agent's current conversation, useful for persistence
// hooks that need to read the message log between turns. sessMu serialises this
// pointer read against SetSession, so a frontend (serve's concurrent /history and
// /new handlers) can't race the swap. The run loop touches a.session directly and
// only swaps it via SetSession while idle, so its reads need no lock.
func (a *Agent) Session() *Session {
	a.sess.mu.Lock()
	defer a.sess.mu.Unlock()
	return a.sess.conversation
}

// SetSession replaces the agent's conversation wholesale. Used by
// `reasonix --resume` to load a saved JSONL transcript before the first turn,
// so the model picks up exactly where it left off. Callers serialise it against a
// running turn (it only fires while idle); sessMu guards the pointer swap itself.
func (a *Agent) SetSession(s *Session) {
	a.sess.reset(s)
	// The replaced conversation's task is over, but the ledger and the bill
	// answer to beginRunTurn's scope check rather than to this seam.
	a.task.repeatFailures = nil
	a.task.repeatScope = ""
	if s != nil {
		a.rebuildTodoState(s.Snapshot())
	}
}

// LastUsage returns the most recent per-turn token telemetry the provider
// reported (nil if no turn has run yet). The TUI uses it to show a context
// gauge alongside the prompt; ContextManager.Prepare owns cache-breaking
// maintenance decisions.
func (a *Agent) LastUsage() *provider.Usage { return a.sess.output.lastUsage.Load() }

// SessionCache returns the cumulative cache hit/miss prompt tokens across every
// API call this session — the basis for the status line's aggregate hit-rate.
func (a *Agent) SessionCache() (hit, miss int) {
	return int(a.sess.cacheHit.Load()), int(a.sess.cacheMiss.Load())
}

// ContextWindow returns the configured context-window size in tokens. 0
// means compaction is disabled for this agent.
func (a *Agent) ContextWindow() int { return a.contextWindow }

// mid-turn steer marker.
// MidTurnSteerPrefix marks user messages that were injected mid-turn as
