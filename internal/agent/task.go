package agent

import (
	"encoding/json"
	"strings"

	"reasonix/internal/ablation"
	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
	"reasonix/internal/workspacelease"
)

// withSubagentSessionTemp installs a fresh session-private temporary directory
// Manager for one sub-agent run. The returned release must be deferred by the
// caller so the directory is retired when the run ends (including background
// sub-agent completion).

// DefaultTaskSystemPrompt steers a sub-agent toward focused, terse delivery —
// it doesn't see the parent's conversation so it must self-contain.

// DefaultReadOnlyTaskSystemPrompt steers read-only sub-agents toward isolated
// research. They never receive writer tools, persisted transcript controls, or
// background process controls, so their final answer is the only handoff.

// read_skill is deliberately not listed: it renders playbook text inline and
// cannot recurse, so depth-capped sub-agents keep it and can still read
// playbooks even when they can no longer delegate.

// maxConcurrentBackgroundTasks is the legacy writer-background fallback used
// only when a TaskTool has no session scheduler (tests). Production boots
// inject MaxParallelWriters via SubagentScheduler.

// AlwaysHiddenSubagentTools returns the tool names excluded from every
// subagent's registry regardless of an explicit allowlist or delegation
// depth (unlike subagentRecursiveTools, which depends on remaining depth).
// That covers both subagentAlwaysHiddenTools and subagentJobTools —
// SubagentToolRegistryForDepth and its read-only variant strip the job tools
// unconditionally too. Host UIs offering a tool picker for a subagent
// profile's allowed-tools should exclude these from the offered choices —
// selecting them would be silently ignored at runtime.

// SubagentMetaTools returns the tool names that spawned agents should not inherit
// from the parent registry unless a future call site deliberately opts into a
// different boundary. They can spawn or author more agent work, so excluding them
// preserves one layer of delegation without adding a spawn-count cap.
// read_skill stays listed here so the guardian and planner surfaces, which
// exclude these names, keep their provider-visible tool sets byte-identical —
// only the sub-agent depth cap deliberately stopped stripping it.

// SubagentToolRegistry returns the tool set exposed inside spawned sub-agents:
// the requested whitelist (or every parent tool), minus meta tools that would
// spawn more agent work and job tools whose runtime manager is not injected into
// sub-agents. When bash is present, it is wrapped to advertise and allow only
// foreground execution.

// SubagentToolRegistryForDepth returns the writer-capable tool set for a spawned
// subagent at childDepth. Recursive delegation tools are available only when the
// child still has room to spawn one more subagent.
//
// Direct mcp__* schemas are never exposed: MCP goes only through the fixed
// use_capability proxy so connect/disconnect/tool-list churn cannot change the
// child provider-visible tool prefix. With no explicit allowlist the child gets
// the full proxy (installed/authorized MCP, including tools without
// readOnlyHint). An explicit allowlist converts mcp__* / mcp-tool: names into a
// capability-id allowlist on a restricted proxy.

// SubagentToolRegistryForDepthWithRuntime is SubagentToolRegistryForDepth with
// an optional session MCP runtime used when the parent registry has no
// use_capability (for example Economy or legacy callers) but sub-agents still
// need the proxy.

