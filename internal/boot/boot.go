// Package boot assembles a ready-to-drive control.Controller from configuration:
// it loads config, resolves the model(s), builds the tool registry (built-ins +
// plugins), wires the permission gate, and constructs the executor — optionally
// wrapping it in a two-model Coordinator. It is the one place that turns "what the
// user configured" into "a Controller a frontend can drive", so every frontend —
// the terminal TUI, the HTTP/SSE server, the desktop webview — shares the exact
// same assembly instead of each re-deriving it. Frontends pass only a sink and a
// couple of run knobs; everything else comes from config.
package boot

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/capability"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/sidecar"
	"reasonix/internal/extension/uihub"
	"reasonix/internal/hook"
	"reasonix/internal/instruction"
	"reasonix/internal/jobs"
	"reasonix/internal/lsp"
	"reasonix/internal/memory"
	"reasonix/internal/netclient"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/skill"
	"reasonix/internal/taskmonitor"
	"reasonix/internal/tool"
	"reasonix/internal/tool/builtin"
	"reasonix/internal/workspacelease"
)

// ErrUnknownModel is returned by Build when the configured model can't be
// resolved to a provider — e.g. a default_model left over from a renamed or
// removed provider. Callers can detect it (errors.Is) to re-run setup.
var ErrUnknownModel = errors.New("unknown model")

func agentKeepPolicy(keep []string) agent.KeepPolicy {
	if keep == nil {
		return agent.KeepErrors | agent.KeepUserMarked
	}
	var p agent.KeepPolicy
	for _, k := range keep {
		switch strings.TrimSpace(k) {
		case "errors":
			p |= agent.KeepErrors
		case "user_marked":
			p |= agent.KeepUserMarked
		}
	}
	return p
}

