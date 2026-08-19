package agent

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"reasonix/internal/ablation"
	"reasonix/internal/agentpreset"
	"reasonix/internal/capability"
	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/instruction"
	"reasonix/internal/jobs"
	"reasonix/internal/nilutil"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/taskpolicy"
	"reasonix/internal/tool"
	"reasonix/internal/workspacelease"
)

// CompactRatio returns the fraction of the window at which auto-compaction
// fires (e.g. 0.8). The status line uses it to show headroom to the next compact.
func (a *Agent) CompactRatio() float64 { return a.compactRatio }

// CompactNow forces one projection compaction (canonical transcript untouched).
func (a *Agent) CompactNow(ctx context.Context, instructions string) error {
	_, err := a.contextManager().Prepare(ctx, ContextPreparePolicy{Trigger: CompactionTriggerManual, Instructions: instructions, Force: true})
	return err
}

// Options configures an Agent.
type Options struct {
	MaxSteps int
	// MaxStepsKey names the explicit runtime control shown when the MaxSteps guard
	// is hit. Empty defaults to the generic max_steps tool/runtime parameter.
	MaxStepsKey string
	// ReasoningByteLimit bounds a single stream's hidden reasoning bytes. Zero
	// uses the default guard; a negative value disables only this client guard.
	// Provider output budgets are a separate protocol/model capability.
	ReasoningByteLimit int
	// MaxOutputTokens overrides the provider's configured/default total output
	// budget. Zero delegates to the provider; a negative value asks optional
	// protocols to omit the budget (Anthropic still requires max_tokens).
	MaxOutputTokens int
	Temperature     float64
	// TaskBudget bounds a task's spend; zero uses DefaultTaskBudget.
	TaskBudget  TaskBudget
	Pricing     *provider.Pricing // optional, for per-turn cost display
	UsageSource string            // optional billable usage source; default executor
	// ModelRef names the canonical "provider/model" ref backing this agent's
	// provider instance. It is attached to emitted Usage events so downstream
	// usage accounting can attribute tokens to the exact model.
	ModelRef string
	// RequireVisibleFinal makes internal callers reject reasoning-only responses.
	RequireVisibleFinal bool
	// Gate is the per-call permission gate. nil disables gating.
	Gate Gate
	// ReadOnlyExecution enables a permanent host-side read-only boundary for
	// planner and research agents. It is intentionally independent of Plan mode
	// so a stale collaboration flag cannot authorize a dynamic writer target.
	ReadOnlyExecution bool
	// PlannerMCPExecution enables Planner-trusted MCP through use_capability:
	// authorized, non-destructive tools may run without readOnlyHint. Only
	// NewPlannerAgent sets this; strict read-only sub-agents must not.
	PlannerMCPExecution bool

	// PlanModeReadOnlyTrustGate is retained for legacy controller compatibility.
	// The main Plan execution path no longer invokes it.
	PlanModeReadOnlyTrustGate PlanModeReadOnlyTrustGate

	// SandboxEscapeApprover confirms a one-shot unconfined shell rerun after an
	// enforced OS sandbox fails. nil keeps fail-closed behavior.
	SandboxEscapeApprover sandbox.EscapeApprover

	// ConfigWriteApprover confirms file-tool writes to Reasonix-managed config
	// files outside the workspace roots. nil keeps fail-closed behavior.
	ConfigWriteApprover tool.ConfigWriteApprover

	// Context management. ContextWindow <= 0 disables compaction. Ratios and
	// RecentKeep fall back to defaults when unset.
	ContextWindow int
	CompactRatio  float64
	// Deprecated compatibility inputs. New agents ignore these fields; automatic
	// maintenance is controlled only by CompactRatio.
	SoftCompactRatio       float64
	ToolResultSnipRatio    float64
	CompactForceRatio      float64
	RecentKeep             int
	ArchiveDir             string
	KeepPolicy             KeepPolicy
	SessionPath            string // projection sidecar path; empty = memory only
	WorkspaceID            string // prompt-cache lineage component
	StrictAlternatingRoles bool   // merge adjacent user turns for strict providers at request time
	ContextEditing         string // deprecated; native provider editing was removed

	// Hooks fires PreToolUse / PostToolUse shell hooks around tool calls. nil
	// disables hook firing.
	Hooks ToolHooks

	// MissingReasoningWarnStateDir, when non-empty, points at the shared
	// directory where missing tool-call thinking recovery retries are gated by
	// opaque provider-configuration fingerprint (#7059). The field name is kept
	// for source compatibility. Boot always supplies it; direct construction
	// with an empty value keeps in-memory gating.
	MissingReasoningWarnStateDir string

	// Jobs is the session's background-job manager (nil disables background tools).
	Jobs *jobs.Manager

	// WriteScheduler is the session-scoped subagent concurrency/write-claim
	// controller. When set on the parent executor, write-capable tools reserve
	// paths for the duration of Execute so background writers cannot TOCTOU
	// race parent writes. Subagents leave this nil (or depth > 0 skips it).
	WriteScheduler *SubagentScheduler
	// WriteWorkspaceRoot normalizes parent write reservations.
	WriteWorkspaceRoot string

	// WorkspaceLease serializes Delivery mutations across sessions that target
	// the same workspace. nil preserves source compatibility for direct Agent
	// construction; boot always supplies it for Delivery sessions.
	WorkspaceLease *workspacelease.Owner

	// ProjectChecks are host-observable structured checks extracted during boot.
	ProjectChecks []instruction.VerifyCheck

	// DeliveryProfile enforces acceptance criteria before mutations and requires
	// post-change review, verification, and evidence-backed sign-off before a
	// final answer. It changes host control flow, not tool schemas.
	// Deprecated: prefer AgentPreset + turn TaskPolicy. Still honored when
	// AgentPreset is empty for one compatibility version of direct constructors.
	DeliveryProfile bool

	// AgentPreset is the session role setting (light|balanced|delivery). Empty
	// falls back to balanced unless DeliveryProfile is true (then delivery).
	// Switching the preset mid-session does not rebuild the agent; the value is
	// frozen at turn admission into TaskPolicy.
	AgentPreset string

	// Ablation switches subsystems off for a benchmark arm. The zero value runs
	// everything, so ordinary callers leave it unset.
	Ablation ablation.Set

	// ClassifierTaskText, when non-empty, is the pristine task text, set by
	// sub-agent spawners before host framing is prepended. Delivery intent
	// classification judges it instead of the raw Run input, so framing verbs
	// cannot arm expectations; the delegation audit scores evidence origin
	// against it, so only locations the parent wrote itself count as hints.
	ClassifierTaskText string

	// CapabilityLedger is the optional turn-scoped capability route ledger for
	// Delivery require/prefer gates. Nil disables capability gates.
	CapabilityLedger *capability.Ledger
	// CapabilityAudit is the optional non-persisted metrics sink for routing.
	CapabilityAudit *capability.Audit

	// RequireReviewReportKind, when non-empty, makes RunSubAgentWithSession fail
	// unless the subagent recorded a successful review_report of this kind —
	// review/security subagents must return typed, host-verifiable reports.
	RequireReviewReportKind evidence.ReviewKind

	// ReasoningLanguage controls visible reasoning language preference as transient
	// user-turn context. Empty/auto injects nothing.
	ReasoningLanguage string

	// ResponseLanguage controls final-answer language preference as transient
	// user-turn context. Empty/auto keeps the stable same-as-user policy.
	ResponseLanguage string

	// PlanModeReadOnlyCommands is retained for old config/controller data. Main
	// Plan execution classifies bash through Permissions instead.
	PlanModeReadOnlyCommands []string

	// RecoveryGate is the optional Auto Guard boundary. It checks deterministic
	// high-risk mutations and failure recovery before permission approval and
	// write-lock acquisition.
	RecoveryGate RecoveryGate
	// RecoveryAgentID labels this agent on recovery cards (empty = root).
	RecoveryAgentID string
	// RecoveryTaskID isolates recovery state for this agent (empty = root task).
	RecoveryTaskID string

	// SubagentDepth is the current nesting depth for this agent. Root sessions are
	// depth 0; child subagents are depth 1. MaxSubagentDepth caps delegation.
	SubagentDepth    int
	MaxSubagentDepth int

	// Extensions is the frozen extension dispatcher for this agent's controller
	// generation (Extension Protocol v2). Nil means no runtime packages are
	// installed; the run loop then passes every intercept point through
	// byte-identically. Boot installs it with SetExtensions once sidecars are
	// live (they start after the agent is constructed).
	Extensions *dispatch.Dispatcher

	// MutationObserver is the host-side file mutation observer shared with
	// (or cloned for) sub-agents. nil disables v2 capture. Does not affect
	// provider-visible tool schemas or prompts.
	MutationObserver *checkpoint.MutationObserver
}

