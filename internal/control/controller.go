// Package control is the transport-agnostic session driver. A Controller owns
// the agent run loop and session lifecycle, takes commands (Send/Cancel/Approve/
// SetPlanMode/Compact/NewSession/…), and emits everything that happens —
// reasoning, tool calls, approvals, turn completion — as a typed event stream to
// a single event.Sink.
//
// The point is one orchestration layer behind every frontend: a terminal TUI, a
// desktop webview, or an HTTP/SSE server each drive the Controller identically
// (issue commands, render events) and none of them re-implement turn lifecycle,
// cancellation, or approval. The Controller depends on no frontend.
package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"reasonix/internal/ablation"
	"reasonix/internal/agent"
	"reasonix/internal/capability"
	"reasonix/internal/checkpoint"
	"reasonix/internal/command"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/extension"
	"reasonix/internal/extension/dispatch"
	"reasonix/internal/extension/uihub"
	"reasonix/internal/goaleval"
	"reasonix/internal/guardian"
	"reasonix/internal/hook"
	"reasonix/internal/jobs"
	"reasonix/internal/permission"
	"reasonix/internal/plugin"
	"reasonix/internal/provider"
	"reasonix/internal/recovery"
	"reasonix/internal/sandbox"
	"reasonix/internal/sessiontemp"
	"reasonix/internal/skill"
	"reasonix/internal/workspacelease"
)

// ErrTurnRunning reports that a caller tried to start a second foreground turn
// while one is already active in the same Controller.
var ErrTurnRunning = errors.New("turn already running")

// ErrRuntimeDraining reports that a caller targeted a controller generation
// superseded by a successful rebuild.
var ErrRuntimeDraining = errors.New("runtime is draining after rebuild")

// errTurnRunningRotation and errRotationInProgress are returned by the
// session-rotation gate (beginRotation) when a rotation cannot proceed: a turn
// is in flight, or another rotation already holds the gate.
var (
	errTurnRunningRotation = errors.New("cannot start a new session while a turn is running")
	errRotationInProgress  = errors.New("cannot start a new session while another session change is in progress")
)

// errNoSessionPath is returned by snapshot when a session has content to persist
// but no resolved session path — a misconfiguration (e.g. an unresolvable data
// dir in a bot deployment) that previously dropped conversations silently
// (#4414). Callers log it and continue; it must never be swallowed quietly.
var errNoSessionPath = errors.New("session has content but no session path; conversation cannot be persisted")

