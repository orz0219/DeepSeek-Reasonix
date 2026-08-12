package agent

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"reasonix/internal/checkpoint"
	"reasonix/internal/event"
	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/tool"
)

// ReadOnlySubagentToolRegistry returns the tool set exposed to read-only
// sub-agents: read-only research tools plus a bash wrapper that enforces the
// permission-layer read-only command policy at execution time. Workflow/meta tools are
// excluded even when their Tool.ReadOnly contract is true.
func ReadOnlySubagentToolRegistry(parent *tool.Registry, names []string) *tool.Registry {
	return ReadOnlySubagentToolRegistryForDepth(parent, names, 1, 1)
}

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
func ReadOnlySubagentToolRegistryForDepth(parent *tool.Registry, names []string, childDepth, maxDepth int) *tool.Registry {
	return ReadOnlySubagentToolRegistryForDepthWithRuntime(parent, names, childDepth, maxDepth, nil)
}

// ReadOnlySubagentToolRegistryForDepthWithRuntime is the read-only registry
// builder with an optional session MCP runtime for proxy injection.
func ReadOnlySubagentToolRegistryForDepthWithRuntime(parent *tool.Registry, names []string, childDepth, maxDepth int, runtime *MCPCapabilityRuntime) *tool.Registry {
	exclude := append([]string(nil), subagentAlwaysHiddenTools...)
	if childDepth >= NormalizeMaxSubagentDepth(maxDepth) {
		exclude = append(exclude, subagentRecursiveTools...)
	} else {
		exclude = append(exclude, "task", "run_skill", "explore", "research", "review", "security_review")
	}
	exclude = append(exclude, subagentJobTools...)
	exclude = append(exclude, plannerNonResearchTools...)
	exclude = append(exclude, readOnlySubagentWorkflowTools...)
	ex := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		ex[e] = true
	}
	sub := tool.NewRegistry()
	if parent == nil {
		return sub
	}
	src := names
	if len(src) == 0 {
		src = parent.Names()
	} else {
		src = expandToolPatterns(parent, src)
	}
	for _, name := range src {
		if ex[name] {
			continue
		}
		if strings.HasPrefix(name, "mcp-tool:") || strings.HasPrefix(name, "mcp-server:") {
			continue
		}
		tl, ok := parent.Get(name)
		if !ok {
			continue
		}
		if name == "bash" {
			sub.Add(readOnlyBash{inner: tl})
			continue
		}

		if isInstalledMCPTool(tl) || strings.HasPrefix(name, tool.MCPNamePrefix) {
			continue
		}
		if !tl.ReadOnly() {
			continue
		}
		sub.Add(tl)
	}
	attachSubagentCapabilityProxy(parent, sub, names, runtime)
	return sub
}

// expandToolPatterns resolves explicit wildcard allowlist entries from imported
// agent profiles against the current registry. Expansion is deterministic and
// session-local, so optional MCP tools only enter a child after connection.
func expandToolPatterns(parent *tool.Registry, names []string) []string {
	if parent == nil {
		return nil
	}
	available := parent.Names()
	seen := map[string]bool{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		if !strings.ContainsAny(name, "*?[") {
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
			continue
		}
		for _, candidate := range available {
			matched, err := filepath.Match(name, candidate)
			if err == nil && matched && !seen[candidate] {
				seen[candidate] = true
				out = append(out, candidate)
			}
		}
	}
	return out
}

// FilterReadOnlyRegistry builds a sub-registry containing only tools whose
// ReadOnly contract is true, minus explicit exclusions. MCP tools must
// additionally come from an authorized server and must not carry
// destructiveHint.
func FilterReadOnlyRegistry(parent *tool.Registry, exclude ...string) *tool.Registry {
	ex := make(map[string]bool, len(exclude))
	for _, e := range exclude {
		ex[e] = true
	}
	sub := tool.NewRegistry()
	if parent == nil {
		return sub
	}
	for _, name := range parent.Names() {
		if ex[name] {
			continue
		}
		tl, ok := parent.Get(name)
		if !ok || !tl.ReadOnly() {
			continue
		}
		if isInstalledMCPTool(tl) && (!mcpServerAuthorized(tl) || mcpDestructiveHint(tl)) {
			continue
		}
		sub.Add(tl)
	}
	return sub
}

