package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/permission"
	"reasonix/internal/recovery"
	"reasonix/internal/tool"
)

// commitReasoningBeforeAnswer closes a real reasoning block and leaves exactly
// one blank transcript row before the assistant answer. Answers that start
// without reasoning keep their existing compact placement.
func (m *chatTUI) commitReasoningBeforeAnswer() {
	hadReasoning := m.reasoningNative || m.reasoningLineIdx >= 0
	m.commitReasoning()
	if hadReasoning {
		m.commitSpacer()
	}
}

// streamAnswer renders the answer streamed so far up to its last completed
// paragraph (flushableMarkdownPrefix) and writes it as one transcript block,
// rewritten in place as later paragraphs land — so a long reply appears chunk by
// chunk instead of all at once on turn end. The trailing, still-streaming block
// stays buffered (a half-written fence/list never renders early), and it only
// re-renders when a new paragraph actually closes.
func (m *chatTUI) streamAnswer() {
	if m.nativeScrollback {
		return
	}
	prefix := flushableMarkdownPrefix(m.pending.String())
	if len(prefix) <= m.answerFlushed {
		return
	}
	source := transcriptSource{kind: transcriptSourceMarkdown, raw: prefix}
	m.answerFlushed = len(prefix)
	if m.answerIdx < 0 {
		m.answerIdx = len(m.transcript)
		m.commitTranscriptSource(source)
	} else {

		block := m.renderTranscriptSource(source, m.width)
		m.setTranscriptBlock(m.answerIdx, block, source)
	}
}

// commitPending freezes the full accumulated answer as markdown — overwriting the
// streamed block if one is open (streamAnswer), else committing fresh. Joining
// commitReasoning then commitPending puts the answer on its own line, restoring
// the thinking→answer break the renderer strips.
func (m *chatTUI) commitPending() {
	if m.pending.Len() == 0 {
		m.answerIdx = -1
		m.answerFlushed = 0
		return
	}
	raw := m.pending.String()
	source := transcriptSource{kind: transcriptSourceMarkdown, raw: raw}
	if m.answerIdx < 0 {
		m.commitTranscriptSource(source)
	} else {
		block := m.renderTranscriptSource(source, m.width)
		m.setTranscriptBlock(m.answerIdx, block, source)
	}
	m.pending.Reset()
	m.answerIdx = -1
	m.answerFlushed = 0
}

// flushableMarkdownPrefix returns the longest prefix of buf made of complete
// markdown blocks — text up to the last blank line outside any open fenced code
// block. A blank line inside a ``` / ~~~ fence isn't a boundary, so a half-written
// code block stays buffered until it closes.
func flushableMarkdownPrefix(buf string) string {
	lines := strings.Split(buf, "\n")
	inFence := false
	boundary := -1
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~") {
			inFence = !inFence
			continue
		}
		if !inFence && t == "" {
			boundary = i
		}
	}
	if boundary <= 0 {
		return ""
	}
	return strings.Join(lines[:boundary], "\n")
}

// planApprovalTool is the Tool name the controller puts on the ApprovalRequest it
// emits to gate a plan (mirrors control's constant). The banner, status line, and
// approval handler key on it to render the plan-specific prompt and to keep the
// [plan] tag in sync when the user starts execution or exits without executing.
const planApprovalTool = "exit_plan_mode"

type approvalChoice struct {
	label           string
	allow           bool
	allowForSession bool
	persistToConfig bool
	exitPlan        bool
}