// TaskTool spawns a sub-agent in its own session for a focused sub-task. The
// sub-agent runs with a filtered tool whitelist and the same step budget shape
// as the parent (see Execute); its tool calls are forwarded to the parent's
// event stream nested under this call, while only its final assistant message is
// returned to the parent model. Use cases: keep noisy tool sequences (multi-file
// exploration, repeated grep / read_file) out of the parent's context budget, or
// parallel research across independent areas (the parallel-dispatch path picks
// these up only when readOnly, which task is not).
type TaskTool struct {
	prov                          provider.Provider
	pricing                       *provider.Pricing
	parentReg                     *tool.Registry
	maxSteps                      int
	contextWindow                 int
	compactRatio                  float64
	recentKeep                    int
	temperature                   float64
	archiveDir                    string
	keepPolicy                    KeepPolicy
	sysPrompt                     string
	gate                          Gate
	subagentModel, subagentEffort string
	resolveProvider               func(modelRef, effort string) (provider.Provider, *provider.Pricing, int, error)
	transcripts                   *SubagentStore
	workspaceRoot                 string
	baseModel                     string
	baseEffort                    string
	identityProfile               func(modelRef, effort string) (string, string)
	maxSubagentDepth              int
	deliveryProfile               bool
	ablation                      ablation.Set
	workspaceLease                *workspacelease.Owner
	// scheduler is the session-scoped concurrency + write-claim controller.
	// nil falls back to the legacy jobs.ReserveStart cap for background tasks.
	scheduler *SubagentScheduler
	// profileLookup resolves profile= names from the live Skill store without
	// embedding the name list in the tool schema (cache stability).
	profileLookup ProfileLookup
	// profileConfigModel/Effort look up persistent per-profile overrides
	// (agent.subagent_models / subagent_efforts).
	profileConfigModel  func(profile string) string
	profileConfigEffort func(profile string) string
	// bashSandboxEnforced reports whether OS sandbox can honour write roots
	// for bash inside path-bound writer sub-agents.
	bashSandboxEnforced func() bool
	// mutationObserver is shared with spawned sub-agents for checkpoint capture.
	mutationObserver *checkpoint.MutationObserver
	// recoveryGate is the shared Auto Guard boundary for
	// this session (root + sub-agents). nil disables recovery in children.
	recoveryGate RecoveryGate
	// capabilityRuntime is the session-shared MCP Host/specs substrate. Each
	// sub-agent gets its own use_capability frontend so ledger state stays
	// isolated while connections reuse the parent Host.
	capabilityRuntime *MCPCapabilityRuntime
}

// TaskToolOptions holds the construction parameters for a TaskTool.
// Prefer NewTaskToolWithOptions for new call sites; the positional NewTaskTool
// remains as a compatibility wrapper for one full iteration cycle.
type TaskToolOptions struct {
	Provider                              provider.Provider
	Pricing                               *provider.Pricing
	ParentRegistry                        *tool.Registry
	MaxSteps                              int
	ContextWindow                         int
	RecentKeep                            int
	SoftCompactRatio                      float64
	ToolResultSnipRatio                   float64
	CompactRatio                          float64
	CompactForceRatio                     float64
	Temperature                           float64
	ContextEditing, ArchiveDir, SysPrompt string
	Gate                                  Gate
	KeepPolicy                            KeepPolicy
	SubagentModel                         string
	SubagentEffort                        string
	ResolveProvider                       func(string, string) (provider.Provider, *provider.Pricing, int, error)
}

// NewTaskToolWithOptions is the internal standard constructor for TaskTool.
// An empty SysPrompt still resolves to DefaultTaskSystemPrompt. No extra
// validation or default overrides are applied beyond the historical NewTaskTool
// behavior.
func NewTaskToolWithOptions(opts TaskToolOptions) *TaskTool {
	sysPrompt := opts.SysPrompt
	if sysPrompt == "" {
		sysPrompt = DefaultTaskSystemPrompt
	}
	return &TaskTool{
		prov:             opts.Provider,
		pricing:          opts.Pricing,
		parentReg:        opts.ParentRegistry,
		maxSteps:         opts.MaxSteps,
		contextWindow:    opts.ContextWindow,
		recentKeep:       opts.RecentKeep,
		compactRatio:     opts.CompactRatio,
		temperature:      opts.Temperature,
		archiveDir:       opts.ArchiveDir,
		keepPolicy:       opts.KeepPolicy,
		sysPrompt:        sysPrompt,
		gate:             opts.Gate,
		subagentModel:    opts.SubagentModel,
		subagentEffort:   opts.SubagentEffort,
		resolveProvider:  opts.ResolveProvider,
		maxSubagentDepth: DefaultMaxSubagentDepth,
	}
}