func (t *TaskTool) resolveSubSessionRuntime(modelRef, effort string) (provider.Provider, *provider.Pricing, int, error) {
	prov, pricing, ctxWin := t.prov, t.pricing, t.contextWindow
	if t.resolveProvider != nil && (modelRef != "" || effort != "") {
		p, pr, cw, err := t.resolveProvider(modelRef, effort)
		if err != nil {
			return nil, nil, 0, err
		}
		prov, pricing, ctxWin = p, pr, cw
	}
	return prov, pricing, ctxWin, nil
}

func (t *TaskTool) runSubSession(ctx context.Context, prompt string, subReg *tool.Registry, sink event.Sink, maxSteps int, prov provider.Provider, pricing *provider.Pricing, ctxWin int, sess *Session, childDepth int, recoveryTaskID, modelRef string, mutationObserver *checkpoint.MutationObserver) (string, error) {
	opts := t.subagentOptions(ctx, maxSteps, pricing, ctxWin, childDepth, recoveryTaskID, mutationObserver)
	opts.ModelRef = modelRef

	opts.ClassifierTaskText = prompt
	prompt = t.withWorkspaceContext(prompt) + "\n\n" + completeSubtaskContract

	ctx = WithUserImages(ctx, SubagentImageCandidates(ctx))
	return RunSubAgentWithSession(ctx, prov, subReg, sess, prompt, opts, sink)
}

func (t *TaskTool) runReadOnlySubSession(ctx context.Context, prompt string, subReg *tool.Registry, sink event.Sink, maxSteps int, prov provider.Provider, pricing *provider.Pricing, ctxWin int, sess *Session, childDepth int, recoveryTaskID, modelRef string, mutationObserver *checkpoint.MutationObserver) (string, error) {
	opts := t.subagentOptions(ctx, maxSteps, pricing, ctxWin, childDepth, recoveryTaskID, mutationObserver)
	opts.ModelRef = modelRef

	opts.ClassifierTaskText = prompt
	prompt = t.withWorkspaceContext(prompt)
	ctx = WithUserImages(ctx, SubagentImageCandidates(ctx))
	return RunReadOnlySubAgentWithSession(ctx, prov, subReg, sess, prompt, opts, sink)
}

// subagentOptions is the single construction point for the run options every
// sub-agent spawned through this tool shares (task, read_only_task, and
// parallel_tasks children). Compaction, language preferences, and depth limits
// must stay uniform across those paths — add new fields here, not at call sites.
func (t *TaskTool) subagentOptions(ctx context.Context, maxSteps int, pricing *provider.Pricing, ctxWin, childDepth int, recoveryTaskID string, mutationObserver *checkpoint.MutationObserver) Options {
	opts := Options{
		MaxSteps:          maxSteps,
		Temperature:       t.temperature,
		Pricing:           pricing,
		UsageSource:       event.UsageSourceSubagent,
		Gate:              t.gate,
		ContextWindow:     ctxWin,
		RecentKeep:        t.recentKeep,
		CompactRatio:      t.compactRatio,
		ArchiveDir:        t.archiveDir,
		KeepPolicy:        t.keepPolicy,
		ResponseLanguage:  ResponseLanguageFromContext(ctx),
		ReasoningLanguage: ReasoningLanguageFromContext(ctx),
		SubagentDepth:     childDepth,
		MaxSubagentDepth:  t.maxDepth(),
		DeliveryProfile:   t.deliveryProfile,
		Ablation:          t.ablation,
		WorkspaceLease:    t.workspaceLease,
		RecoveryGate:      t.recoveryGate,
		RecoveryAgentID:   "subagent",
		RecoveryTaskID:    recoveryTaskID,
		MutationObserver:  mutationObserver,
	}
	return opts
}

func subagentRecoveryTaskID(ctx context.Context, ref string) string {
	if ref = strings.TrimSpace(ref); ref != "" {
		return "subagent:" + ref
	}
	if callID, _, _, ok := CallContext(ctx); ok && strings.TrimSpace(callID) != "" {
		return "subagent:" + strings.TrimSpace(callID)
	}
	return "subagent"
}

// WithRecoveryGate shares Auto Guard with spawned sub-agents.
func (t *TaskTool) WithRecoveryGate(g RecoveryGate) *TaskTool {
	if t == nil {
		return nil
	}
	t.recoveryGate = g
	return t
}