func approvalChoices(a *event.Approval) []approvalChoice {
	if a == nil {
		return nil
	}
	var decisions []approvalChoice
	fresh := a.Fresh || control.RequiresFreshHumanApprovalTool(a.Tool)
	switch {
	case isRecoveryApprovalEvent(a):
		if a.Recovery != nil && a.Recovery.CanGrantTask {

			decisions = []approvalChoice{{allow: true}, {allow: true, allowForSession: true}, {}}
		} else {
			decisions = []approvalChoice{{allow: true}, {}}
		}
	case a.Tool == planApprovalTool:
		decisions = []approvalChoice{{allow: true}, {}, {exitPlan: true}}
	case fresh && freshApprovalAllowsSession(a.Tool):
		decisions = []approvalChoice{{allow: true}, {allow: true, allowForSession: true}, {}}
	case fresh:
		decisions = []approvalChoice{{allow: true}, {}}
	default:
		decisions = []approvalChoice{
			{allow: true},
			{allow: true, allowForSession: true},
			{allow: true, allowForSession: true, persistToConfig: true},
			{},
		}
	}
	labels := approvalChoiceLabels(a)
	for i := range decisions {
		if i < len(labels) {
			decisions[i].label = labels[i]
		}
	}
	return decisions
}

func approvalChoiceLabels(a *event.Approval) []string {
	choices := i18n.M.FreshHumanApprovalChoices
	fresh := a.Fresh || control.RequiresFreshHumanApprovalTool(a.Tool)
	if isRecoveryApprovalEvent(a) {
		if isRecoveryPlanChangeApproval(a) {
			choices = i18n.M.RecoveryPlanChangeChoices
		} else {
			choices = i18n.M.RecoveryApprovalChoices
		}
		if !isRecoveryPlanChangeApproval(a) && a.Recovery != nil && a.Recovery.CanGrantTask {
			choices = i18n.M.RecoveryTaskGrantChoices
		}
	} else if a.Tool == planApprovalTool {
		choices = i18n.M.PlanApprovalChoices
	} else if !fresh {
		exactSessionRule := permission.SessionGrantRuleForScope(a.Tool, a.Subject)
		exactPersistentRule := permission.RememberRuleForScope(a.Tool, a.Subject)
		choices = fmt.Sprintf(i18n.M.ToolApprovalChoices, exactSessionRule, exactPersistentRule)
	}
	if a.Tool == control.SandboxEscapeApprovalTool {
		choices = i18n.M.SandboxEscapeApprovalChoices
	}
	if a.Tool == control.ManagedConfigWriteApprovalTool {
		choices = i18n.M.ConfigWriteApprovalChoices
	}
	if a.Tool == agent.PlanModeReadOnlyCommandApprovalTool {
		choices = i18n.M.PlanModeReadOnlyCommandChoices
	}
	if !fresh && a.Tool == "bash" && permission.BashCommandPrefix(a.Subject) != "" {
		prefixRule := permission.RememberRuleForScope(a.Tool, a.Subject)
		choices = fmt.Sprintf(i18n.M.BashPrefixChoices, prefixRule, prefixRule)
	}
	var labels []string
	for line := range strings.SplitSeq(choices, "\n") {
		line = strings.TrimSpace(line)
		if len(line) < 3 || line[0] < '1' || line[0] > '9' || line[1] != '.' {
			continue
		}
		labels = append(labels, strings.TrimSpace(line[2:]))
	}
	if isRecoveryApprovalEvent(a) && a.Recovery != nil && a.Recovery.CanGrantTask && len(labels) > 1 {
		if scope := strings.TrimSpace(a.Recovery.TaskGrantScope); scope != "" {
			labels[1] += " — " + scope
		}
	}
	return labels
}

