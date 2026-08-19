package control

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/capability"
	"reasonix/internal/event"
	"reasonix/internal/permission"
	"reasonix/internal/provider"
	"reasonix/internal/sandbox"
	"reasonix/internal/tool"
)

// Approve answers a pending ApprovalRequest by ID: allow runs the call, session
// also remembers a grant for the rest of the session so the same approval scope
// is not re-prompted. Unknown/expired IDs are ignored.
func (c *Controller) Approve(id string, allow, session, persist bool) {

	c.mu.Lock()
	gate := c.recoveryGate
	c.mu.Unlock()
	if gate != nil && gate.HasApproval(id) {
		action := agent.RecoveryActionRevise
		if allow {
			action = agent.RecoveryActionContinue
		}
		_ = c.ResolveRecovery(id, action, "")
		return
	}
	pending := c.approval.resolve(id)
	if pending.reply == nil {
		return
	}
	outcome := "deny"
	if pending.tool == planApprovalTool {
		outcome = string(PlanDecisionRevisePlan)
		if allow {
			outcome = string(PlanDecisionStartExecution)
		}
	} else if allow {
		switch {
		case persist:
			outcome = "allow_persistent"
		case session:
			outcome = "allow_session"
		default:
			outcome = "allow_once"
		}
	}
	c.recordDecisionReceipt(pending, outcome)
	pending.reply <- approvalReply{allow: allow, session: session, persist: persist}
}

// ResolvePlanDecision answers the Plan card without collapsing revise and exit
// into the generic approval boolean used by older clients.
func (c *Controller) ResolvePlanDecision(id string, action PlanDecisionAction) error {
	if c == nil {
		return fmt.Errorf("controller is nil")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("empty plan approval id")
	}
	switch action {
	case PlanDecisionStartExecution, PlanDecisionRevisePlan, PlanDecisionExitPlan:
	default:
		return fmt.Errorf("unknown plan decision %q", action)
	}
	pending, ok := c.approval.resolveTool(id, planApprovalTool)
	if !ok || pending.reply == nil {
		return fmt.Errorf("plan approval %q is no longer pending", id)
	}
	pending.kind = "plan"
	c.recordDecisionReceipt(pending, string(action))
	pending.reply <- approvalReply{allow: action == PlanDecisionStartExecution}
	return nil
}

func (c *Controller) recordDecisionReceipt(pending pendingApproval, outcome string) {
	if c == nil || c.executor == nil || pending.reply == nil {
		return
	}
	kind := pending.kind
	if kind == "" {
		kind = "tool"
		if pending.tool == planApprovalTool {
			kind = "plan"
		}
	}
	receipt := &provider.DecisionReceipt{
		ID:      pending.id,
		Kind:    kind,
		Tool:    strings.TrimSpace(pending.tool),
		Subject: clipUTF8(strings.TrimSpace(pending.subject), 240),
		Outcome: strings.TrimSpace(outcome),
	}

	c.executor.Session().AddDecisionReceipt(receipt)
	c.sink.Emit(event.Event{
		Kind:            event.Notice,
		Code:            event.NoticeCodeDecisionReceipt,
		Level:           event.LevelInfo,
		Text:            "Decision recorded: " + receipt.Outcome,
		DecisionReceipt: receipt,
	})
}

