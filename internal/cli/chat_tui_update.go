package cli

import (
	"fmt"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"

	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/provider"
)

func (m chatTUI) Update(msg tea.Msg) (tea.Model, tea.Cmd) {

	if m.diagnostics != nil {
		m.diagnostics.NoteBooted()
	}
	logFirstFrame := false
	if m.diagnostics != nil && !m.firstFrameLogged {
		if _, ok := msg.(tea.WindowSizeMsg); ok {
			logFirstFrame = true
		}
	}

	followTail := m.shouldFollowTail()
	prevLines := len(m.transcript)
	prevWidth := m.width
	prevHeight := m.height
	prevYOff := m.viewport.YOffset()
	var resizeAnchor transcriptResizeAnchor
	if size, ok := msg.(tea.WindowSizeMsg); ok && size.Width != m.width && !followTail {
		resizeAnchor = captureTranscriptResizeAnchor(m.transcript, m.viewport.Width(), prevYOff)
	}

	next, cmd := m.update(msg)
	cm := next.(chatTUI)
	if logFirstFrame {
		cm.firstFrameLogged = true
		if cm.diagnostics != nil {
			cm.diagnostics.Milestone("first_frame")
		}
	}

	contentW := transcriptContentWidth(cm.width, cm.nativeScrollback)
	cm.viewport.SetWidth(contentW)

	cm.statusLineCount = cm.computeStatusLineCount(cm.width)

	cm.syncInputHeightLimit()
	cm.viewport.SetHeight(cm.transcriptHeight())
	widthChanged := cm.width != prevWidth
	if widthChanged {
		cm.reflowTranscript(cm.width)

		cm.sel = selection{}
	}

	forceFullWrap := widthChanged || len(cm.transcript) < prevLines
	wrapBehind := cm.wrapWidth != contentW || cm.wrapBlockCount != len(cm.transcript)
	if forceFullWrap || wrapBehind || len(cm.transcript) != prevLines {
		if cm.syncWrappedLines(contentW, forceFullWrap) {
			cm.feedViewportContent()
		}
		if followTail || cm.shouldFollowTail() {
			cm.viewport.GotoBottom()
			cm.markFollowTail()
		} else if widthChanged && resizeAnchor.valid {
			cm.viewport.SetYOffset(resizeAnchor.yOffset(cm.transcript, contentW))
		}
	} else if followTail && (cm.forceGotoBottom || cm.height != prevHeight) {

		cm.viewport.GotoBottom()
	}
	if cm.forceGotoBottom {
		cm.viewport.GotoBottom()
		cm.markFollowTail()
		cm.forceGotoBottom = false
	}
	cm.transcriptDirty = false

	// Rate-limited mouse re-enable after real resize, focus regain, or turn
	// settle so Windows ConPTY keeps wheel → MouseWheelMsg (#7583). Trailing
	// timer msgs are handled here too. Same-size WindowSizeMsg (session-switch
	// rebuilds) must not force a spurious Raw cmd.
	var mouseCmd tea.Cmd
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		if cm.width != prevWidth || cm.height != prevHeight {
			mouseCmd = cm.maybeReenableMouse()
		}
	case tea.FocusMsg:
		mouseCmd = cm.maybeReenableMouse()
	case mouseReenableMsg:
		mouseCmd = cm.handleMouseReenableMsg(v)
	}
	if cm.wantMouseReenable {
		cm.wantMouseReenable = false
		if c := cm.maybeReenableMouse(); c != nil {
			mouseCmd = batchCmds(mouseCmd, c)
		}
	}

	if cm.legacyScrollClear && cm.viewport.YOffset() != prevYOff && !cm.nativeScrollback && !cm.sessionSwitch {
		cm.sessionSwitch = false
		return cm, batchCmds(tea.ClearScreen, mouseCmd, cmd)
	}
	cm.sessionSwitch = false
	return cm, batchCmds(mouseCmd, cmd)
}

// batchCmds is tea.Batch that collapses an all-nil list to nil so callers can
// assert "no work" without false positives from Batch(nil, nil).
func batchCmds(cmds ...tea.Cmd) tea.Cmd {
	var out []tea.Cmd
	for _, c := range cmds {
		if c != nil {
			out = append(out, c)
		}
	}
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	default:
		return tea.Batch(out...)
	}
}