// handleApprovalKey resolves a pending approval from a keystroke and re-arms the
// listener. 1/y/Enter allows once, 2/a allows for the rest of the session,
// 3/p writes an "always allow" rule to the config file for ordinary tool
// approvals. Fresh two-choice prompts use 2 for deny, while n/Esc and legacy 4
// still deny. Plan prompts use 1 to execute, 2/n/Esc to keep planning, and 3 to
// reject the pending plan and leave plan mode without executing it.
// Ctrl-C cancels the whole turn via the run context. For a plan approval
// (planApprovalTool), starting execution or explicitly exiting without execution
// drops the local [plan] tag and turns plan mode off on the controller.
func (m chatTUI) handleApprovalKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	choices := approvalChoices(m.pendingApproval)
	answer := func(choice approvalChoice) (tea.Model, tea.Cmd) {
		allow, session, persist := choice.allow, choice.allowForSession, choice.persistToConfig
		if isRecoveryApprovalEvent(m.pendingApproval) {
			action := agent.RecoveryActionRevise
			if allow {
				action = agent.RecoveryActionContinue
				if session {
					action = agent.RecoveryActionContinueTask
				}
			}
			_ = m.ctrl.ResolveRecovery(m.pendingApproval.ID, action, "")
			m.pendingApproval = nil
			return m, nil
		}
		if m.pendingApproval.Tool == planApprovalTool && (allow || choice.exitPlan) {
			m.planMode = false
			m.ctrl.SetPlanMode(false)
		}
		m.ctrl.Approve(m.pendingApproval.ID, allow, session, persist)
		m.pendingApproval = nil
		return m, nil
	}
	switch msg.String() {
	case "ctrl+c":
		m.ctrl.Cancel()
		return answer(approvalChoice{})
	case "up", "k", "ctrl+p":
		if m.approvalSelection < 0 && len(choices) > 0 {
			m.approvalSelection = 0
		} else if m.approvalSelection > 0 {
			m.approvalSelection--
		}
		return m, nil
	case "down", "j", "ctrl+n":
		if m.approvalSelection < len(choices)-1 {
			m.approvalSelection++
		}
		return m, nil
	case "enter":
		if m.approvalSelection >= 0 && m.approvalSelection < len(choices) {
			return answer(choices[m.approvalSelection])
		}
		return m, nil
	case "esc":
		return answer(approvalChoice{})
	}
	lower := strings.ToLower(msg.String())
	if len(lower) == 1 && lower[0] >= '1' && lower[0] <= '9' {
		idx := int(lower[0] - '1')
		if idx < len(choices) {
			return answer(choices[idx])
		}

		if lower == "4" {
			return answer(approvalChoice{})
		}
		return m, nil
	}
	switch lower {
	case "y":
		if len(choices) > 0 {
			return answer(choices[0])
		}
	case "a":
		for _, choice := range choices {
			if choice.allowForSession && !choice.persistToConfig {
				return answer(choice)
			}
		}
	case "p":
		for _, choice := range choices {
			if choice.persistToConfig {
				return answer(choice)
			}
		}
	case "n":
		return answer(approvalChoice{})
	}
	return m, nil
}

func isRecoveryApprovalEvent(a *event.Approval) bool {
	return a != nil && (a.Kind == recovery.ApprovalKindRecovery || a.Recovery != nil)
}

func isRecoveryPlanChangeApproval(a *event.Approval) bool {
	if !isRecoveryApprovalEvent(a) || a.Recovery == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(a.Recovery.ChangeKind)) {
	case string(recovery.ChangeStrategy), string(recovery.ChangeScope):
		return true
	default:
		return false
	}
}

func freshApprovalAllowsSession(toolName string) bool {
	return toolName == control.SandboxEscapeApprovalTool || toolName == control.ManagedConfigWriteApprovalTool
}

func (m chatTUI) cancelRequested() bool {
	if m.state != tuiRunning || m.ctrl == nil {
		return false
	}
	return m.ctrl.CancelRequested()
}

func (m chatTUI) runningWorkingLine(cancelRequested, styled bool) string {
	if m.state != tuiRunning {
		return ""
	}
	if m.retryAttempt > 0 && !cancelRequested {
		return fmt.Sprintf("  "+i18n.M.ChatStatusRetryingFmt, m.spinner.View(), m.retryAttempt, m.retryMax)
	}

	var working string
	if cancelRequested {
		working = fmt.Sprintf("  "+i18n.M.ChatStatusCancellingFmt, m.spinner.View(), m.elapsed)
	} else {
		phaseLabel := turnPhaseStatusLabel(m.turnPhase)
		if phaseLabel != "" {
			working = fmt.Sprintf("  %s %s · %ds", m.spinner.View(), phaseLabel, m.elapsed)
		} else {
			working = fmt.Sprintf("  "+i18n.M.ChatStatusThinkingFmt, m.spinner.View(), m.elapsed)
		}
	}
	if m.turnTokens > 0 {
		working += " · ↓" + shortTokens(m.turnTokens)
	}
	if n := m.inboxQueuedCount(); n > 0 {
		var queued string
		if n == 1 {
			queued = " · ✎ 1 in inbox"
		} else {
			queued = fmt.Sprintf(" · ✎ %d in inbox", n)
		}
		if m.inboxSnap().Paused {
			queued += " (paused)"
		}
		if styled {
			working += dim(queued)
		} else {
			working += queued
		}
	}
	return working
}

