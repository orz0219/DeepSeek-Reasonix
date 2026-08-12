package agent

import (
	"context"
	"encoding/json"
	"strings"

	"reasonix/internal/sessiontemp"
	"reasonix/internal/tool"
)

// withSubagentSessionTemp installs a fresh session-private temporary directory
// Manager for one sub-agent run. The returned release must be deferred by the
// caller so the directory is retired when the run ends (including background
// sub-agent completion).
func withSubagentSessionTemp(ctx context.Context) (context.Context, func()) {
	m := sessiontemp.New()
	m.Retain()
	return sessiontemp.WithManager(ctx, m), m.Release
}

// DefaultTaskSystemPrompt steers a sub-agent toward focused, terse delivery —
// it doesn't see the parent's conversation so it must self-contain.
const DefaultTaskSystemPrompt = `You are a sub-agent invoked by a parent coding agent to carry out one focused task.
Use the provided tools to investigate or act. For MCP, use the stable use_capability
proxy (list → inspect → call); do not expect direct mcp__* tool schemas. Return a
single final answer that is concise and self-contained — the parent will see only
that answer, not your tool calls or reasoning. If you need to ask for clarification,
fail with a precise question instead of guessing.`

// DefaultReadOnlyTaskSystemPrompt steers read-only sub-agents toward isolated
// research. They never receive writer tools, persisted transcript controls, or
// background process controls, so their final answer is the only handoff.
const DefaultReadOnlyTaskSystemPrompt = `You are a read-only research sub-agent invoked by a parent coding agent.
Use only the provided read-only tools to inspect code, docs, history, and safe shell output.
For MCP, use use_capability only for authorized tools that declare readOnly and are
not destructive; never treat missing readOnlyHint as permission to call. Do not
attempt to write files, install capabilities, mutate memory, control long-lived
processes, or delegate to writer-capable agents. If a read-only delegation tool is
available and genuinely useful, you may use it within the configured depth limit.
Return a concise, self-contained final answer with the evidence the parent needs.`

const subagentStartContext = `<subagent-context event="SubagentStart">
Before acting, check the available skills and tools. If a relevant skill is available, invoke it before continuing. Delegate to another sub-agent only when the task genuinely benefits from isolated context and the delegation tool is available.
</subagent-context>`

// read_skill is deliberately not listed: it renders playbook text inline and
// cannot recurse, so depth-capped sub-agents keep it and can still read
// playbooks even when they can no longer delegate.
var subagentRecursiveTools = []string{
	"task",
	"read_only_task",
	"run_skill",
	"read_only_skill",
	"explore",
	"research",
	"review",
	"security_review",
}

var subagentAlwaysHiddenTools = []string{
	"parallel_tasks",
	"fleet",
	"read_subagent_result",
	"install_skill",
	"install_source",
}

var subagentJobTools = []string{
	"wait",
	"bash_output",
	"kill_shell",
}

var readOnlySubagentWorkflowTools = []string{
	"connect_tool_source",
}

const subagentToolBoundarySummary = "Recursive agent/skill tools are exposed only while max_subagent_depth leaves another delegation layer; unsupported background job tools (parallel_tasks, wait, bash_output, kill_shell) are excluded; bash is exposed as foreground-only inside subagents."

// maxConcurrentBackgroundTasks is the legacy writer-background fallback used
// only when a TaskTool has no session scheduler (tests). Production boots
// inject MaxParallelWriters via SubagentScheduler.
const maxConcurrentBackgroundTasks = DefaultMaxParallelWriters

// AlwaysHiddenSubagentTools returns the tool names excluded from every
// subagent's registry regardless of an explicit allowlist or delegation
// depth (unlike subagentRecursiveTools, which depends on remaining depth).
// That covers both subagentAlwaysHiddenTools and subagentJobTools —
// SubagentToolRegistryForDepth and its read-only variant strip the job tools
// unconditionally too. Host UIs offering a tool picker for a subagent
// profile's allowed-tools should exclude these from the offered choices —
// selecting them would be silently ignored at runtime.
func AlwaysHiddenSubagentTools() []string {
	names := append([]string(nil), subagentAlwaysHiddenTools...)
	return append(names, subagentJobTools...)
}

