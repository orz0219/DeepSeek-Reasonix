package control

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/autoresearch"
	"reasonix/internal/capability"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/goaleval"
	"reasonix/internal/guardian"
	"reasonix/internal/jobs"
	"reasonix/internal/nilutil"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/recovery"
	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/skill"
	"reasonix/internal/taskmonitor"
	"reasonix/internal/tool"
	"reasonix/internal/workspacelease"
)

// Options carries the already-built pieces setup assembles. Lifecycle metadata
// lets the controller mint and rotate session files; Host/Commands are surfaced
// to frontends that resolve MCP prompts and slash commands.
type Options struct {
	Runner   agent.Runner
	Executor *agent.Agent
	Guardian *guardian.Session
	// RecoveryReviewer is the optional independent recovery reviewer (nil =
	// rule-only path with fail-closed human confirmation for ambiguous cases).
	RecoveryReviewer recovery.Reviewer
	// RecoveryHeadless blocks mutations that need confirmation instead of
	// waiting forever when no human decision channel exists.
	RecoveryHeadless bool
	// TaskBudget is the configured spend gate; unset leaves a turn unbounded.
	TaskBudget agent.TaskBudget
	// GoalTokenBudget bounds an unattended Goal loop by cumulative tokens.
	GoalTokenBudget int
	// GoalEvaluator is the optional bounded Goal completion evaluator consulted
	// when the working model submits no update_goal report. nil fails closed:
	// the goal pauses instead of defaulting to continue.
	GoalEvaluator goaleval.Evaluator
	Sink          event.Sink
	Policy        permission.Policy
	// SubagentGate is the shared, mutable gate every headless-only sub-agent
	// surface (task, writer-capable skill sub-agents, planner) reads from. Nil
	// disables gating for those surfaces same as before this field existed.
	// SetToolApprovalMode and ApplyHeadlessApprovalMode call Update on it so a
	// runtime approval-mode switch reaches sub-agents, not just the parent
	// executor's own gate.
	SubagentGate  *SharedHeadlessGate
	Label         string
	ModelRef      string
	SystemPrompt  string
	SessionDir    string
	SessionPath   string
	Host          *plugin.Host
	Commands      []command.Command
	Skills        []skill.Skill
	AllSkills     []skill.Skill
	SkillStore    *skill.Store
	AllSkillStore *skill.Store
	// DisableImplicitSkillInvocation controls model-facing discovery only;
	// explicit /skill commands and management remain host-side capabilities.
	DisableImplicitSkillInvocation bool
	// SkillRunner executes a runAs=subagent skill in an isolated child loop.
	// ReadOnlySkillRunner is reserved for explicitly read-only entry points;
	// Plan itself is a workflow instruction and uses SkillRunner with the shared
	// Permissions/Sandbox gate. SkillProfile supplies model/effort display
	// metadata for the synthetic top-level run_skill event.
	SkillRunner         skill.SubagentRunner
	ReadOnlySkillRunner skill.SubagentRunner
	SkillProfile        skill.ProfileResolver
	Cleanup             func()
	// BalanceURL/BalanceKey wire the active provider's optional wallet-balance
	// endpoint and bearer key; empty when the provider declares no balance_url.
	BalanceURL    string
	BalanceKey    string
	BalanceClient *http.Client
	// Jobs is the session-scoped background-job manager (nil disables background jobs).
	Jobs *jobs.Manager
	// TaskStore remains a FileStore-compatible authority. Desktop injects one
	// observed instance so recorder and task-control APIs share post-commit
	// projection hints; nil preserves the ordinary FileStore.
	TaskStore taskmonitor.WriteStore
	// WorkspaceLease is the Delivery writer owner shared with the executor.
	WorkspaceLease *workspacelease.Owner
	// Registry is the executor's live tool set, and PluginCtx the session-scoped
	// context; both are needed for hot-adding MCP servers via AddMCPServer.
	Registry  *tool.Registry
	PluginCtx context.Context
	// MCPDefaultCallTimeout is the global MCP call cap used by hot-connected
	// servers when they do not declare a server- or tool-specific override.
	MCPDefaultCallTimeout time.Duration
	// MCPConfigureSpec injects host-local launch and isolation policy into every
	// hot-connected server without persisting that state in project config.
	MCPConfigureSpec func(*plugin.Spec)
	// CapabilityRuntime is the controller-local authoritative MCP inventory used
	// by stable use_capability frontends. It shares Host processes with sibling
	// tabs but never shares their enabled/disabled state.
	CapabilityRuntime *agent.MCPCapabilityRuntime
	RuntimeGeneration uint64 // PublishGate generation for admission
	// RuntimeOwner isolates publish/drain gates and receipts to one
	// controller/session rebuild lineage. Nil preserves compatibility behavior.
	RuntimeOwner *extension.RuntimeOwner
	// WorkspaceRoot is the project root checkpoint restores are confined to ("" =
	// no confinement). Frontends pass the cwd they launched the session in.
	WorkspaceRoot          string
	ExternalFolderToolRefs externalFolderToolRefs
	// ResponseLanguage controls final-answer language preference. Empty/auto
	// means no transient injection because the stable language policy follows the
	// current user turn.
	ResponseLanguage string
	// ReasoningLanguage controls visible reasoning language preference. Empty/auto
	// means no transient injection because the stable language policy already
	// follows the conversation language.
	ReasoningLanguage string
	// DisableColdResumePrune suppresses the cold-resume cache-state notice.
	// Resume never rewrites history regardless of this flag.
	DisableColdResumePrune bool
	// Shell is the interpreter user-invoked "!" commands run under, so /shell
	// matches the agent's configured [tools.shell] choice. Zero value = auto.
	Shell sandbox.Shell
	// OnRemember, when set, is invoked with a new allow rule the user chose to
	// persist to disk (e.g. "Bash(go test:*)"). The callback is wired into the
	// permission Gate on EnableInteractiveApproval.
	OnRemember func(rule string) RememberResult
	// OnRememberPlanModeReadOnlyCommand persists a bash command prefix as trusted
	// read-only when the user chooses "always allow" from the plan-mode trust
	// prompt.
	OnRememberPlanModeReadOnlyCommand func(prefix string) PlanModeReadOnlyCommandTrustResult
	// SessionRecoveryMeta lets a frontend attach scope/topic/profile metadata to
	// an automatic recovery branch before it is written.
	SessionRecoveryMeta func(SessionRecoveryRequest) agent.BranchMeta
	// OnSessionRecovered is called after a stale runtime's transcript has been
	// saved as a recovery branch, before the controller commits to that branch.
	OnSessionRecovered func(SessionRecoveryInfo) error
	// ApprovalTimeout bounds how long a tool-approval or ask prompt blocks waiting
	// for a user decision. Zero (default) waits forever — right for an interactive
	// terminal. Headless frontends set a positive value so an unanswered
	// prompt can't wedge the session indefinitely (#4626, #4402).
	ApprovalTimeout time.Duration
	// RuntimeProfile selects capability routing/filtering behavior. Empty keeps
	// the backward-compatible Balanced profile.
	RuntimeProfile capability.Profile
	// Extensions is the frozen extension dispatcher for this controller
	// generation (Extension Protocol v2, stage 6b1). Nil means no v2 runtime
	// packages are installed: every extension wiring point takes an untouched
	// fast path. Boot installs it through SetExtensions because sidecars (and
	// therefore the dispatcher) only exist after snapshot assembly, which runs
	// after New.
	Extensions *dispatch.Dispatcher
	// ProviderResolver is the build's merged provider catalog — extension
	// sidecar providers folded over the config/broker base (stage 7). Nil when
	// no v2 runtime sidecar declared providers; ProviderCatalog then returns
	// nil and frontends enumerate providers from config alone, as before.
	ProviderResolver provider.Resolver
	// Ablation switches subsystems off for a benchmark arm. The zero value runs
	// everything.
	Ablation ablation.Set
	// SessionTemp is the logical-session private temporary directory manager
	// shared by sandboxed Bash calls. Nil creates a fresh Manager owned by this
	// Controller. Hot rebuilds pass the previous Controller's Manager so the
	// temporary directory survives model/settings swaps.
	SessionTemp *sessiontemp.Manager
}