// New constructs an Agent. MaxSteps <= 0 means no cap — the run loop continues
// until the model gives a final answer, the context is cancelled, or the
// provider errors (compaction keeps the context bounded). A nil sink is replaced
// with event.Discard so the agent can always emit unconditionally.
func New(prov provider.Provider, tools *tool.Registry, session *Session, opts Options, sink event.Sink) *Agent {
	if opts.CompactRatio <= 0 {
		opts.CompactRatio = defaultCompactRatio
	}
	if opts.RecentKeep <= 0 {
		opts.RecentKeep = minRecentKeep
	}
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	gate := opts.Gate
	if nilutil.IsNil(gate) {
		gate = nil
	}
	planModeReadOnlyTrust := opts.PlanModeReadOnlyTrustGate
	if nilutil.IsNil(planModeReadOnlyTrust) {
		planModeReadOnlyTrust = nil
	}
	sandboxEscapeApprover := opts.SandboxEscapeApprover
	if nilutil.IsNil(sandboxEscapeApprover) {
		sandboxEscapeApprover = nil
	}
	configWriteApprover := opts.ConfigWriteApprover
	if nilutil.IsNil(configWriteApprover) {
		configWriteApprover = nil
	}
	hooks := opts.Hooks
	if nilutil.IsNil(hooks) {
		hooks = nil
	}
	maxStepsKey := opts.MaxStepsKey
	if strings.TrimSpace(maxStepsKey) == "" {
		maxStepsKey = "max_steps"
	}
	maxSubagentDepth := opts.MaxSubagentDepth
	if maxSubagentDepth == 0 {
		maxSubagentDepth = DefaultMaxSubagentDepth
	} else {
		maxSubagentDepth = NormalizeMaxSubagentDepth(maxSubagentDepth)
	}
	subagentDepth := max(opts.SubagentDepth, 0)
	reasoningByteLimit := opts.ReasoningByteLimit
	if reasoningByteLimit == 0 {
		reasoningByteLimit = defaultReasoningByteLimit
	}
	a := &Agent{
		svc: newAgentServices(prov, tools, sink, gate, planModeReadOnlyTrust,
			sandboxEscapeApprover, configWriteApprover, hooks, opts),
		agentConfig: agentConfig{
			maxSteps:           opts.MaxSteps,
			maxStepsKey:        maxStepsKey,
			reasoningByteLimit: reasoningByteLimit,
			maxOutputTokens:    opts.MaxOutputTokens,
			temperature:        opts.Temperature,
			usageSource:        usageSourceOrDefault(opts.UsageSource, event.UsageSourceExecutor),
			modelRef:           strings.TrimSpace(opts.ModelRef),
			workspaceID:        strings.TrimSpace(opts.WorkspaceID),
			classifierTaskText: opts.ClassifierTaskText,
			writeWorkspaceRoot: strings.TrimSpace(opts.WriteWorkspaceRoot),
			subagentDepth:      subagentDepth,
			maxSubagentDepth:   maxSubagentDepth,
			contextWindow:      opts.ContextWindow,
			compactRatio:       opts.CompactRatio,
			recentKeep:         opts.RecentKeep,
			archiveDir:         opts.ArchiveDir,
		},
		sess: sessionRuntime{
			conversation: session,
			path:         strings.TrimSpace(opts.SessionPath),
			cacheState:   CacheStateUnknown,
		},
		task: taskRuntime{
			ledger: evidence.NewLedger(),
			budget: runBudget{limit: normalizeTaskBudget(opts.TaskBudget)},
		},
		requireVisibleFinal: opts.RequireVisibleFinal,
		recovery: recoveryIdentity{
			agentID: strings.TrimSpace(opts.RecoveryAgentID),
			taskID:  strings.TrimSpace(opts.RecoveryTaskID),
		},
		readOnlyExecution:      opts.ReadOnlyExecution,
		plannerMCPExecution:    opts.PlannerMCPExecution,
		projectChecks:          append([]instruction.VerifyCheck(nil), opts.ProjectChecks...),
		deliveryProfile:        opts.DeliveryProfile || agentpreset.Normalize(opts.AgentPreset) == agentpreset.Delivery,
		ablation:               opts.Ablation,
		capabilityLedger:       opts.CapabilityLedger,
		capabilityAudit:        opts.CapabilityAudit,
		keepPolicy:             opts.KeepPolicy,
		strictAlternatingRoles: opts.StrictAlternatingRoles,
	}
	a.sess.output.outputBudget = outputBudgetOf(prov)
	if a.sess.path != "" {
		a.LoadProjectionSidecar(a.sess.path)
	}
	preset := strings.TrimSpace(opts.AgentPreset)
	if preset == "" && opts.DeliveryProfile {
		preset = string(agentpreset.Delivery)
	}
	a.SetAgentPreset(preset)
	a.SetResponseLanguage(opts.ResponseLanguage)
	a.SetReasoningLanguage(opts.ReasoningLanguage)
	a.maybeArmForkFromEnv()
	a.maybeWrapForkCaptureProvider()
	return a
}