// NewTaskTool wires a task tool to the parent agent's environment so its
// sub-agents can use the same provider and tools. sysPrompt is the system
// prompt every sub-agent starts with; pass "" for DefaultTaskSystemPrompt. gate
// is the permission gate sub-agents inherit — pass the headless variant so
// deny rules still bite while autonomous sub-agents are never blocked on an
// interactive prompt (there is no UI to answer one).
//
// Compatibility wrapper: new call sites should prefer NewTaskToolWithOptions.
// The positional form is kept for at least one full iteration cycle.
func NewTaskTool(prov provider.Provider, pricing *provider.Pricing, parentReg *tool.Registry,
	maxSteps, contextWindow, recentKeep int, softCompactRatio, toolResultSnipRatio, compactRatio, compactForceRatio, temperature float64, archiveDir, sysPrompt string, gate Gate,
	keepPolicy KeepPolicy, subagentModel, subagentEffort string, resolveProvider func(string, string) (provider.Provider, *provider.Pricing, int, error)) *TaskTool {
	return NewTaskToolWithOptions(TaskToolOptions{
		Provider:            prov,
		Pricing:             pricing,
		ParentRegistry:      parentReg,
		MaxSteps:            maxSteps,
		ContextWindow:       contextWindow,
		RecentKeep:          recentKeep,
		SoftCompactRatio:    softCompactRatio,
		ToolResultSnipRatio: toolResultSnipRatio,
		CompactRatio:        compactRatio,
		CompactForceRatio:   compactForceRatio,
		Temperature:         temperature,
		ArchiveDir:          archiveDir,
		SysPrompt:           sysPrompt,
		Gate:                gate,
		KeepPolicy:          keepPolicy,
		SubagentModel:       subagentModel,
		SubagentEffort:      subagentEffort,
		ResolveProvider:     resolveProvider,
	})
}

// WithTranscripts enables persisted sub-agent transcript continuation for this
// task tool. The base model/effort are the parent provider identity used when no
// subagent override is configured.
func (t *TaskTool) WithTranscripts(store *SubagentStore, workspaceRoot, baseModel, baseEffort string) *TaskTool {
	t.transcripts = store
	t.workspaceRoot = strings.TrimSpace(workspaceRoot)
	t.baseModel = strings.TrimSpace(baseModel)
	t.baseEffort = strings.TrimSpace(baseEffort)
	return t
}

func (t *TaskTool) WithTranscriptIdentityResolver(resolve func(modelRef, effort string) (string, string)) *TaskTool {
	t.identityProfile = resolve
	return t
}

func (t *TaskTool) WithMaxSubagentDepth(depth int) *TaskTool {
	t.maxSubagentDepth = NormalizeMaxSubagentDepth(depth)
	return t
}

// WithDeliveryProfile propagates the parent's runtime delivery contract into
// writer-capable sub-agents. Read-only sub-agents may receive the flag too, but
// the mutation gate remains dormant for them.
func (t *TaskTool) WithDeliveryProfile(enabled bool) *TaskTool {
	t.deliveryProfile = enabled
	return t
}

// WithAblation propagates the parent's benchmark arm so a sub-agent runs with
// the same subsystems switched off.
func (t *TaskTool) WithAblation(set ablation.Set) *TaskTool {
	t.ablation = set
	return t
}

// WithWorkspaceLease shares the parent's workspace-wide delivery write lease
// with every spawned sub-agent. A shared owner is required: independent owners
// in one session would deadlock when a child tries to write while its parent
// already retains the lease.
func (t *TaskTool) WithWorkspaceLease(owner *workspacelease.Owner) *TaskTool {
	t.workspaceLease = owner
	return t
}

// WithScheduler attaches the session-scoped concurrency and write-claim
// controller used by task, fleet, parallel_tasks, and profile skill runners.
func (t *TaskTool) WithScheduler(s *SubagentScheduler) *TaskTool {
	t.scheduler = s
	return t
}

// Scheduler returns the attached session scheduler (may be nil in unit tests).
func (t *TaskTool) Scheduler() *SubagentScheduler {
	if t == nil {
		return nil
	}
	return t.scheduler
}

// WithProfileLookup enables task/fleet profile= resolution from the Skill store.
func (t *TaskTool) WithProfileLookup(lookup ProfileLookup) *TaskTool {
	t.profileLookup = lookup
	return t
}

// WithProfileConfigResolvers supplies persistent per-profile model/effort
// overrides (agent.subagent_models / subagent_efforts).
func (t *TaskTool) WithProfileConfigResolvers(model, effort func(profile string) string) *TaskTool {
	t.profileConfigModel = model
	t.profileConfigEffort = effort
	return t
}

// WithBashSandboxEnforced tells path-bound writer runs whether bash can keep
// the same write roots under the OS sandbox.
func (t *TaskTool) WithBashSandboxEnforced(fn func() bool) *TaskTool {
	t.bashSandboxEnforced = fn
	return t
}