// Controller drives one chat session. Construct with New; drive with the command
// methods; observe through the Sink passed in Options.
type Controller struct {
	runner       agent.Runner
	executor     *agent.Agent
	guardianSess *guardian.Session // nil when guardian is disabled
	guardianPath string            // persisted guardian session file ("" when disabled)
	// recoveryGate is the shared Auto Guard state for this controller.
	// nil when the feature is not wired for this controller.
	recoveryGate *recovery.Gate

	// taskBudget is the configured spend gate, as passed at construction.
	taskBudget agent.TaskBudget
	// goalTokenBudget bounds an unattended Goal loop; 0 leaves it unbounded.
	goalTokenBudget int
	// evaluator is the bounded Goal completion evaluator consulted when the
	// working model submits no update_goal report. nil fails closed: the goal
	// pauses instead of defaulting to continue.
	evaluator goaleval.Evaluator
	// goalUsageTee accounts billable usage events into the active goal turn's
	// observational token total. It wraps the public sink when the caller didn't provide one.
	goalUsageTee *goalUsageTee
	sink         event.Sink
	policy       permission.Policy
	// subagentGate is the shared gate every headless-only sub-agent surface
	// reads from (see Options.SubagentGate). Nil when the caller didn't build
	// one — sub-agents then keep whatever gate they were constructed with.
	subagentGate *SharedHeadlessGate

	label        string
	modelRef     string
	systemPrompt string
	sessionDir   string
	commands     atomic.Pointer[[]command.Command]
	// skills owns the session's discovered skills (enabled subset, full set, and
	// the reloadable stores) — the skills slice of the Capabilities concern. See
	// skill.go.
	skills                         skillSet
	skillRunner                    skill.SubagentRunner
	readOnlySkillRunner            skill.SubagentRunner
	skillProfile                   skill.ProfileResolver
	disableImplicitSkillInvocation bool
	slashSkillSeq                  atomic.Uint64
	hooks                          *hook.Runner // session hook runner; nil-safe (no hooks configured)
	// hookContexts carries one-shot lifecycle hook context into the next real
	// user turn without changing the cache-stable system prompt.
	hookContexts []string
	// memory owns the loaded memory snapshot, the pending turn-tail notes queue,
	// and write serialization behind its own locks, off c.mu — so a memory-panel
	// save never stalls an approval or status poll. See memory.go.
	memory                 memoryManager
	cleanup                func()
	responseLanguage       string
	reasoningLanguage      string
	disableColdResumePrune bool // legacy; rewrite elision removed, still gates cold notice
	// testCacheColdAfter overrides cacheColdAfter() in tests. Zero uses the
	// vendor-aware resolution from config.
	testCacheColdAfter time.Duration

	shell                             sandbox.Shell                    // interpreter for user-invoked "!" commands; zero = auto
	startedOnce                       bool                             // guards the one-shot SessionStart hook on first turn
	closeOnce                         sync.Once                        // makes close idempotent under racing teardown paths
	onRemember                        func(rule string) RememberResult // set via Options; invoked when user picks "always allow"
	onRememberPlanModeReadOnlyCommand func(prefix string) PlanModeReadOnlyCommandTrustResult
	sessionRecoveryMeta               func(SessionRecoveryRequest) agent.BranchMeta
	onSessionRecovered                func(SessionRecoveryInfo) error

	// balanceURL/balanceKey target the active provider's optional wallet-balance
	// endpoint (empty when the provider declares none). Captured at build so a
	// model/key switch — which rebuilds the controller — refreshes them.
	balanceURL    string
	balanceKey    string
	balanceClient *http.Client

	// jobs is the session-scoped background-job manager. The agent's background
	// tools spawn into it; Compose drains its completion notes into the next turn;
	// Close cancels its still-running jobs.
	jobs *jobs.Manager
	// workspaceLease is the Delivery writer owner shared with the executor.
	// It is exposed only through a sanitized state snapshot for Desktop recovery.
	workspaceLease *workspacelease.Owner

	// mcp owns the session's live tool/plugin surface — the MCP plugin Host, the
	// tool registry the executor reads each turn, and the session-scoped context a
	// hot-added stdio server binds its subprocess to — behind its own lock, off
	// c.mu. The Controller keeps the config-facing orchestration (persisting
	// MCP entries to their global/project source on add/remove, building specs
	// from entries). See mcp.go.
	mcp                   mcpManager
	mcpDefaultCallTimeout time.Duration
	mcpConfigureSpec      func(*plugin.Spec)
	capabilityRuntime     *agent.MCPCapabilityRuntime

	runtimeGeneration  uint64 // PublishGate gen; 0 disables
	runtimeOwner       *extension.RuntimeOwner
	lastResumeDecision extension.ResumeDecision
	// extensions is the frozen extension dispatcher for this controller
	// generation, or nil when no v2 runtime packages are installed (the
	// universal pre-dispatch fast path). It is installed before the controller
	// starts serving (Options.Extensions or SetExtensions) and never swapped
	// afterwards, so wiring points read it without locking.
	extensions *dispatch.Dispatcher
	// extensionUI is the host extension UI hub for this controller generation
	// (stage 8a), or nil when no v2 runtime packages started. Installed via
	// SetExtensionUI before serving and never swapped; readers take c.mu.
	extensionUI *uihub.Hub
	// providerResolver is the build's merged provider catalog (extension
	// sidecar providers over the config/broker base), or nil when no sidecar
	// declared providers. Immutable after New; ProviderCatalog reads it.
	providerResolver provider.Resolver

	// Capability routing (Delivery hybrid route + dual-model Planner proxy).
	// Not part of the provider-visible prefix; only seeds the turn-scoped ledger
	// and optional semantic router.
	pluginCfg       []config.PluginEntry
	capCachedTools  map[string][]plugin.CachedTool
	capCacheKeyOK   map[string]bool
	semanticRouter  *capability.SemanticRouter
	capabilityAudit *capability.Audit
	// capabilityProxy directs unready MCP candidates to use_capability in the
	// transient route block (Delivery and dual-model Planner).
	capabilityProxy bool
	// proxyToolsFn returns live tools observed through use_capability without
	// entering the provider-visible registry (Balanced dual-model Planner).
	proxyToolsFn   func() map[string][]plugin.CachedTool
	runtimeProfile capability.Profile
	ablation       ablation.Set

	// goals owns the active goal's FSM (status, intercepts, idle/turn counters)
	// and its persistence, behind its own mutex so a per-turn goal save never
	// stalls an approval or status poll on c.mu. See goal.go.
	goals goalMachine
	// legacyResearchArchive reads explicit pre-unification task paths. It never
	// creates or mutates archive state. See
	// autoresearch_manager.go.
	legacyResearchArchive legacyResearchArchive
	legacyRestoreMu       sync.Mutex
	legacyRestore         legacyGoalRestore

	// workspaceRoot is the workspace root: the base for resolving @-refs and slash
	// path refs, the working directory for user "!" shell commands and custom
	// command discovery, and the guard root for checkpoint restore writes. It is
	// surfaced to frontends via WorkspaceRoot().
	workspaceRoot string

	// externalFolderRefs maps session-generated @ tokens to user-dropped
	// directories outside workspaceRoot. It is intentionally per-controller:
	// dragging a folder authorizes that folder for this chat session only, without
	// widening scoped @ resolution to arbitrary absolute paths.
	externalFolderRefsMu   sync.RWMutex
	externalFolderRefs     map[string]string
	externalFolderToolRefs externalFolderToolRefs

	// checkpoints owns the snapshot-based rewind bookkeeping (the per-session
	// store, the monotonic turn counter, and the conversation-rewind boundary map)
	// behind its own lock, off c.mu — so a boundary read for a rewind/fork never
	// contends on the run-state lock. The Controller keeps the rewind/fork/summarize
	// orchestration (truncating the session, restoring code, emitting events). See
	// checkpoint.go.
	checkpoints checkpointManager
	// mutationObserver is the host-side file mutation observer for v2 checkpoints.
	mutationObserver *checkpoint.MutationObserver
	// sessionRevision increments on successful rewind/undo and is used as a
	// prepare/commit freshness token.
	sessionRevision int64

	// approval owns the approval/ask prompt bookkeeping and the runtime approval
	// posture (ask/auto/yolo, session grants, the just-approved-plan window)
	// behind its own locks, off c.mu. The Controller keeps the I/O orchestration
	// (requestApproval/Ask emit events + fire hooks + rebuild the executor gate).
	// See approval.go.
	approval approvalManager

	// mu guards the run state; every critical section under it is short and
	// non-blocking.
	mu        sync.Mutex
	cancel    context.CancelFunc
	running   bool
	finishing bool // TurnDone is still being delivered; park a replacement turn
	canceling bool
	// closed marks the controller as terminally torn down (close() ran). It
	// seals turn admission: without it, a submit arriving AFTER close cleared
	// the parked queue — but while a still-running turn's TurnDone delivery
	// was in flight — would park again and then start against freed resources
	// when the window closed.
	closed bool
	// parkedTurns holds turn bodies that arrived during the finishing window,
	// FIFO. finishGuardedTurn starts the oldest one as it closes the window
	// (see runGuarded/finishGuardedTurn); close() discards any remainder.
	parkedTurns []func(ctx context.Context) error
	// rotating is set under mu while NewSession/ClearSession swap the executor
	// session out. Checking running once and then swapping later leaves a
	// TOCTOU window: a turn can start (running=false at check time) during the
	// intervening Snapshot() and then have its live session replaced. running
	// and rotating are mutually exclusive gates — a turn refuses to start while
	// a rotation is in progress, and a rotation refuses to start while a turn
	// runs — so the run loop's session reference cannot change under it.
	rotating    bool
	autosaveWG  sync.WaitGroup
	planMode    bool
	sessionPath string
	// sessionTemp owns the logical-session private temporary directory shared
	// by Bash calls. Retained for this Controller's lifetime; rotated on
	// /new, /clear, resume of another session, and branch switches.
	sessionTemp *sessiontemp.Manager
	// recoveryDepthCapNotices records session paths that already surfaced the
	// depth-cap recovery warning. Repeated saves on the same conflict copy are
	// diagnostic noise for the UI; keep logging/diagnostics, but emit the user
	// notice once per controller/session path.
	recoveryDepthCapNotices map[string]bool
	// snapshotMu serializes the whole save/recovery handoff for this controller.
	// Agent-level path locks protect individual files, but recovery also moves
	// controller-owned state (sessionPath, guardianPath, checkpoints, rewrite
	// baseline). Letting a second snapshot observe that migration halfway through
	// can turn one conflict into a recovery cascade. Session/path swaps
	// (new/clear/fork/branch/switch/resume/SetSessionPath) hold it for the same
	// reason: a save that reads the old path but the new session would write one
	// transcript's messages into another's file, or manufacture a bogus conflict.
	// Not reentrant — never call snapshot (or anything that snapshots, such as
	// recoverInterruptedTurn or maybeColdResumePrune) while holding it.
	snapshotMu sync.Mutex
	// turn counts model turns this session, passed to hooks in their payload.
	turn int

	displayRecorder func(content, display string)

	// inbox is the durable session-level instruction queue. Disk I/O never
	// runs under c.mu; the store owns its own lock.
	inbox inboxState
}

