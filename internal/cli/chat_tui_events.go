package cli

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/event"
	"reasonix/internal/i18n"
)

// ingestEvent routes one typed event from the agent. Reasoning (dim) and answer
// free-text accumulate in their live buffers; every other event first finalizes
// the reasoning and answer streamed so far, then commits its own line —
// preserving order. Switching on the event Kind replaces the old prefix-sniffing
// of a flattened byte stream: the structure is now explicit.
func (m *chatTUI) ingestEvent(e event.Event) {
	if e.Kind == event.Retrying {
		m.retryAttempt = e.RetryAttempt
		m.retryMax = e.RetryMax
		return
	}
	if e.Kind == event.StreamAttempt {

		if e.StreamAttempt.Action == event.StreamAttemptDiscard {
			m.toolPartial = ""
			m.toolTail = nil
			m.toolStreamIdx = -1
			m.toolLineCount = 0
			m.commitLine(dim("  ↻ stream interrupted — reconnecting…"))
		}
		return
	}

	m.retryAttempt = 0
	m.retryMax = 0
	if m.turnDiscarded {

		if e.Kind == event.TurnDone {
			m.turnDiscarded = false
			m.state = tuiIdle
			m.noteWatchdogIdle()
		}
		return
	}

	if e.Kind != event.TurnStarted && e.Kind != event.TurnDone {
		m.confirmBubbleSent()
	}
	switch e.Kind {
	case event.Reasoning:
		if m.nativeScrollback {
			if !m.reasoningNative {
				m.thinkStart = time.Now()
				m.reasoningNative = true
			}
			m.streamReasoning(e.Text)
			break
		}
		if m.reasoningLineIdx < 0 {

			m.commitSpacer()
			m.thinkStart = time.Now()
			m.reasoningLineIdx = len(m.transcript)
			m.commitLine(dim("  ▎ " + i18n.M.ChatThinking))
			m.reasoningTextIdx = len(m.transcript)
			m.commitLine("")
			m.reasoningView = m.reasoningView[:0]
		}
		m.streamReasoning(e.Text)

	case event.Text:
		m.commitReasoningBeforeAnswer()
		m.pending.WriteString(e.Text)
		m.streamAnswer()

	case event.Message:

		if e.Text != "" && m.pending.Len() > 0 {
			m.pending.Reset()
			m.pending.WriteString(e.Text)
		}
		m.commitReasoning()
		m.commitPending()

	case event.ToolDispatch:

		if e.Tool.Partial || e.Tool.Refreshed {
			break
		}
		m.finalizeStreamed()
		switch e.Tool.Name {
		case "todo_write":

		case planApprovalTool:

		default:
			m.commitSpacer()
			if block := diffBlock(e.Tool.Name, e.Tool.Args, e.Tool.FileDiff, m.width, m.diffMaxLines); block != nil {
				for _, ln := range block {
					m.commitLine(ln)
				}
				break
			}
			m.commitTranscriptSource(transcriptSource{
				kind: transcriptSourceToolCard, raw: e.Tool.Name, aux: e.Tool.Args,
			})
			m.beginToolRunning(e.Tool.ID)
		}

	case event.ToolProgress:
		if event.IsSubagentProgressName(e.Tool.Name) {
			m.streamSubagentProgress(e.Tool)
			break
		}

		if event.IsReservedSubagentProgressName(e.Tool.Name) {
			break
		}
		m.streamToolOutput(e.Tool.ID, e.Tool.Output)

	case event.ToolResult:

		m.collapseFinalToolOutput(e.Tool)
		if e.Tool.Name == "todo_write" && e.Tool.Err == "" {
			m.todoArgs = e.Tool.Args
		}
		if e.Tool.Err != "" {
			m.finalizeStreamed()
			label := shellToolDisplayName(e.Tool.Name, e.Tool.Execution)
			detail := shellFailureDetail(e.Tool.Execution)
			errText := e.Tool.Err
			if detail != "" {
				errText = detail + " · " + errText
			}
			m.commitLine("  " + red("●") + " " + bold(label) + " " + red("⊘ "+errText))
		}

	case event.Usage:
		if e.Usage != nil {
			m.turnTokens += e.Usage.CompletionTokens
		}
		if m.showTurnUsage {
			if line := renderTurnReceipt(e.Usage, e.Pricing, e.CacheDiagnostics); line != "" {
				m.finalizeStreamed()
				m.commitSpacer()
				m.commitTranscriptSource(transcriptSource{kind: transcriptSourceTurnReceipt, raw: line})
			}
		}

	case event.TurnPhase:

		if phase := strings.TrimSpace(string(e.PhaseName)); phase != "" {
			m.turnPhase = phase
		} else if phase := strings.TrimSpace(e.Text); phase != "" {
			m.turnPhase = phase
		}

	case event.CompletionSummary:
		if e.Completion != nil {
			if completionSummaryNeedsAttention(e.Completion) {
				m.finalizeStreamed()
				m.commitLine(fmt.Sprintf("  ! %s", completionSummaryWarning(e.Completion)))
			}
			if m.showReasoning {
				m.finalizeStreamed()
				m.commitLine(dim("  · " + formatCompletionSummaryLine(e.Completion)))
			}
		}

	case event.Notice:
		glyph := "·"
		if e.Level == event.LevelWarn {
			glyph = "!"
		}
		m.finalizeStreamed()
		m.commitLine(fmt.Sprintf("  %s %s", glyph, e.Text))

	case event.GuardianAssessment:
		m.finalizeStreamed()
		g := e.Guardian
		line := fmt.Sprintf("Guardian %s · %s", g.Outcome, g.Tool)
		if g.Subject != "" {
			line += " · " + truncateSubject(g.Subject, m.width)
		}
		if g.RiskLevel != "" {
			line += " · risk=" + g.RiskLevel
		}
		if g.UserAuthorization != "" {
			line += " · authorization=" + g.UserAuthorization
		}
		if g.Rationale != "" {
			line += " · " + g.Rationale
		}
		if g.Outcome == "deny" {
			m.commitLine("  ! " + line)
		} else {
			m.commitLine("  · " + line)
		}

	case event.ExtensionStatus:

		if line := extensionStatusLine(e.Extension); line != "" {
			m.finalizeStreamed()
			m.commitLine(line)
		}

	case event.ExtensionSurface:

		m.finalizeStreamed()
		if e.Extension != nil && e.Extension.Notification != nil {
			if line := extensionNotificationLine(e.Extension); line != "" {
				m.commitLine(line)
			}
			break
		}
		for _, ln := range extensionSurfaceLines(e.Extension, m.width) {
			m.commitLine(ln)
		}

	case event.CompactionStarted:
		m.finalizeStreamed()
		m.commitLine(dim("  ⋯ " + i18n.M.CompactionWorking))

	case event.CompactionDone:

		if e.Compaction.Summary == "" {
			break
		}
		m.finalizeStreamed()
		for _, ln := range compactionCardLines(e.Compaction) {
			m.commitLine(ln)
		}

	case event.Phase:
		m.finalizeStreamed()
		m.commitLine(fmt.Sprintf("[%s]", e.Text))

	case event.ApprovalRequest:

		a := e.Approval
		m.pendingApproval = &a
		m.approvalSelection = 0
		if isRecoveryPlanChangeApproval(&a) {

			m.approvalSelection = -1
		}

	case event.AskRequest:

		m.startAskChooser(e.Ask)

	case event.MCPSurfaceReady:

		m.refreshHostAndInvalidateSlashCatalog()
		m.refreshMCPManager()

	case event.TurnDone:

		m.commitReasoning()
		m.commitPending()

		m.confirmBubbleSent()
		m.state = tuiIdle
		m.turnPhase = ""
		m.noteWatchdogIdle()
		m.queueEditCursor, m.queueEditDraft = -1, ""
		m.clearSubmittedPastes()
		if e.Outcome == event.TurnOutcomeRecoveryPaused {
			m.commitLine(wrapForViewport("⏸ "+i18n.M.RecoveryPaused, m.width, activeCLITheme.info))
		} else if e.Err != nil && e.Err.Error() != "" && !strings.Contains(e.Err.Error(), "context canceled") {
			m.commitLine(wrapForViewport(i18n.M.ErrorPrefix+" "+e.Err.Error(), m.width, activeCLITheme.warn))
		}
		m.commitReceipt(e.Receipt)

		m.wantMouseReenable = true

	}
}