// SetAgentPreset updates the role setting used by subsequent turns. It does not
// rebuild providers, tools, or history. The in-flight turn keeps its frozen
// TaskPolicy.
func (a *Agent) SetAgentPreset(preset string) {
	if a == nil {
		return
	}
	p := agentpreset.Normalize(preset)
	a.agentPreset.Store(string(p))

	switch p {
	case agentpreset.Delivery:
		a.deliveryProfile = true
	case agentpreset.Light, agentpreset.Balanced:

		a.deliveryProfile = false
	}
}

// AgentPreset returns the current session role setting (never empty).
func (a *Agent) AgentPreset() string {
	if a == nil {
		return string(agentpreset.Balanced)
	}
	if v := a.agentPreset.Load(); v != nil {
		if s, ok := v.(string); ok && s != "" {
			return string(agentpreset.Normalize(s))
		}
	}
	if a.deliveryProfile {
		return string(agentpreset.Delivery)
	}
	return string(agentpreset.Balanced)
}

// TurnPolicy returns the frozen TaskPolicy for the active turn, if any.
func (a *Agent) TurnPolicy() (taskpolicy.TaskPolicy, bool) {
	if a == nil || !a.turn.policySet {
		return taskpolicy.TaskPolicy{}, false
	}
	return a.turn.policy, true
}