// update runs the model's message handling. Update wraps it to keep the
// transcript viewport sized, fed, and tail-following after every message.
func (m chatTUI) update(msg tea.Msg) (tea.Model, tea.Cmd) {
	var cmds []tea.Cmd
	var inputBeforeSelection string

	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.followComposerCursor()
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(max(msg.Width-4, 1))

		if !m.started {
			m.started = true
			history := append([]provider.Message(nil), m.history...)
			m.commitTranscriptSource(transcriptSource{
				kind: transcriptSourceReplayBundle, raw: m.missing, history: history,
			})
			m.history = nil
		}

	case tea.FocusMsg:

		return m, nil

	case tea.MouseWheelMsg:
		if m.mouseOverComposer(msg.X, msg.Y) {
			delta := 0
			switch msg.Button {
			case tea.MouseWheelUp:
				delta = -composerWheelRows
			case tea.MouseWheelDown:
				delta = composerWheelRows
			}
			if delta != 0 && m.scrollComposer(delta) {
				return m, nil
			}
		}

		switch msg.Button {
		case tea.MouseWheelUp:
			m.viewport.ScrollUp(3)
		case tea.MouseWheelDown:
			m.viewport.ScrollDown(3)
		}
		m.syncScrollModeAfterGesture()
		return m, nil

	case tea.MouseClickMsg:

		if msg.Button == tea.MouseMiddle {
			if m.hideComposer() {
				return m, nil
			}
			cmds = append(cmds, pasteMiddleClick())
			return m, finalize(m, cmds)
		}
		if msg.Button == tea.MouseRight && m.validComposerSelection() && !m.composerSel.empty() {
			cmds = append(cmds, m.copySelectionWithNotice(m.selectedComposerText()))
			return m, finalize(m, cmds)
		}
		if msg.Button == tea.MouseRight && m.sel.active && !m.sel.empty() {
			text := m.selectedText()
			m.sel = selection{}
			cmds = append(cmds, m.copySelectionWithNotice(text))
			return m, finalize(m, cmds)
		}
		if msg.Button == tea.MouseRight && !m.hideComposer() {
			cmds = append(cmds, pasteClipboardText())
			return m, finalize(m, cmds)
		}
		if msg.Button == tea.MouseLeft {
			if at, ok := m.composerCaretAt(msg.X, msg.Y, false); ok {
				m.sel = selection{}
				m.autoScroll = 0
				m.setComposerCursor(at.offset)
				m.composerSel = composerSelection{
					active: true, anchor: at.offset, head: at.offset, value: m.input.Value(),
				}
				return m, nil
			}
			m.composerSel = composerSelection{}
		}
		if msg.Button == tea.MouseLeft && m.inScrollbar(msg.X, msg.Y) {
			m.sel = selection{}
			m.autoScroll = 0
			m.scrollbarDrag = true
			m.scrollbarGrabOffset = m.scrollbarGrabRowOffset(msg.Y)
			m.dragScrollbar(msg.Y)
			return m, nil
		}
		if msg.Button == tea.MouseLeft && msg.Y < m.viewport.Height() {

			lineIdx := m.viewport.YOffset() + msg.Y
			if lineIdx >= 0 && lineIdx < len(m.wrappedLines) {
				clicked := m.wrappedLines[lineIdx]
				if strings.Contains(clicked, "more lines") && strings.Contains(clicked, "Ctrl+B") {
					m.toggleShellOutput()
					return m, finalize(m, cmds)
				}
			}
			at := m.transcriptCaret(msg.X, msg.Y)
			m.sel = selection{active: true, anchor: at, head: at}
			m.autoScroll = 0
		}
		return m, nil

	case tea.MouseMotionMsg:
		if m.validComposerSelection() {
			if at, ok := m.composerCaretAt(msg.X, msg.Y, true); ok {
				m.composerSel.head = at.offset
			}
			return m, nil
		}
		if m.scrollbarDrag {
			m.dragScrollbar(msg.Y)
			return m, nil
		}

		if m.sel.active {
			m.sel.head = m.transcriptCaret(msg.X, msg.Y)
			m.dragX = msg.X
			prev := m.autoScroll
			m.autoScroll = edgeScrollDir(msg.Y, m.viewport.Height())
			if m.autoScroll != 0 && prev == 0 {
				return m, autoScrollTick()
			}
		}
		return m, nil

	case autoScrollMsg:

		if !m.sel.active || m.autoScroll == 0 {
			return m, nil
		}
		edgeY := 0
		if m.autoScroll > 0 {
			m.viewport.ScrollDown(1)
			edgeY = m.viewport.Height() - 1
		} else {
			m.viewport.ScrollUp(1)
		}
		m.syncScrollModeAfterGesture()
		m.sel.head = m.transcriptCaret(m.dragX, edgeY)

		if (m.autoScroll > 0 && m.viewport.AtBottom()) || (m.autoScroll < 0 && m.viewport.AtTop()) {
			m.autoScroll = 0
			return m, nil
		}
		return m, autoScrollTick()

	case tea.MouseReleaseMsg:
		if msg.Button == tea.MouseLeft && m.validComposerSelection() {
			if at, ok := m.composerCaretAt(msg.X, msg.Y, true); ok {
				m.composerSel.head = at.offset
				m.setComposerCursor(at.offset)
			}
			if m.composerSel.empty() {
				m.composerSel = composerSelection{}
				return m, nil
			}

			cmds = append(cmds, m.copySelectionWithNotice(m.selectedComposerText()))
			return m, finalize(m, cmds)
		}

		if m.scrollbarDrag {
			m.dragScrollbar(msg.Y)
			m.scrollbarDrag = false
			m.scrollbarGrabOffset = 0
			return m, nil
		}
		m.autoScroll = 0
		if msg.Button == tea.MouseLeft && m.sel.active {
			if m.sel.empty() {
				m.sel = selection{}
			} else {
				cmds = append(cmds, m.copySelectionWithNotice(m.selectedText()))
			}
		}
		return m, finalize(m, cmds)

	case tea.PasteMsg:
		m.followComposerCursor()
		pasteBefore := m.input.Value()
		if m.state != tuiRunning && m.attachPastedImages(msg.Content) {
			if shouldClearWideInputChange(pasteBefore, m.input.Value()) {
				cmds = append(cmds, tea.ClearScreen)
			}
			return m, finalize(m, cmds)
		}
		if m.validComposerSelection() && !m.composerSel.empty() {
			inputBeforeSelection = pasteBefore
			m.deleteComposerSelection()
		}
		if ref, ok := pastedFileRef(msg.Content); ok {
			m.input.InsertString(ref + " ")
			m.growInputToFit()
			m.updateCompletion()
			if shouldClearWideInputChange(pasteBefore, m.input.Value()) {
				cmds = append(cmds, tea.ClearScreen)
			}
			return m, finalize(m, cmds)
		}
		if !m.chooserTyping() && m.pendingApproval == nil && m.rewind == nil && m.resumePick == nil && m.mcp == nil && m.clearConfirm == nil && m.mcpImport == nil && m.skillPick == nil && m.shouldFoldPaste(msg.Content) {
			m.insertFoldedPaste(msg.Content)
			m.growInputToFit()
			m.updateCompletion()
			if shouldClearWideInputChange(pasteBefore, m.input.Value()) {
				cmds = append(cmds, tea.ClearScreen)
			}
			return m, finalize(m, cmds)
		}

	case tea.KeyPressMsg:
		return m.handleKeyPress(msg, cmds, &inputBeforeSelection)

	case agentEventMsg:
		e := event.Event(msg)

		m.noteWatchdogHeartbeat(watchdogAgentSource(e.Kind))
		m.ingestEvent(e)
		turnDone := e.Kind == event.TurnDone
		gitMaybeChanged := e.Kind == event.ToolResult && !e.Tool.ReadOnly

	drain:
		for range maxEventDrain {
			select {
			case e2 := <-m.eventCh:
				m.noteWatchdogHeartbeat(watchdogAgentSource(e2.Kind))
				m.ingestEvent(e2)
				if e2.Kind == event.TurnDone {
					turnDone = true
				}
				if e2.Kind == event.ToolResult && !e2.Tool.ReadOnly {
					gitMaybeChanged = true
				}
			default:
				break drain
			}
		}
		cmds = append(cmds, waitForAgentEvent(m.eventCh))

		if turnDone {
			cmds = append(cmds, fetchBalance(m.ctrl))
			if c := m.runStatusline(); c != nil {
				cmds = append(cmds, c)
			}

			m.resetQueueNavigation()

			if c := m.drainQueuedRuntimeReload(); c != nil {
				cmds = append(cmds, c)
			}
		}
		if turnDone || gitMaybeChanged {
			if c := m.refreshGitStatus(); c != nil {
				cmds = append(cmds, c)
			}
		}

	case balanceMsg:
		m.balance = msg.text

	case statuslineMsg:
		m.statuslineOut = msg.out

	case gitStatusMsg:
		m.gitStatus = msg.status

	case compactDoneMsg:
		if msg.err != nil {
			m.notice(fmt.Sprintf("%s: %v", i18n.M.SlashCompactFailed, msg.err))
		} else {
			_ = m.ctrl.Snapshot()
			m.followSessionLease()
		}

	case tuiShutdownMsg:
		if m.ctrl != nil {
			_ = m.ctrl.Snapshot()
			m.followSessionLease()
		}
		return m, tea.Quit

	case modelSwitchMsg:
		m.modelSwitchPending = false
		m.pendingModelSwitch = nil
		if msg.err != nil {
			prefix := msg.failurePrefix
			if prefix == "" {
				prefix = "model"
			}
			m.notice(prefix + ": " + msg.err.Error())

			m.followSessionLease()
		} else {
			m.ctrl = msg.ctrl
			m.updateWatchdogStatusProvider()
			m.label = msg.label
			m.commands = msg.commands
			m.skills = msg.skills
			m.setHostAndInvalidateSlashCatalog(msg.host)
			m.modelRef = msg.ref
			if msg.profile != "" {
				m.runtimeProfile = msg.profile
			}
			m.refreshEffortStatus()

			if msg.oldCtrl != nil && msg.oldCtrl != msg.ctrl {
				m.oldControllers = append(m.oldControllers, msg.oldCtrl)
			}

			m.followSessionLease()
			if msg.successNotice != "" {
				m.notice(msg.successNotice)
			} else {
				m.notice(fmt.Sprintf(i18n.M.ModelSwitchedFmt, m.label))
			}
			cmds = append(cmds, fetchBalance(m.ctrl))
			if c := m.runStatusline(); c != nil {
				cmds = append(cmds, c)
			}

		}

		if c := m.drainQueuedRuntimeReload(); c != nil {
			cmds = append(cmds, c)
		}

	case promptResolvedMsg:
		switch {
		case msg.err != nil:
			m.commitLine(wrapForViewport(i18n.M.ErrorPrefix+" "+msg.err.Error(), m.width, activeCLITheme.warn))
		case strings.TrimSpace(msg.sent) == "":
			m.notice(i18n.M.SlashPromptEmpty)
		default:
			cmds = append(cmds, m.startTurn(msg.sent, msg.display, msg.display))
		}

	case extensionActionMsg:
		switch {
		case msg.err != nil:
			m.commitLine(wrapForViewport(i18n.M.ErrorPrefix+" "+msg.err.Error(), m.width, activeCLITheme.warn))
		case strings.TrimSpace(msg.message) != "":
			m.notice(msg.message)
		}

	case mcpExternalDoneMsg:
		m.handleMCPExternalDone(msg)

	case refsResolvedMsg:
		for _, e := range msg.errs {
			m.notice(e)
		}
		sent := msg.sent
		if msg.block != "" {
			sent = "Referenced context:\n\n" + msg.block + "\n\n" + msg.sent
		}

		cmds = append(cmds, m.startTurnWithRaw(sent, msg.display, msg.restore, msg.display))

	case clipboardImageMsg:
		m.clipboardImagePending = false
		if msg.err != nil {
			m.notice(fmt.Sprintf(i18n.M.ClipboardImagePasteFailedFmt, msg.err))
			break
		}
		imageBefore := m.input.Value()
		m.insertImageRef(msg.path)
		if shouldClearWideInputChange(imageBefore, m.input.Value()) {
			cmds = append(cmds, tea.ClearScreen)
		}

	case clipboardTextPasteMsg:
		if msg.remote {
			m.notice(i18n.M.ClipboardTextPasteRemoteHint)
			break
		}
		if msg.err != nil {
			m.notice(fmt.Sprintf(i18n.M.ClipboardTextPasteFailedFmt, msg.err))
			break
		}
		if msg.text == "" {
			break
		}

		return m.update(tea.PasteMsg{Content: msg.text})

	case clipboardCopyMsg:
		if msg.statusHint && msg.seq != m.copyNoticeSeq {
			break
		}
		label := i18n.M.MouseCopiedHint
		if !msg.statusHint {
			label = i18n.M.SlashCopyDone
		}
		if msg.osc52 || msg.err != nil {
			label = i18n.M.ClipboardCopyOSC52Hint
			if msg.err != nil {
				label = i18n.M.ClipboardCopyFallbackHint
			}
			cmds = append(cmds, tea.SetClipboard(msg.text))
		}
		if msg.statusHint {
			m.copyNoticeText = label
			cmds = append(cmds, copyNoticeExpire(msg.seq))
		} else {
			m.notice(label)
		}

	case copyNoticeExpireMsg:
		if msg.seq == m.copyNoticeSeq {
			m.copyNoticeText = ""
		}

	case themeSweepTickMsg:
		if m.themeSweep != nil {
			if m.themeSweep.advance() {
				cmds = append(cmds, themeSweepTick())
			} else {
				m.themeSweep = nil
			}
		}

	case elapsedTickMsg:
		if m.state == tuiRunning {

			m.noteWatchdogHeartbeat("elapsed_tick")
			m.elapsed = int(time.Since(m.runStart).Seconds())
			m.tickToolRunning()
			m.tickSubagentProgress()
			cmds = append(cmds, elapsedTick())
		}

	case spinner.TickMsg:
		if m.state == tuiRunning {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(msg)
			cmds = append(cmds, cmd)
		}
	}

	beforeInput := m.input.Value()
	if inputBeforeSelection != "" {
		beforeInput = inputBeforeSelection
	}
	var ic tea.Cmd
	m.input, ic = m.input.Update(msg)
	cmds = append(cmds, ic)
	m.growInputToFit()

	if _, ok := msg.(tea.KeyPressMsg); ok {
		m.updateCompletion()
	}
	if shouldClearWideInputChange(beforeInput, m.input.Value()) {
		cmds = append(cmds, tea.ClearScreen)
	}

	return m, finalize(m, cmds)
}