type approvalReply struct {
	allow   bool
	session bool
	persist bool // true = write "always allow" rule to config
}

type pendingApproval struct {
	id           string
	tool         string
	subject      string
	reason       string
	rawInput     json.RawMessage
	fresh        bool
	requireHuman bool
	autoDrain    bool
	kind         string // tool | plan | recovery; empty = tool
	recovery     *event.RecoveryApproval
	reply        chan approvalReply
}

// pendingAsk is an in-flight ask question batch. questions is retained so the
// AskRequest can be re-emitted to a frontend that reconnected after the original
// event (see ReplayPendingPrompts).
type pendingAsk struct {
	questions []event.AskQuestion
	reply     chan []event.AskAnswer
	queued    bool // registered but not yet shown; replay must skip it
}

type plannerSessionResetter interface {
	ResetPlannerSession()
}

// RuntimeStatus is the frontend-facing snapshot of foreground turn state. It is
// intentionally more explicit than the legacy Running bool so UI code can
// distinguish a cancellable foreground turn from pending prompts and background
// jobs.

const (
	ToolApprovalAsk     = "ask"
	ToolApprovalAuto    = "auto"
	ToolApprovalDontAsk = "dontAsk"
	ToolApprovalYolo    = "yolo"
)

const (
	memoryRememberTool = "remember"
	memoryForgetTool   = "forget"
)