// WithCapabilityRuntime attaches the session-shared MCP runtime so ordinary and
// read-only sub-agents receive a stable use_capability frontend without
// inheriting dynamic mcp__* schemas.
func (t *TaskTool) WithCapabilityRuntime(rt *MCPCapabilityRuntime) *TaskTool {
	if t != nil {
		t.capabilityRuntime = rt
	}
	return t
}

func (t *TaskTool) Name() string { return "task" }

func (t *TaskTool) Description() string {
	return "Spawn a sub-agent for a focused sub-task. Optional profile selects a runAs=subagent Skill whose body becomes the full system prompt (no implicit concise default). Optional write_paths declare non-overlapping write targets so background writers may run in parallel; omitting write_paths on a writer claims the whole workspace and serializes writers. The sub-agent runs in its own session with a filtered tool list (defaults to every parent tool, then applies the subagent boundary: " + subagentToolBoundarySummary + "). Only its final answer is returned."
}

func (t *TaskTool) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "prompt":{"type":"string","description":"What the sub-agent should accomplish. Be specific about the deliverable — the sub-agent does not see this conversation."},
  "description":{"type":"string","description":"Short label for the sub-task (3-7 words). Surfaced in the dispatch line so the user sees what's running."},
  "profile":{"type":"string","description":"Optional runAs=subagent profile name. Resolved at runtime from the Skill store; explicit names may invoke invocation=manual profiles. The profile body becomes the full system prompt."},
  "write_paths":{"type":"array","items":{"type":"string"},"description":"Optional workspace-relative or absolute file/directory paths this writer may modify. Globs and workspace escapes are rejected. Writers without write_paths claim the whole workspace (serializing against every other writer claim). Non-overlapping paths allow parallel writers up to max_parallel_writers. In fleet, multiple whole-workspace claims fail preflight before any task starts."},
  "tools":{"type":"array","items":{"type":"string"},"description":"Optional tool whitelist. When profile sets allowed-tools, this list is intersected (call args cannot expand profile permissions). ` + subagentToolBoundarySummary + `"},
  "max_steps":{"type":"integer","description":"Optional cap on tool-call rounds. Defaults to half the parent's cap (min 5).","minimum":1},
  "run_in_background":{"type":"boolean","description":"Run the sub-agent asynchronously: returns a job id immediately and keeps working across turns. Collect its final answer with wait, and you'll be notified when it finishes. Use for long, independent sub-tasks you don't need to block on right now."},
  "model":{"type":"string","description":"Optional model override for the sub-agent (a configured provider/model name). Precedence: persistent profile config, this argument, profile frontmatter, global subagent default, parent model."},
  "effort":{"type":"string","description":"Optional reasoning effort for the sub-agent (e.g. high, max). Same precedence as model."},
  "continue_from":{"type":"string","description":"Continue a prior compatible subagent transcript in the current conversation context. Pass only the 'sa_...' value from the prior result's 'Subagent reference: ...' line. If the ref belongs to an ancestor conversation, the framework continues a current-conversation copy."}
},
"required":["prompt"]
}`)
}

// ReadOnly is false: a sub-agent can invoke any whitelisted tool, including
// writers. Conservative classification keeps the parallel-dispatch path from
// running two sub-agents at once and letting their writes race.
func (t *TaskTool) ReadOnly() bool { return false }

// ResolveProfile extracts model/effort from task args (and optional profile
// overrides) for dispatch-line display. Runtime execution re-resolves with the
// full precedence chain.
func (t *TaskTool) ResolveProfile(args json.RawMessage) *event.Profile {
	var p struct {
		Model   string `json:"model"`
		Effort  string `json:"effort"`
		Profile string `json:"profile"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil
	}
	profileModel, profileEffort := "", ""
	configModel, configEffort := "", ""
	if name := strings.TrimSpace(p.Profile); name != "" {
		if def, err := ResolveProfileDefinition(t.profileLookup, name); err == nil {
			profileModel, profileEffort = def.Model, def.Effort
		}
		if t.profileConfigModel != nil {
			configModel = t.profileConfigModel(name)
		}
		if t.profileConfigEffort != nil {
			configEffort = t.profileConfigEffort(name)
		}
	}
	model, effort := ResolveModelEffort(
		configModel, configEffort,
		p.Model, p.Effort,
		profileModel, profileEffort,
		t.subagentModel, t.subagentEffort,
	)
	if model == "" && effort == "" {
		return nil
	}
	return &event.Profile{Model: model, Effort: effort}
}