// EnableInteractiveApproval swaps the executor's gate for one that routes
// approval decisions to the frontend via ApprovalRequest events, and wires the
// controller in as the executor's Asker so the `ask` tool can question the user.
// Interactive frontends (chat, desktop) call this; the headless run keeps the
// silent gate and a nil asker from setup.
func (c *Controller) EnableInteractiveApproval() {
	trustGate := planModeReadOnlyTrustApprover{c}
	escapeApprover := sandboxEscapeApprover{c}
	configApprover := managedConfigWriteApprover{c}
	if c.executor != nil {
		c.executor.SetGate(c.newInteractiveGate())
		c.executor.SetPlanModeReadOnlyTrustGate(trustGate)
		c.executor.SetSandboxEscapeApprover(escapeApprover)
		c.executor.SetConfigWriteApprover(configApprover)
		c.executor.SetAsker(c)
	}
	if setter, ok := c.runner.(interface {
		SetPlanModeReadOnlyTrustGate(agent.PlanModeReadOnlyTrustGate)
	}); ok {
		setter.SetPlanModeReadOnlyTrustGate(trustGate)
	}
	if setter, ok := c.runner.(interface {
		SetSandboxEscapeApprover(sandbox.EscapeApprover)
	}); ok {
		setter.SetSandboxEscapeApprover(escapeApprover)
	}
	if setter, ok := c.runner.(interface {
		SetConfigWriteApprover(tool.ConfigWriteApprover)
	}); ok {
		setter.SetConfigWriteApprover(configApprover)
	}
	if setter, ok := c.runner.(interface {
		SetPlannerPlanApprover(agent.PlannerPlanApprover)
	}); ok {
		setter.SetPlannerPlanApprover(plannerPlanApprover{c: c})
	}

	if setter, ok := c.runner.(interface{ SetAsker(agent.Asker) }); ok {
		setter.SetAsker(c)
	}
}

type plannerPlanApprover struct {
	c *Controller
}

func (p plannerPlanApprover) RunWithPlannerApproval(ctx context.Context, plan string, run func(context.Context) error) error {
	c := p.c
	allow, _, err := c.requestApprovalWithReason(ctx, planApprovalTool, "", nil, "Planner requested host approval before execution.")
	if err != nil {
		return err
	}
	if !allow {
		return nil
	}
	todoArgs := c.seedPlanTodos(plan)
	execStart := c.sessionMessageCount()
	c.approval.setPlanAutoApprove(true)
	defer c.approval.setPlanAutoApprove(false)
	if err := run(ctx); err != nil {
		return err
	}
	if todoArgs != "" && !c.hasTodoUpdateSince(execStart) {
		c.completePlanTodos(todoArgs)
	}
	return nil
}

func (c *Controller) newInteractiveGate() *permission.Gate {
	policy := c.policy
	mode := c.approval.mode()
	switch mode {
	case ToolApprovalAuto, ToolApprovalYolo:
		policy.Mode = permission.Allow
	case ToolApprovalDontAsk:
		policy.Mode = permission.Deny
	default:
		policy.Mode = permission.Ask
	}

	policy.SessionAllow = rulesWithoutFreshHumanApproval(policy.SessionAllow)
	var approver permission.Approver = gateApprover{c}
	if mode == ToolApprovalDontAsk {
		approver = denyPermissionApprover{}
	}
	gate := permission.NewGate(policy, approver)
	gate.OnRemember = func(rule string) {
		if c.onRemember != nil {
			_ = c.onRemember(rule)
		}
	}
	return gate
}

func (c *Controller) newHeadlessGate(mode string) *freshHumanHeadlessGate {
	gate := BuildHeadlessApprovalGate(c.policy, mode)
	return gate
}

type denyPermissionApprover struct{}

func (denyPermissionApprover) Approve(context.Context, string, string, json.RawMessage) (bool, bool, error) {
	return false, false, nil
}

// rulesWithoutFreshHumanApproval drops any session-allow rule that targets a
// tool requiring fresh human approval, so an explicit allowlist cannot bypass
// the always-prompt contract for those tools.
func rulesWithoutFreshHumanApproval(rules []permission.Rule) []permission.Rule {
	if len(rules) == 0 {
		return rules
	}
	filtered := make([]permission.Rule, 0, len(rules))
	for _, r := range rules {
		if RequiresFreshHumanApprovalTool(r.Tool) {
			continue
		}
		filtered = append(filtered, r)
	}
	return filtered
}