// Options carries the per-run knobs a frontend chooses; everything else is read
// from configuration. Model "" falls back to the configured default_model;
// MaxSteps 0 uses automatic execution. RequireKey forces the executor's API key to
// be present (run/serve pass true so a missing key fails fast; chat/desktop pass
// false so the UI is reachable before a key is set). Sink receives the agent's
// typed event stream.
type Options struct {
	Model       string
	MaxSteps    int
	MaxStepsKey string
	RequireKey  bool
	Sink        event.Sink
	// EffortOverride is a session-local reasoning effort override. Nil means use
	// the resolved provider config; a non-nil empty string means provider default.
	EffortOverride *string
	// PermissionAllow adds process-local allow rules (for example CLI
	// --allowed-tools). They override configured ask rules but never deny rules
	// and are not persisted.
	PermissionAllow []string
	// AdditionalDirs grants this session's file writers and sandboxed shell
	// access to extra directories without changing persisted sandbox config.
	AdditionalDirs []string
	// Stderr is the writer for diagnostic warnings and plugin subprocess
	// stderr output. When nil, defaults to os.Stderr. Interactive terminal
	// frontends must provide a private diagnostic writer (or io.Discard) so
	// background output cannot corrupt the TUI's terminal raw mode.
	Stderr io.Writer
	// WorkspaceRoot is the project root directory for config, skills, memory,
	// commands, hooks, and tool confinement. When empty, the current working
	// directory is used (CLI default). Desktop tabs pass their project root here
	// so each tab loads its own config/skills/hooks without changing the process
	// cwd — enabling concurrent multi-project sessions.
	WorkspaceRoot string
	// StatsSource labels this frontend's usage records (desktop/cli/serve).
	// Empty disables usage recording for this controller.
	StatsSource string
	TaskStore   taskmonitor.WriteStore // Authoritative store, never a SQLite catalog.
	// OnConfigLoadWarnings accepts resilient-loader warnings. Returning true
	// lets boot suppress the duplicate migration diagnostic.
	OnConfigLoadWarnings func([]string) bool
	// ExtraPlugins are session-scoped MCP servers supplied by a host transport
	// (for example ACP session/new). They are connected eagerly for this
	// controller but are not persisted to reasonix.toml.
	ExtraPlugins []plugin.Spec
	// AgentPreset selects the session role setting (light|balanced|delivery).
	// Empty falls back to balanced. It controls planning depth, verification
	// breadth, and independent review frequency without changing the
	// provider-visible tool schema or base system prompt.
	AgentPreset string
	// TokenMode is the deprecated one-version fallback for AgentPreset.
	// When AgentPreset is empty, TokenMode is normalized through the legacy
	// economy/full/delivery mapping. Prefer AgentPreset.
	TokenMode string
	// SessionDir overrides where persisted chat transcripts are written. When
	// empty, the shared CLI/global session directory is used.
	SessionDir string
	// SharedHost is an optional plugin.Host shared across controllers for the
	// same workspace root. When set, boot.Build reuses its running clients
	// instead of creating new subprocesses, and the caller manages the host's
	// lifecycle. When nil, Build creates and owns a new host as before.
	SharedHost *plugin.Host
	// CleanupPendingReconciler retries delayed physical cleanup for session
	// artifacts left by a previous process. Nil uses the core physical-delete
	// reconciler; frontends with different deletion semantics can override it.
	CleanupPendingReconciler func(sessionDir string) error
	// ApprovalTimeout bounds how long a tool-approval or ask prompt blocks for a
	// user decision. Zero (default) waits forever — correct for an interactive
	// terminal. Headless/bot frontends pass a positive value so an unanswered
	// prompt can't wedge the session indefinitely (#4626, #4402).
	ApprovalTimeout time.Duration
	// HeadlessApprovalMode selects the non-interactive tool-approval contract
	// (control.ToolApprovalAuto/DontAsk/Yolo) applied to every headless-only gate
	// this boot constructs: the top-level executor, task/read_only_task,
	// writer-capable skill sub-agents, and the planner runner. Empty (or "ask")
	// keeps the default fail-closed headless gate. Callers that later call
	// Controller.ApplyHeadlessApprovalMode with a
	// different mode than they passed here should also pass it here, or
	// sub-agent gates will not match the parent executor's mode.
	HeadlessApprovalMode string
	// SessionRecoveryMeta and OnSessionRecovered let richer frontends attach
	// local UI metadata to automatic transcript recovery branches.
	SessionRecoveryMeta func(control.SessionRecoveryRequest) agent.BranchMeta
	OnSessionRecovered  func(control.SessionRecoveryInfo) error
	// SubagentParentLive reports whether this process currently owns or is
	// building the parent session. Desktop uses it to avoid probing a live tab's
	// lease during stale-subagent cleanup. Nil preserves lease-only cleanup.
	SubagentParentLive func(sessionPath string) bool
	// FileOverlay and TerminalRunner let a host transport (ACP) serve file
	// content from editor buffers and run foreground bash in a host terminal.
	// Both only change where tool I/O happens — tool names, descriptions, and
	// schemas stay byte-identical, so the provider-visible surface is unchanged.
	FileOverlay    builtin.FileOverlay
	TerminalRunner builtin.TerminalRunner
	// ProviderResolver routes every model role through a caller-owned provider
	// catalog. Nil preserves local behavior.
	ProviderResolver provider.Resolver
	// Ablation switches subsystems off for a benchmark arm, and is also the
	// process-local hard override supervised ACP workers use to force the planner
	// off. It wins over user/project configuration without mutating config or
	// changing the provider-visible prompt/tool surface. The zero value runs
	// everything.
	Ablation ablation.Set
	// SandboxNetworkOverride and WorkspaceOnly are process-local hard bounds for
	// supervised ACP workers. Nil/false preserve normal Reasonix config.
	SandboxNetworkOverride *bool
	SandboxBashOverride    string
	WorkspaceOnly          bool
	// SessionTemp is the session-private temp manager; Rebuild reuses old's.
	SessionTemp *sessiontemp.Manager
	RuntimeReload
	// deferPublish keeps a replacement generation private until migration and
	// commit succeed. Cold BuildRuntime leaves this false and publishes at boot.
	deferPublish bool
}

func recoveryHeadlessMode(opts Options) bool {
	return strings.TrimSpace(opts.HeadlessApprovalMode) != ""
}