var clearWideInputChanges = runtime.GOOS == "windows"

func shouldClearWideInputChange(before, after string) bool {
	return clearWideInputChanges &&
		before != after &&
		(hasWideInputCells(before) || hasWideInputCells(after))
}

func hasWideInputCells(s string) bool {
	return s != "" && visibleWidth(s) != utf8.RuneCountInString(s)
}

// finalize drains the committed-line queue and batches the turn's commands. In
// the default alt-screen path the queue is already mirrored in m.transcript. In
// Termux finalized lines are also emitted into the terminal's native scrollback.
func finalize(m chatTUI, cmds []tea.Cmd) tea.Cmd {
	if m.nativeScrollback && len(*m.pendingCommit) > 0 {
		out := strings.TrimRight(clampWidth(strings.Join(*m.pendingCommit, "\n"), m.width), "\n")
		*m.pendingCommit = (*m.pendingCommit)[:0]
		var prints []tea.Cmd
		for _, chunk := range chunkLines(out, m.scrollChunkHeight()) {
			prints = append(prints, tea.Println(chunk))
		}
		cmds = append(cmds, tea.Sequence(prints...))
		return tea.Batch(cmds...)
	}
	*m.pendingCommit = (*m.pendingCommit)[:0]
	return tea.Batch(cmds...)
}