// ApplyHeadlessApprovalMode configures the executor gate for a non-interactive
// (`reasonix run`) session from an explicit --permission-mode. Unlike
// EnableInteractiveApproval it installs no blocking approver, asker, or
// fresh-approval prompt: there is no key loop to answer them, and the default
// infinite approval timeout would wedge the run forever on an Ask rule, the
// `ask` tool, or a sandbox/config approval. Modes map straight onto a headless
// gate, and each preserves the interactive contract as closely as a run with no
// one to prompt allows:
//
//   - auto: auto-approve the writer fallback (Mode=Allow) but PRESERVE explicit
//     ask rules. Interactive auto prompts on those (it never auto-approves them);
//     headless can't prompt, so a would-ask decision fails closed (deny) rather
//     than running silently. Only bypass may run such a command unattended.
//   - yolo/bypassPermissions: skip ordinary approval-gated decisions (nil
//     approver); deny rules and fresh decisions still fail closed.
//   - dontAsk: deny anything that would ask, and deny the writer fallback too.
//
// Deny rules and fresh-human tools (memory, plan, sandbox, config) stay enforced
// by the gate for every mode. The only exception is a controller-assessed,
// create-only project/reference memory; every other memory write remains denied.
func (c *Controller) ApplyHeadlessApprovalMode(mode string) {
	mode = normalizeToolApprovalMode(mode)
	c.approval.setMode(mode)
	if c.subagentGate != nil {
		c.subagentGate.Update(mode)
	}
	if c.executor != nil {
		c.executor.SetGate(c.newHeadlessGate(mode))
	}
}

func (c *Controller) refreshInteractiveGate() {
	if c.executor != nil {
		c.executor.SetGate(c.newInteractiveGate())
	}
}

// TrySteer queues mid-turn guidance only when the active agent turn accepts it.
func (c *Controller) TrySteer(text string) bool {
	c.mu.Lock()
	exec := c.executor
	running := c.running
	c.mu.Unlock()
	return running && exec != nil && exec.Steer(text)
}

// Steer is the compatibility path for callers that cannot observe admission.
// Interactive hosts should call TrySteer so a rejected steer remains in their
// draft/queue and can be retried as a regular follow-up.
func (c *Controller) Steer(text string) {
	if c.TrySteer(text) {
		return
	}

	c.submitSteerFallback(text)
}

// submitSteerFallback records steer text that no active turn accepted as
// unapplied guidance, not as a new task. This compatibility path deliberately
// never opens a provider turn: replaying stale historical guidance as the
// user's current request caused unintended code changes (#7045).
func (c *Controller) submitSteerFallback(text string) admissionResult {
	return c.runGuardedOrPark(func(context.Context) error {
		if c.executor != nil {
			c.executor.RecordUnappliedSteer(text)
		}
		return nil
	})
}

// SteerConsumed returns true when the steer queue is empty after the last consume.
func (c *Controller) SteerConsumed() bool {
	c.mu.Lock()
	exec := c.executor
	c.mu.Unlock()
	if exec != nil {
		return exec.SteerConsumed()
	}
	return true
}

// promptQueueNoticeDelay is how long a prompt may wait behind another before
// the user is told why nothing has appeared. Short enough to beat "it's stuck",
// long enough that an approval answered promptly never emits a notice.
var promptQueueNoticeDelay = 3 * time.Second

// lockPromptFor acquires the prompt lock, emitting one notice if the wait is
// long enough to look like a hang. It reports false only when ctx ended first;
// the lock is held on true.
func (c *Controller) lockPromptFor(ctx context.Context, kind string) bool {
	acquired := make(chan struct{})
	go func() {
		c.approval.promptMu.Lock()
		close(acquired)
	}()
	select {
	case <-acquired:
		return true
	case <-ctx.Done():
	case <-time.After(promptQueueNoticeDelay):
	}
	if ctx.Err() == nil {
		c.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelInfo, Code: event.NoticeCodePromptQueued,
			Text:   "A " + kind + " is waiting for you to answer the prompt ahead of it.",
			Detail: "the assistant asked something while an earlier approval or question was still open; it appears once that one is answered"})
	}
	select {
	case <-acquired:
		return true
	case <-ctx.Done():

		go func() {
			<-acquired
			c.approval.promptMu.Unlock()
		}()
		return false
	}
}