// finalizeStreamed freezes any in-progress reasoning + answer into scrollback so
// a following event line lands after them, preserving chronological order.
func (m *chatTUI) finalizeStreamed() {
	m.collapseToolOutput(m.toolStreamID, "")
	m.commitReasoning()
	m.commitPending()
}

// startAskChooser opens the ask question card, entering free-text entry right
// away when the first question is an input field.
func (m *chatTUI) startAskChooser(a event.Ask) {
	m.finalizeStreamed()
	m.chooser = newChooser(a)
	if m.chooser.isInputTab() {
		m.enterChooserInput()
	}
}

func waitForAgentEvent(ch chan event.Event) tea.Cmd {
	return func() tea.Msg { return agentEventMsg(<-ch) }
}

func elapsedTick() tea.Cmd {
	return tea.Tick(time.Second, func(_ time.Time) tea.Msg { return elapsedTickMsg{} })
}

// runSlashCommand handles "/<cmd> <args>" input. Local commands queue their
// output to scrollback; MCP prompt / custom commands resolve to a model turn.

// showStatusDetails keeps diagnostics available without permanently crowding
// the two-line composer footer.

// activeConfigTag names the config file actually in effect. A ./reasonix.toml
// outranks the user-global file, so a session started in a directory holding
// one silently ignores global edits unless the source is visible (#3317).