// SubagentMetaTools returns the tool names that spawned agents should not inherit
// from the parent registry unless a future call site deliberately opts into a
// different boundary. They can spawn or author more agent work, so excluding them
// preserves one layer of delegation without adding a spawn-count cap.
// read_skill stays listed here so the guardian and planner surfaces, which
// exclude these names, keep their provider-visible tool sets byte-identical —
// only the sub-agent depth cap deliberately stopped stripping it.
func SubagentMetaTools() []string {
	out := append([]string(nil), subagentRecursiveTools...)
	out = append(out, "read_skill")
	out = append(out, subagentAlwaysHiddenTools...)
	return out
}

// SubagentToolRegistry returns the tool set exposed inside spawned sub-agents:
// the requested whitelist (or every parent tool), minus meta tools that would
// spawn more agent work and job tools whose runtime manager is not injected into
// sub-agents. When bash is present, it is wrapped to advertise and allow only
// foreground execution.
func SubagentToolRegistry(parent *tool.Registry, names []string) *tool.Registry {
	return SubagentToolRegistryForDepth(parent, names, 1, 1)
}

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
func SubagentToolRegistryForDepth(parent *tool.Registry, names []string, childDepth, maxDepth int) *tool.Registry {
	return SubagentToolRegistryForDepthWithRuntime(parent, names, childDepth, maxDepth, nil)
}

// SubagentToolRegistryForDepthWithRuntime is SubagentToolRegistryForDepth with
// an optional session MCP runtime used when the parent registry has no
// use_capability (for example Economy or legacy callers) but sub-agents still
// need the proxy.
func SubagentToolRegistryForDepthWithRuntime(parent *tool.Registry, names []string, childDepth, maxDepth int, runtime *MCPCapabilityRuntime) *tool.Registry {
	exclude := append([]string(nil), subagentAlwaysHiddenTools...)
	if childDepth >= NormalizeMaxSubagentDepth(maxDepth) {
		exclude = append(exclude, subagentRecursiveTools...)
	}
	exclude = append(exclude, subagentJobTools...)
	sub := FilterRegistry(parent, names, exclude...)
	stripDirectMCPTools(sub)
	AttachCompleteSubtaskTool(sub)
	attachSubagentCapabilityProxy(parent, sub, names, runtime)
	if bash, ok := sub.Get("bash"); ok {
		sub.Add(foregroundOnlyBash{inner: bash})
	}
	return sub
}

type foregroundOnlyBash struct {
	inner tool.Tool
}

func (b foregroundOnlyBash) Name() string { return "bash" }

func (b foregroundOnlyBash) Description() string {
	desc := strings.TrimSpace(b.inner.Description())
	if desc == "" {
		desc = "Execute a command in the shell and return combined stdout/stderr."
	}
	desc = strings.Replace(desc, "Execute a command in the shell", "Execute a foreground command in the shell", 1)
	return desc + " Background execution is unavailable inside subagents."
}

func (foregroundOnlyBash) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Shell command to execute in the foreground"}},"required":["command"]}`)
}

func (b foregroundOnlyBash) ReadOnly() bool { return b.inner.ReadOnly() }

type readOnlyBash struct {
	inner tool.Tool
}

func (b readOnlyBash) Name() string { return "bash" }

func (b readOnlyBash) Description() string {
	desc := strings.TrimSpace(b.inner.Description())
	if desc == "" {
		desc = "Execute a command in the shell and return combined stdout/stderr."
	}
	desc = strings.Replace(desc, "Execute a command in the shell", "Execute a foreground read-only command in the shell", 1)
	return desc + " Only permission-classified read-only commands are allowed; shell operators, background execution, process preservation, and write-capable arguments are blocked."
}

func (readOnlyBash) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"command":{"type":"string","description":"Read-only shell command to execute in the foreground. Must match the permission-layer read-only command policy."}},"required":["command"]}`)
}

func (readOnlyBash) ReadOnly() bool { return true }