// Bottom region pinned under the transcript viewport: optional panels, the
// composer when visible, then the two status rows. Its height feeds
// transcriptHeight so the viewport above fills exactly the rest of the screen.

// clampCursorToTerminal keeps the reported caret inside [0,w) × [0,h).

// compactionCardLines renders a finished compaction as a titled card: a header
// with the message count and trigger, then the structured summary under a dim
// gutter so it reads as one block in scrollback. The summary is also the new
// context base, so this card is the user's window into exactly what was kept.

// contextTag renders the prompt-vs-context-window gauge for the status line,
// framed around the auto-compaction threshold: it shows how much headroom is
// left until the next compaction, and colours by proximity to that point rather
// than the raw window. Falls back to a plain percentage when compaction is disabled.

// cacheTag renders both prompt cache-hit rates for the status line —
// "turn hit 88.00% · avg 78.00%": the single-turn rate (latest turn, the higher/steeper
// number on a non-compacting DeepSeek session) and the session-aggregate rate
// Σhit/Σ(hit+miss) (the steadier, cost-oriented number that matches the legacy
// dashboard). "" before any cache tokens have been reported.

// jobsTag shows the count of running background jobs in the status line. Job
// start/finish emit Notices that arrive on eventCh and re-render the frame, so
// the count stays current without a dedicated tick.

// mouseTag is a persistent status-line marker while mouseCaptureOff is on, so
// the loss of in-app scrollbar/wheel-scroll/drag-select reads as a deliberate
// state rather than a bug the user has to guess at.

// shortTokens prints token counts compactly: 1_500 → "1.5K", 142_000 → "142.0K", 1_000_000 → "1.0M".

// turnPhaseStatusLabel maps host turn_phase values to a short status label.
// Empty when the phase is unknown so callers fall back to the default thinking line.

// formatCompletionSummaryLine renders a content-free quality summary for TUI scrollback.

// renderApprovalBanner is the slim notice shown above the input while a tool
// call (or a plan) awaits the user's decision.
func (m chatTUI) renderApprovalBanner() string {
	w := max(m.width, 10)
	if m.pendingApproval == nil {
		return ""
	}
	var text string
	var planDetails []string
	if m.pendingApproval.Tool == planApprovalTool {
		text = i18n.M.PlanApprovalPrompt
	} else if isRecoveryPlanChangeApproval(m.pendingApproval) {
		text = i18n.M.RecoveryPlanDecisionPrompt
		if rec := m.pendingApproval.Recovery; rec != nil {
			if before := compactApprovalPlan(rec.PlanBefore); before != "" {
				planDetails = append(planDetails, fmt.Sprintf(i18n.M.RecoveryPlanBeforeFmt, truncateSubject(before, w)))
			}
			if after := compactApprovalPlan(rec.PlanAfter); after != "" {
				planDetails = append(planDetails, fmt.Sprintf(i18n.M.RecoveryPlanAfterFmt, truncateSubject(after, w)))
			}
		}
	} else {
		name, detail := approvalToolDetails(m.pendingApproval.Tool)
		subj := strings.TrimSpace(m.pendingApproval.Subject)
		full := subj
		if subj != "" {
			subj = " " + truncateSubject(subj, w)
		}
		text = strings.TrimSpace(fmt.Sprintf(i18n.M.ToolApprovalPromptFmt, name, subj, detail, ""))

		if body := approvalSubjectBody(full, strings.TrimSpace(subj), w); body != "" {
			planDetails = append(planDetails, body)
		}
	}
	if reason := strings.TrimSpace(m.pendingApproval.Reason); reason != "" {
		text += " · " + truncateSubject(reason, w)
	}
	if len(planDetails) > 0 {
		text += "\n" + strings.Join(planDetails, "\n")
	}
	var b strings.Builder
	b.WriteString("⏸ " + text + "\n")
	for i, choice := range approvalChoices(m.pendingApproval) {
		b.WriteString(rowLine(i == m.approvalSelection, i+1, "", choice.label, false) + "\n")
	}
	b.WriteString(dim("↑/↓ navigate · Enter select · y/a/p/n shortcuts"))
	return choicePanelStyle.Width(w).Render(b.String())
}