// WithMutationObserver shares the host mutation observer with spawned sub-agents.
// Foreground children inherit the parent ownership turn; background children
// keep the turn that spawned them (set via OwnershipTurn at Begin).
func (t *TaskTool) WithMutationObserver(obs *checkpoint.MutationObserver) *TaskTool {
	if t == nil {
		return nil
	}
	t.mutationObserver = obs
	return t
}

func (t *TaskTool) withWorkspaceContext(prompt string) string {
	if t == nil {
		return prompt
	}
	ctx := subagentWorkspaceContext(t.workspaceRoot)
	if ctx == "" {
		return prompt
	}
	return ctx + "\n\n" + prompt
}

func subagentWorkspaceContext(root string) string {
	root = strings.TrimSpace(root)
	if root == "" {
		return ""
	}

	return `<workspace-context event="SubagentWorkspace">
Current workspace: ` + strconv.Quote(root) + `
File tools interpret relative paths against this workspace. For project inspection, prefer "." or relative paths unless the user explicitly named another absolute path.
</workspace-context>`
}

func FormatSubagentReference(run *SubagentRun) string {
	if run == nil || run.Ref == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Subagent reference: %s\n", run.Ref)
	if strings.TrimSpace(run.ForkedFrom) != "" {
		fmt.Fprintf(&b, "Forked from: %s\n", strings.TrimSpace(run.ForkedFrom))
		b.WriteString("The requested ref resolves to an ancestor conversation transcript, so the framework continues a copy owned by the current conversation. To continue this copied subagent transcript in a later call, pass ")
		b.WriteString(run.Ref)
		b.WriteString(" as `continue_from`. Start a fresh subagent when the next task is independent.")
		return b.String()
	}
	b.WriteString("To continue this same subagent transcript in a later call, pass this ref as `continue_from`. Start a fresh subagent when the next task is independent.")
	return b.String()
}

func FormatSubagentRunResult(answer string, run *SubagentRun, failed bool) string {
	answer = GuardSubagentHostDecisionText(answer)
	if run == nil || run.Ref == "" {
		return answer
	}
	if failed {
		if answer == "" {
			return "Subagent reference (failed): " + run.Ref
		}
		return "Subagent reference (failed): " + run.Ref + "\n\nFinal answer:\n" + answer
	}
	return FormatSubagentReference(run) + "\n\nFinal answer:\n" + answer
}

// GuardSubagentHostDecisionText appends a fixed boundary warning only when a
// child agent result appears to discuss host approval or user-owned decisions.
// The implementation lives in internal/tool so the skill tools share the exact
// same phrase list and notice.
func GuardSubagentHostDecisionText(answer string) string {
	return tool.GuardSubagentHostDecisionText(answer)
}

// maxReviewReportNudges bounds the in-session completion nudges sent to a
// review subagent that finished without submitting review_report. Each nudge is
// one cheap continuation request on the same (cached) subagent session — far
// cheaper than discarding the run and re-reviewing from scratch.
// maxReviewReportNudges is the single in-session retry after the first failed
// review run (plan: fail once, retry once). A second failure becomes Partial.
const maxReviewReportNudges = 1

// reviewReportTaskContract is appended to the task prompt of a review subagent
// whose run must end with a typed report. The skill body describes how to
// review; this states the non-negotiable submission protocol.
func reviewReportTaskContract(kind evidence.ReviewKind) string {
	return fmt.Sprintf(`<review-report-contract event="SubagentReviewReport">
Before your final answer you MUST call the review_report tool exactly once with kind=%q, your verdict (pass | warn | block), reviewed_paths listing only files you actually read this run, and your findings. The host discards a review run that ends without a successful review_report call — your prose summary alone does not count.
</review-report-contract>`, string(kind))
}

// reviewReportNudgePrompt asks an already-finished review subagent to submit
// the missing typed report without redoing the review.
func reviewReportNudgePrompt(kind evidence.ReviewKind) string {
	return fmt.Sprintf("You finished the review without calling the review_report tool, so the host cannot accept the run yet. Do not redo the review. Call review_report now with kind=%q, your verdict (pass | warn | block), reviewed_paths listing only the files you actually read in this conversation, and the findings you already reported. Then restate your final verdict in one sentence.", string(kind))
}