// ReadOnlyTaskTool runs an isolated sub-agent with a strictly read-only tool
// registry. It intentionally omits background execution and transcript
// continuation/fork controls so the call has no durable host side effects.
type ReadOnlyTaskTool struct {
	task *TaskTool
}

func NewReadOnlyTaskTool(task *TaskTool) *ReadOnlyTaskTool {
	return &ReadOnlyTaskTool{task: task}
}

func (*ReadOnlyTaskTool) Name() string { return "read_only_task" }

func (*ReadOnlyTaskTool) Description() string {
	return "Spawn a read-only research sub-agent for a focused investigation. The sub-agent runs in an isolated, ephemeral session with read-only tools only; bash is wrapped to allow only permission-classified foreground read-only commands. It cannot write files, install capabilities, mutate memory, run background jobs, continue/fork transcripts, or delegate to writer-capable agents. Read-only nested delegation may be available until max_subagent_depth is reached. Only its final answer is returned."
}

func (*ReadOnlyTaskTool) Schema() json.RawMessage {
	return json.RawMessage(`{
"type":"object",
"properties":{
  "prompt":{"type":"string","description":"What the read-only sub-agent should investigate. Be specific about the evidence or summary to return — the sub-agent does not see this conversation."},
  "description":{"type":"string","description":"Short label for the read-only sub-task (3-7 words). Surfaced in the dispatch line so the user sees what's running."},
  "tools":{"type":"array","items":{"type":"string"},"description":"Optional read-only tool whitelist. Writer, installer, memory mutation, background job, and delegation tools are never exposed."},
  "max_steps":{"type":"integer","description":"Optional cap on tool-call rounds. Defaults to half the parent's cap (min 5).","minimum":1},
  "model":{"type":"string","description":"Optional model override for the sub-agent (a configured provider/model name)."},
  "effort":{"type":"string","description":"Optional reasoning effort for the sub-agent (e.g. high, max)."}
},
"required":["prompt"]
}`)
}

func (*ReadOnlyTaskTool) ReadOnly() bool { return true }

// PlanModeSafe reports true: read_only_task spawns a strictly read-only research
// sub-agent (no writers, installers, memory mutation, background jobs, or
// delegation), so it is safe to run while planning.
func (*ReadOnlyTaskTool) PlanModeSafe() bool { return true }

func (r *ReadOnlyTaskTool) ResolveProfile(args json.RawMessage) *event.Profile {
	if r == nil || r.task == nil {
		return nil
	}
	return r.task.ResolveProfile(args)
}

// Every entry point compiles to a spec and runs through RunProfileSpec, so a
// boundary added there cannot be missed by one caller. read_only_task keeps
// its own promise of no durable side effects through Ephemeral.

// buildTaskSpec resolves profile, tools, model/effort, and write claims for a
// single task/fleet item. forceReadOnly forces the read-only registry.

// Every writer carries a claim. Omitting write_paths conservatively claims
// the whole workspace, including foreground task calls, so they cannot
// bypass an already-running background/fleet writer claim. Direct legacy
// TaskTool constructions without a workspace/scheduler keep their old
// no-claim behavior; production boot always configures both.

// RunProfileSpec executes a unified profile/task specification. Shared by task,
// fleet items, and boot-wired skill runners so prompt, tools, claims, and
// scheduling cannot drift across entry points.

// Per-child progress tracker: converts the child's reasoning/text/notice/
// retrying into reserved ToolProgress previews and guarantees exactly one
// terminal status (completed/cancelled/failed). The background job owns
// finish after handoff; every other exit finishes here, including
// validation errors and panics.

// Explicit paths are an execution boundary and rebind/drop tools that
// cannot honor it. A synthesized whole-workspace claim is a scheduling
// boundary for omitted write_paths; it preserves the legacy registry and
// the parent session's existing sandbox/permission boundaries.

// Defensive fallback for callers that manually construct a background spec
// instead of going through buildTaskSpec.

// Legacy hard-cap remains only when no scheduler is attached. With a
// scheduler, return the job immediately and queue for a slot inside the
// job so the parent turn is not blocked at concurrency limits.

// Capture acquire request by value for the job goroutine.