// New builds a Controller. A nil Sink becomes event.Discard; unless the caller
// already provided a goalUsageTee (NewGoalUsageTee), the sink is wrapped in one
// so billable usage can be accounted to Goal budgets.
func New(opts Options) *Controller {
	sink := opts.Sink
	if nilutil.IsNil(sink) {
		sink = event.Discard
	}
	usageTee, ok := sink.(*goalUsageTee)
	if !ok {
		usageTee = NewGoalUsageTee(sink).(*goalUsageTee)
		sink = usageTee
	}
	pluginCtx := opts.PluginCtx
	if pluginCtx == nil {
		pluginCtx = context.Background()
	}
	runtimeOwner := runtimeOwnerOrDefault(opts.RuntimeOwner)
	pluginCtx = extension.ContextWithRuntimeOwner(pluginCtx, runtimeOwner)
	runtimeProfile := opts.RuntimeProfile
	if runtimeProfile == "" {
		runtimeProfile = capability.ProfileBalanced
	}
	c := &Controller{
		taskBudget:                        opts.TaskBudget,
		goalTokenBudget:                   opts.GoalTokenBudget,
		goals:                             goalMachine{tokenBudget: opts.GoalTokenBudget},
		runner:                            opts.Runner,
		executor:                          opts.Executor,
		guardianSess:                      opts.Guardian,
		guardianPath:                      guardian.PathFor(opts.SessionPath),
		evaluator:                         opts.GoalEvaluator,
		goalUsageTee:                      usageTee,
		sink:                              sink,
		policy:                            opts.Policy,
		subagentGate:                      opts.SubagentGate,
		label:                             opts.Label,
		modelRef:                          opts.ModelRef,
		systemPrompt:                      opts.SystemPrompt,
		sessionDir:                        opts.SessionDir,
		sessionPath:                       opts.SessionPath,
		commands:                          atomic.Pointer[[]command.Command]{},
		skills:                            newSkillSet(opts.Skills, opts.AllSkills, opts.SkillStore, opts.AllSkillStore),
		disableImplicitSkillInvocation:    opts.DisableImplicitSkillInvocation,
		skillRunner:                       opts.SkillRunner,
		readOnlySkillRunner:               opts.ReadOnlySkillRunner,
		skillProfile:                      opts.SkillProfile,
		cleanup:                           opts.Cleanup,
		responseLanguage:                  config.NormalizeLanguage(opts.ResponseLanguage),
		reasoningLanguage:                 config.NormalizeReasoningLanguage(opts.ReasoningLanguage),
		disableColdResumePrune:            opts.DisableColdResumePrune,
		shell:                             opts.Shell,
		onRemember:                        opts.OnRemember,
		onRememberPlanModeReadOnlyCommand: opts.OnRememberPlanModeReadOnlyCommand,
		sessionRecoveryMeta:               opts.SessionRecoveryMeta,
		onSessionRecovered:                opts.OnSessionRecovered,
		balanceURL:                        opts.BalanceURL,
		balanceKey:                        opts.BalanceKey,
		balanceClient:                     opts.BalanceClient,
		jobs:                              opts.Jobs,
		workspaceLease:                    opts.WorkspaceLease,
		mcp:                               newMcpManager(opts.Host, opts.Registry, pluginCtx),
		mcpDefaultCallTimeout:             opts.MCPDefaultCallTimeout,
		mcpConfigureSpec:                  opts.MCPConfigureSpec,
		capabilityRuntime:                 opts.CapabilityRuntime,
		runtimeProfile:                    runtimeProfile,
		ablation:                          opts.Ablation,
		workspaceRoot:                     opts.WorkspaceRoot,
		externalFolderToolRefs:            opts.ExternalFolderToolRefs,
		providerResolver:                  opts.ProviderResolver,
		runtimeGeneration:                 opts.RuntimeGeneration,
		runtimeOwner:                      runtimeOwner,
		approval:                          newApprovalManager(opts.Policy, ToolApprovalAsk, opts.ApprovalTimeout),
	}

	if opts.SessionTemp != nil {
		c.sessionTemp = opts.SessionTemp
	} else {
		c.sessionTemp = sessiontemp.New()
	}
	c.sessionTemp.Retain()

	if strings.TrimSpace(opts.WorkspaceRoot) != "" {
		c.legacyResearchArchive = legacyResearchArchive{store: autoresearch.NewStore(opts.WorkspaceRoot)}
	}
	if opts.Extensions != nil {
		c.extensions = opts.Extensions
		c.sink = newFrontendEventSink(c.sink, opts.Extensions)
		if c.executor != nil {
			c.executor.SetExtensions(opts.Extensions)
		}
	}

	c.rebindCheckpoints(opts.SessionPath)
	c.setActiveJobSession(opts.SessionPath)
	c.rebindInbox()

	c.sink = &inboxEventSink{inner: c.sink, c: c}
	if c.executor != nil {
		c.executor.SetSink(c.sink)
	}
	cmdsInit := opts.Commands
	c.commands.Store(&cmdsInit)
	if c.executor != nil {
		c.wireMutationObserver()
	}

	c.initRecoveryGate(opts.RecoveryReviewer, opts.RecoveryHeadless)

	if c.jobs != nil && c.workspaceRoot != "" {
		taskStore := opts.TaskStore
		if taskStore == nil {
			taskStore = taskmonitor.NewFileStore(filepath.Join(".reasonix", "tasks"))
		}
		c.jobs.SetTaskRecorder(taskmonitor.NewTaskRecorder(
			taskStore,
			c.workspaceRoot,
			func() string { return c.parentSessionID() },
		))
	}
	return c
}