// bootContext carries the assembled runtime objects between the sequential
// build stages. It is a plain assembly record: no locks, no lifetime grouping.
type bootContext struct {
	stderr                  io.Writer
	root                    string
	additionalDirs          []string
	fileWriteReceipt        func(string, bool, []byte)
	cfg                     *config.Config
	sink                    event.Sink
	generation              uint64
	sessionID               string
	proxySpec               netclient.ProxySpec
	ctrlRef                 *atomic.Pointer[control.Controller]
	controllerReady         chan struct{}
	controllerBuildFailed   chan struct{}
	extUIHub                *uihub.Hub
	extensionMgr            *sidecar.Manager
	pendingMgr              *sidecar.Manager
	baseResolver            provider.Resolver
	effectiveResolver       provider.Resolver
	extensionResolver       provider.Resolver
	modelName               string
	modelRef                string
	entry                   *config.ProviderEntry
	agentPreset             string
	tokenDelivery           bool
	runtimeProfile          capability.Profile
	keepPolicy              agent.KeepPolicy
	workspaceLease          *workspacelease.Owner
	jm                      *jobs.Manager
	sessionDir              string
	balanceClient           *http.Client
	execProv                provider.Provider
	shell                   sandbox.Shell
	sysPrompt               string
	owner                   *extension.RuntimeOwner
	entryResolver           provider.Resolver
	reconcileCleanupPending func(sessionDir string) error
	extWarn                 func(msg string)

	mem                     *memory.Set
	projectChecks           []instruction.VerifyCheck
	skillStore              *skill.Store
	skills                  []skill.Skill
	allSkillStore           *skill.Store
	allSkills               []skill.Skill
	implicitSkillInvocation bool
	reg                     *tool.Registry
	pluginHost              *plugin.Host
	pluginSpecOptions       PluginSpecOptions
	extraSpecs              []plugin.Spec
	enabledMCPNames         map[string]bool
	lspMgr                  *lsp.Manager
	maxSteps                int
	maxSubagentDepth        int
	policy                  permission.Policy
	headlessGate            *control.SharedHeadlessGate
	resolvedHooks           []hook.ResolvedHook
	hookRunner              *hook.Runner
	resolveSubagentProvider func(modelRef, effort string) (provider.Provider, *provider.Pricing, int, error)
	subagentIdentity        func(modelRef, effort string) (string, string)
	subagentScheduler       *agent.SubagentScheduler
	taskTool                *agent.TaskTool
	capRuntime              *agent.MCPCapabilityRuntime
	configSpecs             []plugin.Spec
	cleanup                 func()
	readPathResolver        *builtin.PathResolver
	sessionTemp             *sessiontemp.Manager
	subagentStore           *agent.SubagentStore

	capLedger           *capability.Ledger
	capAudit            *capability.Audit
	capSpecs            []plugin.Spec
	cmds                []command.Command
	skillRunner         func(sctx context.Context, sk skill.Skill, task string, runOpts skill.SubagentRunOptions) (string, error)
	skillProfile        func(sk skill.Skill) *event.Profile
	readOnlySkillRunner func(sctx context.Context, sk skill.Skill, task string, runOpts skill.SubagentRunOptions) (string, error)
	cachedTools         map[string][]plugin.CachedTool
	cacheKeyOK          map[string]bool
}

// build is the assembly body behind BuildRuntime (and the Build compat
// wrapper): it loads config, resolves the model(s), wires the full runtime,
// and freezes the extension kernel snapshot from the objects it just
// assembled. The returned controller owns plugin subprocesses; call Close
// (via Controller.Close) to release them.
func build(ctx context.Context, opts Options) (*BuildResult, error) {
	bc, err := prepareBuild(ctx, opts)
	if err != nil {
		return nil, err
	}
	// Until the RuntimeSet takes ownership at snapshot assembly, every error
	// path must retire the preflighted sidecars — no process may outlive a
	// failed build.
	defer func() {
		if bc.pendingMgr != nil {
			close(bc.controllerBuildFailed)
			_ = bc.pendingMgr.Close()
		}
	}()
	if err := buildTools(ctx, bc, opts); err != nil {
		return nil, err
	}
	if err := buildSubagents(ctx, bc, opts); err != nil {
		return nil, err
	}
	return buildAssemble(ctx, bc, opts)
}