// Ask implements agent.Asker: it emits an AskRequest and blocks until
// AnswerQuestion(ID, …) answers or ctx is cancelled. promptMu serialises it
// against tool-approval prompts so at most one user prompt is outstanding.
// Unlike tool-approval gates, Ask is NOT bypassed in YOLO mode — the `ask`
// tool exists to get a genuine user decision, and YOLO only auto-approves
// tool calls; it must not answer the user's questions for them.
func (c *Controller) Ask(ctx context.Context, questions []event.AskQuestion) ([]event.AskAnswer, error) {

	id, reply := c.approval.registerAsk(questions)

	if !c.lockPromptFor(ctx, "question") {
		c.approval.cancelAsk(id)
		return nil, ctx.Err()
	}
	defer c.approval.promptMu.Unlock()

	c.approval.promptEmitMu.Lock()
	c.approval.markAskEmitted(id)
	c.sink.Emit(event.Event{Kind: event.AskRequest, Ask: event.Ask{ID: id, Questions: questions}})
	c.approval.promptEmitMu.Unlock()

	waitCtx, cancelWait := c.approval.waitContext(ctx)
	defer cancelWait()

	select {
	case ans := <-reply:
		return ans, nil
	case <-waitCtx.Done():
		c.approval.cancelAsk(id)
		return nil, waitCtx.Err()
	}
}

// AnswerQuestion resolves a pending AskRequest by ID with the user's selections.
// Unknown/expired IDs are ignored.
func (c *Controller) AnswerQuestion(id string, answers []event.AskAnswer) {
	if pending, ok := c.approval.resolveAsk(id); ok {

		if !askAnswersHaveSelection(answers) {
			c.mu.Lock()
			activeTurn := c.cancel != nil
			c.mu.Unlock()
			if activeTurn {
				c.Cancel()
				return
			}
		}
		c.recordAskDecisionReceipt(id, pending, answers)
		pending.reply <- answers
	}
}

func (c *Controller) recordAskDecisionReceipt(id string, pending pendingAsk, answers []event.AskAnswer) {
	if c == nil || c.executor == nil {
		return
	}
	selected := make(map[string][]string, len(answers))
	for _, answer := range answers {
		selected[answer.QuestionID] = append([]string(nil), answer.Selected...)
	}
	parts := make([]string, 0, len(pending.questions))
	for _, question := range pending.questions {
		answer := strings.TrimSpace(strings.Join(selected[question.ID], ", "))
		if answer == "" {
			answer = "—"
		}
		prompt := strings.TrimSpace(question.Prompt)
		if prompt == "" {
			prompt = strings.TrimSpace(question.Header)
		}
		if prompt == "" {
			prompt = question.ID
		}
		parts = append(parts, prompt+": "+answer)
	}
	receipt := &provider.DecisionReceipt{
		ID:      id,
		Kind:    "ask",
		Subject: clipUTF8(strings.Join(parts, " · "), 240),
		Outcome: "answered",
	}
	c.executor.Session().AddDecisionReceipt(receipt)
	c.sink.Emit(event.Event{
		Kind:            event.Notice,
		Code:            event.NoticeCodeDecisionReceipt,
		Level:           event.LevelInfo,
		Text:            "Decision recorded: answered",
		DecisionReceipt: receipt,
	})
}

func askAnswersHaveSelection(answers []event.AskAnswer) bool {
	for _, answer := range answers {
		if len(answer.Selected) > 0 {
			return true
		}
	}
	return false
}

// ReplayPendingPrompts re-emits the ApprovalRequest / AskRequest event for every
// prompt currently blocking the run loop. A frontend that reconnected or reloaded
// after the original event has no way to rebuild its approval/ask modal otherwise,
// so the blocked gate goroutine stays stuck forever while the session shows a
// "waiting" status with no actionable prompt. promptMu serialises Ask and
// requestApproval, so in practice at most one prompt is outstanding; the loops
// stay general so a future concurrent prompt would still replay correctly.
func (c *Controller) ReplayPendingPrompts() {
	c.approval.promptEmitMu.Lock()
	noApprovals := c.replayPendingPromptsTo(c.sink)
	c.approval.promptEmitMu.Unlock()
	if noApprovals {

		c.ReplayUnresolvedRecoveries()
	}
}