// Emit queued before the job goroutine can start so the status slot
// never regresses to a stale queued after running.

// The job owns the terminal status: the parent tool call has
// already returned its job id by now.

// Queue for a concurrency/write slot here — not before Start —
// so the parent tool call returns a job id immediately.

// Hand the tracker to the job goroutine: the outer defer must not
// finish (and close) it while the job still runs.

// Foreground: acquire a slot (queue if needed), then run synchronously.

// usageModelRef returns the canonical provider/model identity of the runtime
// selected for a child. The resolver expands aliases and supplies the parent
// model when no child override is configured.

// buildSubReg returns the sub-agent's tool set: the named whitelist (minus
// unavailable sub-agent tools), or every parent tool except those tools.

// FilterRegistry builds a sub-registry from parent: the named whitelist (empty =
// every parent tool), minus any excluded names. Used to scope what a spawned
// sub-agent — a `task` sub-agent or a subagent skill — may call, e.g. excluding
// `task` to bar recursive nesting, or restricting to a skill's allowed-tools.
// Direct MCP tools may be copied here; callers that need a stable MCP surface
// should strip them and attach use_capability via attachSubagentCapabilityProxy.

// MCP never enters through the generic filter when named as capability
// ids; model-visible mcp__* may still be listed for conversion later.

// stripDirectMCPTools removes provider-visible mcp__* tools so sub-agents use
// only the stable use_capability proxy for MCP.

// restrictedCapabilityProxy preserves a subagent allowed-tools boundary when
// MCP is available only through use_capability. The pseudo mcp-tool: and
// mcp-server: entries never become provider tools; they select one proxy schema
// whose resolver rejects every capability outside the exact allowlist.
//
// Provider-visible name/description/schema stay identical to the unrestricted
// proxy so allowlist expansion never changes the child cache prefix. Allowlist
// enforcement is host-local (check + filtered list results).

// servers is the set of MCP server names implied by allowed IDs; list
// results are filtered to this set so profile isolation covers discovery.

// Description is fixed: never embed dynamic capability IDs (they change with
// MCP install/tool-list and would break the stable provider tool prefix).

// emptyCapabilityListResult is the fail-closed list payload: no server metadata.

// filterCapabilityListResult keeps only servers in the allowlist for restricted
// proxies. Empty allowlist or unreadable payloads fail closed (empty server
// list) so discovery never leaks the full configured MCP inventory.

// validMCPServerCapabilityID accepts mcp-server:<non-empty-name> only.

// Reject empty and path-like fragments that are not bare server names.

// validMCPToolCapabilityID accepts mcp-tool:<server>/<tool> with both parts non-empty.

// attachSubagentCapabilityProxy installs a per-agent use_capability frontend.
// Any parent-copied proxy is replaced so children never share Executor ledger
// state. No allowlist → full proxy. Explicit allowlist with MCP names →
// restricted proxy. Explicit "use_capability" → full proxy. Explicit allowlist
// without MCP entries → no proxy.

// Drop any provider-copied use_capability so we always install an isolated
// frontend (shared Host/runtime, independent ledger/audit).

// Custom allowlist with no valid MCP entries: do not expose the proxy.

// Incomplete capability IDs produced an empty server set: fail closed
// rather than installing a restricted proxy that would list everything.

// mcpCapabilityAllowlist converts profile/call tool names into capability IDs
// for the restricted use_capability proxy. Accepts complete mcp-tool:<s>/<t>,
// mcp-server:<s>, model-visible mcp__* names, and wildcards expanded against
// the parent. Incomplete prefixes such as "mcp-server:" or "mcp-tool:foo" are
// rejected so they cannot install a restricted proxy with an empty server set.

// Explicit proxy grant is handled as a full frontend by the caller
// when this is the only MCP-related entry; leave empty here so a
// bare use_capability allowlist entry still installs unrestricted.

// ReadOnlySubagentToolRegistry returns the tool set exposed to read-only
// sub-agents: read-only research tools plus a bash wrapper that enforces the
// permission-layer read-only command policy at execution time. Workflow/meta tools are
// excluded even when their Tool.ReadOnly contract is true.