// RememberResult describes what happened when an approval rule was persisted.
type RememberResult struct {
	Rule      string
	Path      string
	Saved     bool
	CoveredBy string
	Err       error
}

// PlanModeReadOnlyCommandTrustResult describes what happened when a trusted bash
// command prefix was persisted for plan-mode research.
type PlanModeReadOnlyCommandTrustResult struct {
	Prefix    string
	Path      string
	Saved     bool
	CoveredBy string
	Err       error
}

type SessionRecoveryRequest struct {
	OriginalPath string
	Reason       string
	Mode         string
}

type SessionRecoveryInfo struct {
	OriginalPath string
	RecoveryPath string
	Existing     bool
	Reason       string
	Meta         agent.BranchMeta
}

type externalFolderToolRefs interface {
	RegisterReadRoot(token, root string)
}

// Options carries the already-built pieces setup assembles. Lifecycle metadata
// lets the controller mint and rotate session files; Host/Commands are surfaced
// to frontends that resolve MCP prompts and slash commands.

// RecoveryReviewer is the optional independent recovery reviewer (nil =
// rule-only path with fail-closed human confirmation for ambiguous cases).

// RecoveryHeadless blocks mutations that need confirmation instead of
// waiting forever when no human decision channel exists.

// TaskBudget is the configured spend gate; unset leaves a turn unbounded.

// GoalTokenBudget bounds an unattended Goal loop by cumulative tokens.