// ReplayPendingPromptsTo re-emits pending prompts to one frontend sink. Serve
// uses this for a newly attached SSE client so existing browsers do not receive
// duplicate approval/ask cards when another client reconnects.
func (c *Controller) ReplayPendingPromptsTo(sink event.Sink) {
	c.approval.promptEmitMu.Lock()
	defer c.approval.promptEmitMu.Unlock()
	c.replayPendingPromptsTo(sink)
}

// ReplayPendingPromptsWith performs an SSE connection handoff while prompt
// registration and emission are paused. The factory must subscribe the new
// client and return a sink that targets it; this closes the attach race where
// the original prompt could otherwise land between Subscribe and replay.
func (c *Controller) ReplayPendingPromptsWith(sinkFactory func() event.Sink) {
	if sinkFactory == nil {
		return
	}
	c.approval.promptEmitMu.Lock()
	defer c.approval.promptEmitMu.Unlock()
	c.replayPendingPromptsTo(sinkFactory())
}

func (c *Controller) replayPendingPromptsTo(sink event.Sink) bool {
	approvals, asks := c.approval.snapshotPrompts()
	c.emitPendingPrompts(sink, approvals, asks)
	return len(approvals) == 0
}

func (c *Controller) emitPendingPrompts(sink event.Sink, approvals []event.Approval, asks []event.Ask) {
	if sink == nil {
		return
	}
	for _, a := range approvals {
		sink.Emit(c.approvalRequestEvent(a))
	}
	for _, a := range asks {
		sink.Emit(event.Event{Kind: event.AskRequest, Ask: a})
	}
}

// SetPlanMode flips the executor's plan-first workflow flag without touching the
// cache-stable system/tool prefix, and remembers the state so Compose can prepend
// the plan-mode marker to outgoing user turns.
func (c *Controller) SetPlanMode(v bool) {
	c.applyPlanMode(v)
}

// SetAgentPreset updates the session role setting for subsequent turns without
// rebuilding the controller, provider, or tool schemas. Callers must already
// hold active-work guards (no foreground turn, background jobs, or pending
// approvals/asks).
func (c *Controller) SetAgentPreset(preset string) {
	if c == nil {
		return
	}
	preset = strings.TrimSpace(preset)
	if preset == "" {
		preset = "balanced"
	}

	if normalized := strings.ToLower(preset); normalized == "economy" || normalized == "full" {
		switch normalized {
		case "economy":
			preset = "light"
		case "full":
			preset = "balanced"
		}
	}
	if setter, ok := c.runner.(interface{ SetAgentPreset(string) }); ok {
		setter.SetAgentPreset(preset)
	}
	if c.executor != nil {
		c.executor.SetAgentPreset(preset)
	}

	c.mu.Lock()
	switch strings.ToLower(preset) {
	case "light", "economy":
		c.runtimeProfile = capability.ProfileEconomy
	case "delivery":
		c.runtimeProfile = capability.ProfileDelivery
	default:
		c.runtimeProfile = capability.ProfileBalanced
	}
	c.mu.Unlock()
}

// AgentPreset returns the current session role setting.
func (c *Controller) AgentPreset() string {
	if c == nil {
		return "balanced"
	}
	if c.executor != nil {
		return c.executor.AgentPreset()
	}
	if getter, ok := c.runner.(interface{ AgentPreset() string }); ok {
		return getter.AgentPreset()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch c.runtimeProfile {
	case capability.ProfileEconomy:
		return "light"
	case capability.ProfileDelivery:
		return "delivery"
	default:
		return "balanced"
	}
}

func (g gateApprover) Approve(ctx context.Context, tool, subject string, args json.RawMessage) (bool, bool, error) {
	allow, remember, _, err := g.ApproveWithReason(ctx, tool, subject, args)
	return allow, remember, err
}