// ReadOnlySubagentToolRegistryForDepth returns the tool set exposed to read-only
// subagents. It permits only read-only delegation tools while another depth
// layer is available. Direct mcp__* schemas are never exposed; MCP goes only
// through use_capability. Dynamic execution still requires authorized server +
// readOnlyHint + non-destructive (enforced by ReadOnlyExecution), so strict
// agents share the stable proxy schema and connection reuse without permission
// relaxation.
//
// Custom profile/call allowlists remain authoritative and convert MCP names
// into a capability-id allowlist on a restricted proxy.

// ReadOnlySubagentToolRegistryForDepthWithRuntime is the read-only registry
// builder with an optional session MCP runtime for proxy injection.

// Direct MCP never enters the strict registry — use_capability only.

// expandToolPatterns resolves explicit wildcard allowlist entries from imported
// agent profiles against the current registry. Expansion is deterministic and
// session-local, so optional MCP tools only enter a child after connection.

// FilterReadOnlyRegistry builds a sub-registry containing only tools whose
// ReadOnly contract is true, minus explicit exclusions. MCP tools must
// additionally come from an authorized server and must not carry
// destructiveHint.

// Capture the pristine task before host framing is prepended: delivery
// intent classification must judge the task, not the wrapper.

// The child provider owns the final vision decision. Text-only providers
// retain the attachment metadata but omit image parts during serialization.

// Capture the pristine task before host framing is prepended: delivery
// intent classification must judge the task, not the wrapper.

// subagentOptions is the single construction point for the run options every
// sub-agent spawned through this tool shares (task, read_only_task, and
// parallel_tasks children). Compaction, language preferences, and depth limits
// must stay uniform across those paths — add new fields here, not at call sites.

// WithRecoveryGate shares Auto Guard with spawned sub-agents.

// WithMutationObserver shares the host mutation observer with spawned sub-agents.
// Foreground children inherit the parent ownership turn; background children
// keep the turn that spawned them (set via OwnershipTurn at Begin).

// Wording note: avoid incidental action verbs ("resolve", "fix", …) in this
// host framing — it is prepended to every sub-agent prompt and must never
// read as task intent (see classifierTaskText, which also strips it).

// GuardSubagentHostDecisionText appends a fixed boundary warning only when a
// child agent result appears to discuss host approval or user-owned decisions.
// The implementation lives in internal/tool so the skill tools share the exact
// same phrase list and notice.

// maxReviewReportNudges bounds the in-session completion nudges sent to a
// review subagent that finished without submitting review_report. Each nudge is
// one cheap continuation request on the same (cached) subagent session — far
// cheaper than discarding the run and re-reviewing from scratch.
// maxReviewReportNudges is the single in-session retry after the first failed
// review run (plan: fail once, retry once). A second failure becomes Partial.

// reviewReportTaskContract is appended to the task prompt of a review subagent
// whose run must end with a typed report. The skill body describes how to
// review; this states the non-negotiable submission protocol.

// reviewReportNudgePrompt asks an already-finished review subagent to submit
// the missing typed report without redoing the review.

// RunSubAgentWithSession continues an existing sub-agent session with prompt and
// returns the latest final assistant answer. Fresh sub-agents pass a newly-created
// session; continued sub-agents pass a loaded transcript session.
//
// Each call installs an independent session-private temporary directory Manager
// so parent, sibling, and nested sub-agents never share temporary files.
// continue_from restores conversation history only — a new run still gets a
// fresh temporary directory.

// Isolate temporary files for this run before any tool execution.

// Callers that wrap the prompt themselves (runSubSession) set
// ClassifierTaskText before wrapping; for everyone else the prompt is
// still pristine here, so capture it before host framing is prepended.

// Nested reasoning stays isolated; the parent consumes only final Content.
// Require it so a reasoning-only stop cannot fall back to older tool text.

// Still merge any partial child evidence so parent gates see real writes.

// Review/security subagents must hand back a typed report the parent's
// delivery gate can verify; prose alone would leave the gate demanding a
// review forever with no way to tell why it never arrives. A run that
// finished without the report gets bounded completion nudges on the same
// session (evidence preserved, so review_report can still cite the reads it
// already earned) before the whole run is declared failed.

// A retry that fails still keeps local parent mutations; the
// parent turns this into Partial/Unverified rather than rolling back.

// Partial path: local changes are retained; the parent readiness
// layer treats missing review as Partial/Unverified (not rollback).