// GoalEvaluator is the optional bounded Goal completion evaluator consulted
// when the working model submits no update_goal report. nil fails closed:
// the goal pauses instead of defaulting to continue.

// SubagentGate is the shared, mutable gate every headless-only sub-agent
// surface (task, writer-capable skill sub-agents, planner) reads from. Nil
// disables gating for those surfaces same as before this field existed.
// SetToolApprovalMode and ApplyHeadlessApprovalMode call Update on it so a
// runtime approval-mode switch reaches sub-agents, not just the parent
// executor's own gate.

// DisableImplicitSkillInvocation controls model-facing discovery only;
// explicit /skill commands and management remain host-side capabilities.

// SkillRunner executes a runAs=subagent skill in an isolated child loop.
// ReadOnlySkillRunner is reserved for explicitly read-only entry points;
// Plan itself is a workflow instruction and uses SkillRunner with the shared
// Permissions/Sandbox gate. SkillProfile supplies model/effort display
// metadata for the synthetic top-level run_skill event.

// BalanceURL/BalanceKey wire the active provider's optional wallet-balance
// endpoint and bearer key; empty when the provider declares no balance_url.

// Jobs is the session-scoped background-job manager (nil disables background jobs).

// TaskStore remains a FileStore-compatible authority. Desktop injects one
// observed instance so recorder and task-control APIs share post-commit
// projection hints; nil preserves the ordinary FileStore.

// WorkspaceLease is the Delivery writer owner shared with the executor.

// Registry is the executor's live tool set, and PluginCtx the session-scoped
// context; both are needed for hot-adding MCP servers via AddMCPServer.

// MCPDefaultCallTimeout is the global MCP call cap used by hot-connected
// servers when they do not declare a server- or tool-specific override.

// MCPConfigureSpec injects host-local launch and isolation policy into every
// hot-connected server without persisting that state in project config.

// CapabilityRuntime is the controller-local authoritative MCP inventory used
// by stable use_capability frontends. It shares Host processes with sibling
// tabs but never shares their enabled/disabled state.

// PublishGate generation for admission
// RuntimeOwner isolates publish/drain gates and receipts to one
// controller/session rebuild lineage. Nil preserves compatibility behavior.

// WorkspaceRoot is the project root checkpoint restores are confined to ("" =
// no confinement). Frontends pass the cwd they launched the session in.

// ResponseLanguage controls final-answer language preference. Empty/auto
// means no transient injection because the stable language policy follows the
// current user turn.

// ReasoningLanguage controls visible reasoning language preference. Empty/auto
// means no transient injection because the stable language policy already
// follows the conversation language.

// DisableColdResumePrune suppresses the cold-resume cache-state notice.
// Resume never rewrites history regardless of this flag.

// Shell is the interpreter user-invoked "!" commands run under, so /shell
// matches the agent's configured [tools.shell] choice. Zero value = auto.

// OnRemember, when set, is invoked with a new allow rule the user chose to
// persist to disk (e.g. "Bash(go test:*)"). The callback is wired into the
// permission Gate on EnableInteractiveApproval.

// OnRememberPlanModeReadOnlyCommand persists a bash command prefix as trusted
// read-only when the user chooses "always allow" from the plan-mode trust
// prompt.

// SessionRecoveryMeta lets a frontend attach scope/topic/profile metadata to
// an automatic recovery branch before it is written.

// OnSessionRecovered is called after a stale runtime's transcript has been
// saved as a recovery branch, before the controller commits to that branch.

// ApprovalTimeout bounds how long a tool-approval or ask prompt blocks waiting
// for a user decision. Zero (default) waits forever — right for an interactive
// terminal. Bot/headless frontends set a positive value so an unanswered
// prompt can't wedge the session indefinitely (#4626, #4402).

// RuntimeProfile selects capability routing/filtering behavior. Empty keeps
// the backward-compatible Balanced profile.

// Extensions is the frozen extension dispatcher for this controller
// generation (Extension Protocol v2, stage 6b1). Nil means no v2 runtime
// packages are installed: every extension wiring point takes an untouched
// fast path. Boot installs it through SetExtensions because sidecars (and
// therefore the dispatcher) only exist after snapshot assembly, which runs
// after New.