// runCopyCommand copies the Nth-latest assistant message from the current turn
// (after the last user message) to the clipboard.
//
//   - "/copy"   — shows a numbered list of assistant messages to choose from.
//   - "/copy N" — copies the Nth message directly (1 = most recent).
//
// Counting does not cross user message boundaries.

// firstLine returns the first non-empty line of s, truncated to 80 runes.

// copyAssistantParts returns the Content of assistant messages after the last
// user message in msgs, skipping empty strings and model placeholders ("…", "...").
// The result is chronological (oldest first).

// runExportCommand exports the entire session as a markdown file, excluding
// system messages, reasoning/thinking content, and tool calls/results.

// commandNames renders the custom command list for /help, "" when there are none.

// showSandboxStatus displays the current sandbox configuration and whether
// the OS sandbox backend is available. It reads from the stored config so
// the user can inspect sandbox state without leaving the TUI (closes #3316).

// runMCPSubcommand handles "/mcp" (status), "/mcp add …" (connect a server live
// and persist it), and "/mcp remove <name>" (disconnect + drop from config). Add
// connects synchronously — like /compact, an explicit command may briefly block
// the UI while the handshake runs.

// showMCPStatus queues the connected MCP servers, their counts, and the prompt
// commands / resource refs they expose — the discovery surface for /mcp.

// notice queues a dim informational line to scrollback.

// resolveRefs resolves a line's @references off the event loop via the
// controller, delivering a refsResolvedMsg with the tagged context block.

// runMCPPrompt resolves a /mcp__server__prompt command off the event loop via
// the controller, delivering a promptResolvedMsg with the rendered prompt.

// runExtensionAction invokes one extension UI action off the event loop (the
// call is a blocking sidecar round-trip), delivering an extensionActionMsg
// whose message surfaces as a transcript notice.

// replaySectionsFor turns a loaded session into scrollback blocks. Normal tool
// results remain quiet, while interrupted-turn reasoning and tool cards replay
// from provider-excluded LocalOnly records so restart matches the live view.