// maxApprovalSubjectLines bounds the expanded command so a heredoc cannot push
// the composer off screen.
const maxApprovalSubjectLines = 8

// approvalSubjectBody returns the full command wrapped over several lines when
// the banner's one-line preview had to clip it, or "" when the preview already
// showed everything.
func approvalSubjectBody(full, preview string, width int) string {
	full = strings.TrimSpace(full)
	if full == "" || full == preview {
		return ""
	}
	wrapWidth := max(width-4, 20)
	lines := strings.Split(wrapStatusLine(full, wrapWidth), "\n")
	if len(lines) > maxApprovalSubjectLines {
		lines = lines[:maxApprovalSubjectLines]
		lines[maxApprovalSubjectLines-1] = ansi.Truncate(lines[maxApprovalSubjectLines-1], wrapWidth-1, "") + "…"
	}
	return strings.Join(lines, "\n")
}

func compactApprovalPlan(plan string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(strings.TrimSpace(plan), "\n", " · ")), " ")
}

// approvalToolDetails turns provider-visible tool IDs into user-facing labels.
// MCP tools are advertised as mcp__<server>__<tool>; showing the short tool name
// first keeps the approval prompt readable while preserving the source.
func approvalToolDetails(toolName string) (name, detail string) {
	if toolName == agent.PlanModeReadOnlyCommandApprovalTool {
		return i18n.M.ApprovalToolLabelPlanModeReadOnly, fmt.Sprintf(i18n.M.ToolApprovalSourceFmt, i18n.M.ToolApprovalBuiltIn)
	}
	if toolName == control.SandboxEscapeApprovalTool {
		return i18n.M.ApprovalToolLabelSandboxEscape, fmt.Sprintf(i18n.M.ToolApprovalSourceFmt, i18n.M.ToolApprovalBuiltIn)
	}
	if toolName == control.ManagedConfigWriteApprovalTool {
		return i18n.M.ApprovalToolLabelConfigWrite, fmt.Sprintf(i18n.M.ToolApprovalSourceFmt, i18n.M.ToolApprovalBuiltIn)
	}
	if server, short, ok := tool.SplitMCPName(toolName); ok {
		lines := []string{}
		if strings.EqualFold(short, "understand_image") {
			lines = append(lines, i18n.M.ToolApprovalImageUse)
		}
		lines = append(lines, fmt.Sprintf(i18n.M.ToolApprovalSourceFmt, server))
		return short, strings.Join(lines, "\n")
	}
	return approvalToolLabel(toolName), fmt.Sprintf(i18n.M.ToolApprovalSourceFmt, i18n.M.ToolApprovalBuiltIn)
}

func approvalToolLabel(toolName string) string {
	switch toolName {
	case "bash":
		return i18n.M.ApprovalToolLabelBash
	case "edit_file":
		return i18n.M.ApprovalToolLabelEditFile
	case "write_file":
		return i18n.M.ApprovalToolLabelWriteFile
	case "multi_edit":
		return i18n.M.ApprovalToolLabelMultiEdit
	case "move_file":
		return i18n.M.ApprovalToolLabelMoveFile
	case "web_fetch":
		return i18n.M.ApprovalToolLabelWebFetch
	case "run_skill":
		return i18n.M.ApprovalToolLabelRunSkill
	case "remember":
		return i18n.M.ApprovalToolLabelRemember
	case "forget":
		return i18n.M.ApprovalToolLabelForget
	default:
		return toolName
	}
}