// ProviderResolver is the build's merged provider catalog — extension
// sidecar providers folded over the config/broker base (stage 7). Nil when
// no v2 runtime sidecar declared providers; ProviderCatalog then returns
// nil and frontends enumerate providers from config alone, as before.

// Ablation switches subsystems off for a benchmark arm. The zero value runs
// everything.

// SessionTemp is the logical-session private temporary directory manager
// shared by sandboxed Bash calls. Nil creates a fresh Manager owned by this
// Controller. Hot rebuilds pass the previous Controller's Manager so the
// temporary directory survives model/settings swaps.

// New builds a Controller. A nil Sink becomes event.Discard; unless the caller
// already provided a goalUsageTee (NewGoalUsageTee), the sink is wrapped in one
// so billable usage can be accounted to Goal budgets.

// Session-private temporary directory: reuse a shared Manager on hot
// rebuild, otherwise create one. Retain so ReleaseResources/Close drop the
// owner reference without racing a replacement Controller.

// Checkpoints: bind a store to the session and route writer pre-edits into it.

// Observe Steer / unapplied-steer for durable inbox state transitions.
// Must wrap both the controller sink and the executor sink: agent.Steer
// emits on the executor path, TurnDone on the controller path.

// Auto Guard is built into Auto. Ask and YOLO bypass it through the mode
// provider, so no separate enablement state is needed.

// Task monitoring: record background-job lifecycle into the project-local
// task store so CLI, Desktop, scripts, and future clients observe the same
// state/event evidence. The recorder swallows its own failures — monitoring
// must never affect the agent pipeline. The session id is resolved lazily
// because the session path is only fixed once the first turn begins.

// SetDisplayRecorder installs an optional hook used by frontends that persist a
// shorter user-facing transcript than the fully composed model prompt.

// SetExtensions installs the extension dispatcher after construction. Boot
// uses it because sidecars — and therefore the dispatcher — only exist after
// snapshot assembly, which runs after New. First non-nil install wins for the
// cold-start path; use ReplaceExtensions for generation-safe rebuild swaps.
// Nil is a no-op. The executor agent receives the same dispatcher (stage 6b2).

// ReplaceExtensions atomically swaps the dispatcher for a reused controller
// after a narrow rebuild. Updates sink strategy owner and executor together.

// Keep the inbox observer as the outermost sink so Steer/unapplied events
// always update durable state, while still installing/updating the
// frontendEventSink wrapper underneath for extension rulings.

// Ensure inbox observer stays outer.

// SetProviderResolver replaces the session's merged provider catalog (narrow
// rebuild after sidecar Manager roll). Nil clears extension-hosted providers.

// ApplyExtensionSystemPrompt swaps the executor to a fresh session carrying
// the extension strategy's final system prompt and makes it the controller's
// rotation prompt, so /new and /clear keep the strategy-composed prompt too.
// Boot calls it when a system_prompt.build replacement changed the prompt
// after the controller (and its session) was built with the host-composed
// one. It must run before any turn or history resume: the fresh session holds
// only the system message, so a later resume cleanly layers history on top.

// SetOnSessionRecovered installs the ownership handoff invoked before the
// controller commits to an automatically created recovery branch. Frontends
// that acquire their session owner after controller construction (for example
// reasonix serve) use this before publishing the controller.

// ToolContractEntries returns a stable snapshot of the executor's live tool
// contract: provider-visible names, descriptions, canonical schemas, and
// read-only flags. It is intended for diagnostics and regression tests.

// AllToolContractEntries returns every registered tool, including those hidden
// from the provider-visible schema and only reachable via use_capability.

// ProviderCatalog returns the session's merged provider catalog: the config
// (or broker) base plus every provider a live extension sidecar declared,
// keyed by ref — extension refs carry their plugin/<plugin>/<provider>/<model>
// namespace. Nil when no sidecar declared providers, so frontends can tell
// "enumerate config only" apart from "the extension catalog is empty".

// A periodic autosave may already contain this user message without its
// local edit metadata. Classify the mutation atomically so the turn-end
// save performs an owned rewrite instead of forking a bogus
// same-revision recovery branch. Edited/Original are local-only display
// metadata (provider requests ignore them), so this must not report a
// cache-prefix change — ReplaceLocalMetadata, not Rewrite.

