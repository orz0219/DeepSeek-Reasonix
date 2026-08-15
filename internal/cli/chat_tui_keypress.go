package cli

import (
	"fmt"
	"reasonix/internal/control"
	"reasonix/internal/i18n"
	"reasonix/internal/memory"
	"reasonix/internal/sessioninbox"
	"runtime"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
)

func (m chatTUI) handleKeyPress(msg tea.KeyPressMsg, cmds []tea.Cmd, inputBeforeSelection *string) (tea.Model, tea.Cmd) {

	sel := m.sel
	m.sel = selection{}
	if m.validComposerSelection() && !m.composerSel.empty() {
		switch {
		case msg.String() == "ctrl+c" || msg.String() == "super+c" || msg.String() == "meta+c" || msg.String() == "ctrl+insert":
			cmds = append(cmds, m.copySelectionWithNotice(m.selectedComposerText()))
			return m, finalize(m, cmds)
		case imagePasteShortcut(msg.String(), runtime.GOOS):

		case msg.String() == "left":
			start, _ := m.composerSel.ordered()
			m.composerSel = composerSelection{}
			m.setComposerCursor(start)
			return m, finalize(m, cmds)
		case msg.String() == "right":
			_, end := m.composerSel.ordered()
			m.composerSel = composerSelection{}
			m.setComposerCursor(end)
			return m, finalize(m, cmds)
		default:
			*inputBeforeSelection = m.input.Value()
			if composerSelectionDeletes(msg, m.input.KeyMap) {
				m.deleteComposerSelection()
				m.growInputToFit()
				m.updateCompletion()
				if shouldClearWideInputChange(*inputBeforeSelection, m.input.Value()) {
					cmds = append(cmds, tea.ClearScreen)
				}
				return m, finalize(m, cmds)
			}
			if composerSelectionReplaces(msg, m.input.KeyMap) {
				m.deleteComposerSelection()
			} else {
				m.composerSel = composerSelection{}
			}
		}
	}

	switch msg.String() {
	case "pgup":
		m.viewport.PageUp()
		m.syncScrollModeAfterGesture()
		return m, finalize(m, cmds)
	case "pgdown":
		m.viewport.PageDown()
		m.syncScrollModeAfterGesture()
		return m, finalize(m, cmds)
	case "ctrl+home":
		m.viewport.GotoTop()
		m.markUserScrolled()
		return m, finalize(m, cmds)
	case "ctrl+end":
		m.viewport.GotoBottom()
		m.markFollowTail()
		return m, finalize(m, cmds)
	case "ctrl+z":
		return m, suspendWithMouseReset()
	}

	m.followComposerCursor()

	if m.chooser != nil {
		if m.chooser.typing {
			return m.handleChooserTypingKey(msg, cmds)
		}
		return m.handleChooserKey(msg)
	}

	if m.rewind != nil {
		return m.handleRewindKey(msg)
	}

	if m.mcpImport != nil {
		return m.handleMCPImportKey(msg)
	}

	if m.copyPick != nil {
		return m.handleCopyPickerKey(msg)
	}

	if m.resumePick != nil {
		return m.handleResumePickerKey(msg)
	}

	if m.quickPick != nil {
		return m.handleQuickPickerKey(msg)
	}

	if m.mcp != nil {
		return m.handleMCPManagerKey(msg)
	}

	if m.clearConfirm != nil {
		return m.handleClearConfirmKey(msg)
	}

	if m.skillPick != nil {
		return m.handleSkillPickerKey(msg)
	}

	if m.pendingApproval != nil {
		return m.handleApprovalKey(msg)
	}

	if m.completion.active {
		switch msg.String() {
		case "up", "ctrl+p":
			m.moveCompletion(-1)
			return m, nil
		case "down", "ctrl+n":
			m.moveCompletion(1)
			return m, nil
		case "tab", "enter":
			if msg.String() == "enter" && (m.completionExactLabel() || m.completionBareOverlayCommand()) {
				m.completion = completion{}
				break
			}

			if msg.String() == "enter" && m.completionSelectedInsertPresent() {
				m.completion = completion{}
				break
			}
			m.acceptCompletion()
			return m, nil
		case "esc":
			m.completion = completion{}
			if m.state == tuiRunning {
				break
			}
			return m, nil
		}
	}
	switch msg.String() {
	case "up":
		if m.state == tuiRunning {
			if m.navigateQueue(-1) {
				return m, nil
			}
		} else if m.recallSubmittedInput(-1) {
			return m, nil
		}
	case "down":
		if m.state == tuiRunning {
			if m.navigateQueue(1) {
				return m, nil
			}
		} else if m.recallSubmittedInput(1) {
			return m, nil
		}
	case "alt+up", "meta+up":
		if m.handleQueueReorder(-1) {
			return m, finalize(m, cmds)
		}
	case "alt+down", "meta+down":
		if m.handleQueueReorder(1) {
			return m, finalize(m, cmds)
		}
	case " ":
		if m.inboxQueuedCount() > 0 && m.input.Value() == "" {
			m.toggleInboxPaused()
			return m, finalize(m, cmds)
		}
	case "r":
		if m.queueEditCursor >= 0 && m.inboxSelectedID != "" {
			if err := m.ctrl.RetryInboxItem(m.inboxSelectedID); err != nil {
				m.notice("retry: " + err.Error())
			} else {
				m.notice("retry queued #" + shortID(m.inboxSelectedID))
			}
			return m, finalize(m, cmds)
		}
	case "d":
		if m.queueEditCursor >= 0 && m.inboxSelectedID != "" {
			if !m.queueConfirmDelete {
				m.queueConfirmDelete = true
				m.notice("press d again to delete #" + shortID(m.inboxSelectedID))
				return m, finalize(m, cmds)
			}
			if err := m.ctrl.DeleteInboxItem(m.inboxSelectedID); err != nil {
				m.notice("delete: " + err.Error())
			} else {
				m.notice("deleted #" + shortID(m.inboxSelectedID))
			}
			m.resetQueueNavigation()
			return m, finalize(m, cmds)
		}
	case "enter":

	default:
		m.resetSubmittedInputRecall()

		if m.queueEditCursor < 0 {
			m.resetQueueNavigation()
		} else {
			m.queueConfirmDelete = false
		}
	}
	if imagePasteShortcut(msg.String(), runtime.GOOS) {
		if m.state == tuiRunning {
			return m, nil
		}
		if cmd := m.beginClipboardImagePaste(); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, finalize(m, cmds)
	}

	if msg.String() == "shift+insert" {
		cmds = append(cmds, pasteClipboardText())
		return m, finalize(m, cmds)
	}

	if modeToggleKey(msg.String()) {

		m.cycleMode()
		return m, nil
	}
	switch msg.String() {
	case "esc":

		switch {
		case m.state == tuiRunning && m.bubblePending:
			m.unsendPending()
		case m.state == tuiRunning:
			m.ctrl.Cancel()

			if !m.ctrl.Running() {
				m.state = tuiIdle
				m.confirmBubbleSent()
				m.noteWatchdogIdle()
			}
		default:

			if strings.TrimSpace(m.input.Value()) == "" {
				if !m.lastEsc.IsZero() && time.Since(m.lastEsc) < 600*time.Millisecond {
					m.lastEsc = time.Time{}
					m.openRewind()
				} else {
					m.lastEsc = time.Now()
				}
			} else {
				m.input.Reset()
				m.pastedBlocks = nil
			}
		}
		return m, nil
	case "ctrl+insert":

		if sel.active && !sel.empty() {
			m.sel = sel
			text := m.selectedText()
			m.sel = selection{}
			cmds = append(cmds, m.copySelectionWithNotice(text))
			return m, finalize(m, cmds)
		}
		return m, nil
	case "ctrl+c", "super+c", "meta+c":
		if m.state == tuiRunning {

			if sel.active && !sel.empty() {
				m.sel = sel
				text := m.selectedText()
				m.sel = selection{}
				cmds = append(cmds, m.copySelectionWithNotice(text))
				return m, finalize(m, cmds)
			}
			if m.bubblePending {
				m.unsendPending()
			} else if m.cancelRequested() {
				m.ctrl.Cancel()
				return m, shutdownNow
			} else {
				m.ctrl.Cancel()
			}
			return m, nil
		}

		if sel.active && !sel.empty() {
			m.sel = sel
			text := m.selectedText()
			m.sel = selection{}
			cmds = append(cmds, m.copySelectionWithNotice(text))
			return m, finalize(m, cmds)
		}

		if strings.TrimSpace(m.input.Value()) != "" {
			m.input.Reset()
			m.pastedBlocks = nil
			m.lastCtrlCAt = time.Time{}
			return m, nil
		}
		if !m.lastCtrlCAt.IsZero() && time.Since(m.lastCtrlCAt) < 1500*time.Millisecond {
			return m, shutdownNow
		}
		m.lastCtrlCAt = time.Now()
		m.notice(i18n.M.CtrlCQuitHint)
		return m, finalize(m, nil)
	case "ctrl+d":

		if m.input.Value() != "" {
			// Delegate to textarea DeleteCharacterForward (bound to
			// ctrl+d by default) so mid-line forward delete works.
			var ic tea.Cmd
			m.input, ic = m.input.Update(msg)
			if ic != nil {
				cmds = append(cmds, ic)
			}
			m.growInputToFit()
			m.updateCompletion()
			return m, finalize(m, cmds)
		}
		if m.state == tuiIdle {
			return m, shutdownNow
		}
		return m, nil
	case "ctrl+l":
		if m.state != tuiRunning {
			m.finalizeStreamed()
			m.clearTranscriptDisplay()
			m.commitTranscriptSource(transcriptSource{kind: transcriptSourceBanner})
			m.transcriptDirty = true
			m.forceGotoBottom = true
			m.notice(i18n.M.SlashClsDone)
		}
		return m, finalize(m, cmds)
	case "ctrl+y", "super+y", "meta+y":
		m.toggleYoloMode()
		return m, nil
	case "ctrl+o":
		m.toggleVerboseReasoning(m.state != tuiRunning)
		return m, finalize(m, cmds)
	case "ctrl+b":
		m.toggleShellOutput()
		return m, finalize(m, cmds)
	case "ctrl+enter":

		if m.state == tuiRunning {
			line := strings.TrimSpace(m.input.Value())
			if line == "" {
				return m, nil
			}

			if handled, msg := m.handleQueueSlash(line); handled {
				m.notice(msg)
				m.input.Reset()
				m.pastedBlocks = nil
				return m, finalize(m, cmds)
			}
			body := m.expandPastedBlocks(line)
			rec, err := m.enqueueSteer(body, body)
			if err != nil {
				m.notice("steer: " + err.Error())

				return m, finalize(m, cmds)
			}
			switch rec.Disposition {
			case sessioninbox.DispositionSteerAccepted:
				m.notice(fmt.Sprintf("steer accepted #%s", shortID(rec.ItemID)))
			case sessioninbox.DispositionQueuedFollowup:
				m.notice(fmt.Sprintf("steer rejected — durable follow-up #%s", shortID(rec.ItemID)))
			default:
				m.notice(fmt.Sprintf("queued #%s", shortID(rec.ItemID)))
			}
			m.input.Reset()
			m.pastedBlocks = nil
			m.resetQueueNavigation()
			return m, finalize(m, cmds)
		}
	case "enter":
		if m.state == tuiRunning {
			line := strings.TrimSpace(m.input.Value())
			if line == "" {
				m.viewport.GotoBottom()
				m.markFollowTail()
				return m, nil
			}

			if handled, msg := m.handleQueueSlash(line); handled {
				m.notice(msg)
				m.input.Reset()
				m.pastedBlocks = nil
				return m, finalize(m, cmds)
			}
			body := m.expandPastedBlocks(line)
			items := m.inboxPreviews()
			if m.queueEditCursor >= 0 && m.queueEditCursor < len(items) {
				id := items[m.queueEditCursor].ID
				if _, err := m.ctrl.UpdateInboxItem(id, body, body, body); err != nil {
					m.notice("queue update: " + err.Error())
					return m, finalize(m, cmds)
				}
				m.notice(fmt.Sprintf("queue [%d] updated", m.queueEditCursor+1))
				m.resetQueueNavigation()
			} else {
				rec, err := m.enqueueFollowup(body, body)
				if err != nil {
					m.notice("queue: " + err.Error())

					return m, finalize(m, cmds)
				}
				m.notice(fmt.Sprintf("durable follow-up queued #%s — will run when idle", shortID(rec.ItemID)))
				m.resetQueueNavigation()
			}
			m.input.Reset()
			m.pastedBlocks = nil
			return m, finalize(m, cmds)
		}
		if m.modelSwitchPending {
			return m, nil
		}
		line := strings.TrimSpace(m.input.Value())

		if line == "" {
			m.viewport.GotoBottom()
			m.markFollowTail()
			return m, nil
		}
		if line == "exit" || line == "quit" || line == ":q" {
			return m, shutdownNow
		}

		if handled, msg := m.handleQueueSlash(line); handled {
			m.notice(msg)
			m.input.Reset()
			m.pastedBlocks = nil
			return m, finalize(m, cmds)
		}
		m.rememberSubmittedInput(line)

		if note, ok := control.MemoryQuickAddNote(line); ok {
			m.input.Reset()
			m.pastedBlocks = nil
			if note == "" {
				m.notice(i18n.M.QuickRememberEmpty)
			} else if path, err := m.ctrl.QuickAdd(memory.ScopeProject, note); err != nil {
				m.notice("memory: " + err.Error())
			} else {
				m.notice(fmt.Sprintf(i18n.M.QuickRememberDoneFmt, path))
			}
			return m, finalize(m, cmds)
		}

		if after, ok := strings.CutPrefix(line, "!"); ok {
			cmd := after
			if strings.TrimSpace(cmd) == "" {
				m.input.Reset()
				m.pastedBlocks = nil
				m.notice(i18n.M.ShellExecEmpty)
				return m, finalize(m, cmds)
			}
			m.input.Reset()
			m.pastedBlocks = nil
			m.state = tuiRunning
			m.runStart = time.Now()
			m.elapsed = 0
			m.turnTokens = 0
			m.pendingRestore = line
			m.bubbleStartIdx = len(m.transcript)
			m.commitLine("")
			m.commitTranscriptSource(transcriptSource{
				kind: transcriptSourceUser, raw: line, planMode: m.planMode,
			})
			m.bubblePending = true
			m.turnDiscarded = false
			m.confirmBubbleSent()
			m.noteWatchdogRunning()
			m.ctrl.RunShell(cmd)
			return m, tea.Batch(m.spinner.Tick, elapsedTick())
		}

		if control.SlashCodeCommentLine(line) {

		} else if strings.HasPrefix(line, "/") {
			if ref, ok := control.FileRefLine(line); ok {
				line = ref
			} else {
				m.input.Reset()
				m.pastedBlocks = nil
				cmds = append(cmds, m.runSlashCommand(line))
				return m, finalize(m, cmds)
			}
		}

		sentLine := m.expandPastedBlocks(line)
		m.input.Reset()

		if m.ctrl.HasRefs(sentLine) {
			cmds = append(cmds, m.resolveRefs(sentLine, sentLine, line))
			return m, finalize(m, cmds)
		}

		cmds = append(cmds, m.startTurnWithRaw(sentLine, sentLine, line, sentLine))
		return m, finalize(m, cmds)
	}

	beforeInput := m.input.Value()
	if *inputBeforeSelection != "" {
		beforeInput = *inputBeforeSelection
	}
	var ic tea.Cmd
	m.input, ic = m.input.Update(msg)
	cmds = append(cmds, ic)
	m.growInputToFit()
	m.updateCompletion()
	if shouldClearWideInputChange(beforeInput, m.input.Value()) {
		cmds = append(cmds, tea.ClearScreen)
	}
	return m, finalize(m, cmds)
}

// handleChooserTypingKey routes keys while the ask card is in free-text entry.
// An empty Enter confirms an input question (its empty answer is meaningful),
// but only after the user typed something for an option question.
func (m chatTUI) handleChooserTypingKey(msg tea.KeyPressMsg, cmds []tea.Cmd) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "enter":
		val := strings.TrimSpace(m.input.Value())
		m.input.Reset()
		m.chooser.typing = false
		m.refreshInputPlaceholder()
		if val == "" && !m.chooser.isInputTab() {
			return m, finalize(m, cmds)
		}
		m.chooser.custom[m.chooser.tab] = val
		m.chooser.sel[m.chooser.tab] = map[int]bool{}
		return m.chooserAdvance()
	case "esc":
		m.chooser.typing = false
		m.input.Reset()
		m.refreshInputPlaceholder()
		return m, finalize(m, cmds)
	}
	beforeInput := m.input.Value()
	var ic tea.Cmd
	m.input, ic = m.input.Update(msg)
	cmds = append(cmds, ic)
	m.growInputToFit()
	if shouldClearWideInputChange(beforeInput, m.input.Value()) {
		cmds = append(cmds, tea.ClearScreen)
	}
	return m, finalize(m, cmds)
}