func usageSourceOrDefault(source, fallback string) string {
	source = strings.TrimSpace(source)
	if source != "" {
		return source
	}
	return fallback
}

// missingReasoningWarnStateFor returns nil when no state dir is configured, so
// direct Agent construction keeps the historical once-per-session notice scope.
func missingReasoningWarnStateFor(dir string) *missingReasoningWarnState {
	if strings.TrimSpace(dir) == "" {
		return nil
	}
	return newMissingReasoningWarnState(dir)
}

// reserveParentWrite holds write claims for the duration of a parent-agent
// write tool call. Returns a no-op release when reservation is not needed
// (subagent, read-only, no scheduler, or non-write tool).
func (a *Agent) reserveParentWrite(runTool tool.Tool, args json.RawMessage, readOnly bool) (release func(), err error) {
	noop := func() {}
	if a == nil || a.svc.writeScheduler == nil || a.subagentDepth > 0 || readOnly || runTool == nil {
		return noop, nil
	}
	name := runTool.Name()
	if !parentWriteGuardTarget(name) {
		return noop, nil
	}
	claim, err := parentWriteReservation(a.writeWorkspaceRoot, name, args)
	if err != nil {
		return noop, err
	}
	return a.svc.writeScheduler.ReserveParentWrite(claim)
}

// Run appends the user input and drives the tool loop until the model returns a
// final answer, the context is cancelled, or the provider errors. maxSteps <= 0
// leaves the loop unbounded here: bounding it is the host's call, and the
// adaptive stop is the no-progress ladder rather than a round count. Turn policy
// lives in beginRunTurn / runToolLoop / handleFinalResponse / handleToolRound.
func (a *Agent) Run(ctx context.Context, input string) (runErr error) {
	runMaxSteps := a.maxSteps
	runMaxStepsKey := a.maxStepsKey
	runLimitHostOwned := false
	if limit, ok := runStepLimitFromContext(ctx); ok {
		runMaxSteps = limit.steps
		runLimitHostOwned = true
		if limit.key != "" {
			runMaxStepsKey = limit.key
		}
	}
	a.recovery.runSeq.Add(1)

	if a.svc.workspaceLease != nil {
		a.svc.workspaceLease.BeginRun()
		defer a.svc.workspaceLease.EndRun()
	}
	// Registered first so it runs last, after the evidence-commit and
	// checkpoint defers: a waiver closes the scope's delivery contract so the
	// next turn in the same goal stops inheriting the waived mutation evidence.
	defer func() {
		if runErr == nil && a.turn.deliveryWaiverActive && a.task.ledger != nil {
			a.resetTurnEvidence()
		}
	}()
	turnStartedAt := time.Now()
	workDurationMs := func() int64 {
		if elapsed := time.Since(turnStartedAt).Milliseconds(); elapsed > 0 {
			return elapsed
		}
		return 1
	}
	defer a.flushSteerQueue()
	a.steerMu.Lock()
	a.steerConsumed = false
	a.steerRunActive = true
	a.steerMu.Unlock()

	defer func() {
		if runErr != nil || a.task.ledger == nil || a.svc.jobs == nil {
			return
		}
		for _, lease := range a.task.ledger.BackgroundLeases() {
			a.svc.jobs.CommitEvidenceForSession(lease.Session, lease.JobID)
		}
	}()
	if _, scoped := DeliveryExecutionScopeFromContext(ctx); scoped {
		defer func() { a.updateDeliveryCheckpoint(runErr) }()
	}
	defer a.activeTurnCreatedAt.Store(0)

	if err := a.interceptAgentStart(ctx); err != nil {
		return err
	}

	_, state := a.beginRunTurn(ctx, input)
	if a.pending.forkRestore != nil {
		a.pending.forkRestore(state)
	}
	state.runMaxSteps = runMaxSteps
	state.runMaxStepsKey = runMaxStepsKey
	state.runLimitHostOwned = runLimitHostOwned
	state.workDurationMs = workDurationMs
	return a.runToolLoop(ctx, state)
}