// ckptDir derives a session's checkpoint directory from its file path
// (…/<id>.jsonl → …/<id>.ckpt). Empty path → empty (in-memory checkpoints).

// rebindCheckpoints points the store at the (possibly new) session, loading any
// checkpoints already on disk, and resets the turn boundaries. Called on
// construction and whenever the session path changes (NewSession/Resume/SetSessionPath).
// Also re-wires the mutation observer so capture targets the new store.

// commands (frontend → controller)

// spawnGuardedTurn launches an admitted turn body plus its autosave companion.
// The caller must already have claimed admission (running=true) under c.mu.

// finishGuardedTurn keeps admission closed while TurnDone is delivered. The
// sink fan-out may detach per-turn transports; allowing a replacement turn in
// after running=false but before that fan-out completed let the old completion
// clear or inherit the replacement turn's transport.
//
// When the window closes, the oldest parked turn (if any) is started under the
// SAME critical section that clears finishing: opening the gate first and then
// re-admitting would let an unrelated submit slip in ahead and bounce the
// parked turn back to a drop. Remaining parked turns drain one per
// finishGuardedTurn, preserving FIFO order. Rotation cannot interleave here:
// beginRotation refuses while running or finishing, and the drain flips
// finishing directly into running.

// A live controller keeps admission closed until TurnDone fan-out finishes.
// Close has already sealed admission permanently, so a late completion must
// not resurrect a finishing state after teardown.

// No parked compatibility body: admit the next durable inbox item.

// Prefer a single representative id for the wire event (first active).
// Full multi-item ack happens in onInboxTurnDone via activeItemIDs.

// Ack active durable items before exposing TurnDone. Frontends commonly
// refresh the inbox from that event and must not observe already-consumed
// steers in the completed turn. Dispatch still waits for finishing to clear.

// Send starts a turn with an uncomposed message. The controller applies
// plan-mode, memory, and background-job framing inside the async turn path.

// SendWithRaw starts a turn with separate model input and raw prompt text.

// planApprovalTool is the Tool name on the ApprovalRequest the controller emits
// to gate a proposed plan. Frontends key their plan-approval UI on it (the
// desktop renders a plan card; the chat TUI a plan banner).
const planApprovalTool = "exit_plan_mode"

// PlanDecisionAction preserves the three user-owned meanings of the Plan card.
// Revise and exit both deny execution at the approval gate, but they are not the
// same product decision and must remain distinguishable in durable receipts.
type PlanDecisionAction string

const (
	PlanDecisionStartExecution PlanDecisionAction = "start_execution"
	PlanDecisionRevisePlan     PlanDecisionAction = "revise_plan"
	PlanDecisionExitPlan       PlanDecisionAction = "exit_plan"
)

// SandboxEscapeApprovalTool is the internal Tool name used for one-shot approval
// to rerun a shell command without the OS sandbox after the sandbox failed.
const SandboxEscapeApprovalTool = "sandbox_escape"

// ManagedConfigWriteApprovalTool is the internal Tool name used for per-write
// approval when a file tool targets a Reasonix-managed config file outside the
// workspace write roots. It is a fresh human decision: config files control
// providers, sandbox rules, permissions, and MCP servers for future sessions,
// so YOLO/auto approval must never answer it.
const ManagedConfigWriteApprovalTool = "config_write"

// planApprovedMessage is the follow-up turn sent once the user approves a plan —
// the in-context nudge to execute and keep the (already-seeded) task list honest.
const planApprovedMessage = "Plan approved — plan mode is off. Implement the plan now. The ordinary writer fallback is approved for this execution turn; explicit ask/deny rules and forced fresh reviews still apply. Use this serial workflow: 1) mark the first sub-step in_progress with todo_write (this establishes the task list); 2) execute the sub-step; 3) call complete_step with evidence — the host then marks that sub-step completed and moves the next one to in_progress for you. Repeat 2–3 for each remaining sub-step. You don’t need another todo_write to mark steps completed; each complete_step advances the list. Sign off one sub-step at a time — never batch multiple completions."

func init() { midTurnSnapshotInterval.Store(int64(30 * time.Second)) }