// readOnlyAgentConstruction is the single pairing every strictly read-only
// loop shares: the permanent ReadOnlyExecution flag plus the final registry
// filter. Batch children (RunReadOnlySubAgentWithSession) and legacy call sites
// that still use NewReadOnlyAgent build through it, so a missed call site
// cannot set only half the boundary. The interactive two-model planner uses
// NewPlannerAgent instead (PlannerMCPExecution).

// NewReadOnlyAgent constructs a long-lived, strictly read-only agent through
// the shared construction boundary. Prefer NewPlannerAgent for the two-model
// planner so authorized non-destructive MCP can run via use_capability.

// NewPlannerAgent constructs the interactive two-model planner: permanent
// ReadOnlyExecution still blocks bash, file writers, and ordinary non-MCP
// writers, while PlannerMCPExecution allows authorized, non-destructive MCP
// through the stable use_capability proxy without requiring readOnlyHint.

// The coordinator needs visible plan text to hand off to the executor;
// reasoning shown in a frontend is not a substitute for that contract.

// Keep construction-time filter for ordinary tools; use_capability stays
// because it is ReadOnly. Direct mcp__* tools are already excluded by
// PlannerToolRegistry. Dynamic MCP targets are re-checked after resolve.

// plannerExecutionRegistry is the construction-time filter for NewPlannerAgent.
// It removes ordinary writers and destructive direct MCP tools while keeping
// use_capability and built-in research tools. Host-starting deferred MCP
// targets are allowed at execution time under PlannerMCPExecution.

// Defense in depth: planner never exposes direct MCP schemas.

// Ordinary host mutations stay out; MCP startup is only via proxy.

// RunReadOnlySubAgentWithSession is the construction boundary for every
// strictly read-only child loop. Registry filtering limits the visible surface;
// this permanent execution flag also re-checks targets resolved dynamically by
// proxy tools such as use_capability. It never enables PlannerMCPExecution.

// strictReadOnlyExecutionRegistry is the final construction-time filter shared
// by every strict child. Callers still apply role-specific filtering (review,
// planner, profile allowlists), while this layer guarantees that a missed call
// site cannot expose writers, destructive MCP tools, readers from unauthorized
// servers, or an unauthorized host-starting target to the model.

// latestAssistantAnswer walks the session backwards for the last assistant
// message with content — that's the sub-agent's final answer. Intermediate
// assistant messages with tool_calls but no text don't count.

// dumpFailedSubagentSession best-effort persists a failed report-required
// subagent transcript for post-hoc diagnosis (read-only skill subagents are
// otherwise ephemeral, so a protocol failure leaves no trace). Returns a
// human-readable suffix naming the dump, or "" when disabled/failed.

// mergeChildEvidence folds a sub-agent's real receipts into the parent ledger
// carried on ctx. Meta tools themselves are never mutations.

// EvidenceSummary exports this agent's turn-scoped receipts for parent merge.

// NestedSink returns a sink that forwards a sub-agent's tool activity to the
// parent stream, nested under the tool call carried by ctx, so a frontend shows
// it beneath that call (the same nesting `task` uses). Falls back to the given
// sink when ctx carries no call context. Used by subagent skills.

// subSink forwards a sub-agent's tool dispatch/result/progress events and
// billable usage to the parent's event stream. Only tool activity is nested
// visually; the sub-agent's text/reasoning stays isolated (progress previews
// travel as reserved ToolProgress channels, not as parent Text/Reasoning) and
// only its final answer is returned.
//
// The sub-agent's own turn/text/reasoning events are dropped — forwarding them
// would make the parent transcript noisy and could imply they belong to the
// parent model context, which they do not.
//
// Usage events are observability only, so forwarding them preserves billing
// totals without polluting the parent provider-visible prefix.
//
// Tool events are tagged with the parent task call's ID so a frontend nests them
// under it. The forwarded call IDs are namespaced with the parent ID so a
// sub-agent call can never collide with a parent call in the frontend's
// dispatch→result matching. ToolProgress covers both the sub-agent's real tool
// output and nested sub-agent progress previews, which ride the same sink so
// their IDs match the cards they belong to. Falls back to Discard when there's
// no parent stream (the headless run loop, or a direct Execute in tests).

// subSinkFor builds the nesting sink from an already-captured parent ID + stream,
// for the background path where the job runs under a context that no longer
// carries the call context. Falls back to Discard when there's no parent stream.
