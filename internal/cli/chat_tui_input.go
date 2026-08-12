package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/i18n"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// todoPanelMaxRows caps how many task lines the pinned panel shows; a long list
// is truncated with a "+N more" footer so the bottom region stays compact.
const todoPanelMaxRows = 8

type todoPanelTodo struct {
	Content    string `json:"content"`
	Status     string `json:"status"`
	ActiveForm string `json:"activeForm"`
	Level      int    `json:"level"`
}

// renderTodoPanel renders the task list pinned above the input from the latest
// todo_write call (m.todoArgs): a "Tasks done/total" header, completed items
// dimmed/checked, the in-progress one highlighted (its activeForm if given),
// pending ones muted. It returns "" when there's no list or every item is done,
// so the panel appears while work is outstanding and clears itself when finished.
func (m chatTUI) renderTodoPanel() string {
	var p struct {
		Todos []todoPanelTodo `json:"todos"`
	}
	if err := json.Unmarshal([]byte(m.todoArgs), &p); err != nil || len(p.Todos) == 0 {
		return ""
	}
	done := 0
	for _, t := range p.Todos {
		if t.Status == "completed" {
			done++
		}
	}
	if done == len(p.Todos) {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", accent("To-dos"), dim(fmt.Sprintf("%d/%d", done, len(p.Todos))))
	start, end := todoPanelWindow(p.Todos)
	if start > 0 {
		b.WriteString(dim(fmt.Sprintf("  +%d above", start)) + "\n")
	}
	for _, t := range p.Todos[start:end] {
		indent := "  "
		if t.Level >= 1 {
			indent = "      "
		}
		switch t.Status {
		case "completed":
			b.WriteString(indent + green("✔") + " " + dim(t.Content) + "\n")
		case "in_progress":
			label := t.Content
			if t.ActiveForm != "" {
				label = t.ActiveForm
			}
			b.WriteString(indent + yellow("▶ "+label) + "\n")
		default:
			b.WriteString(indent + dim("○ "+t.Content) + "\n")
		}
	}
	if end < len(p.Todos) {
		b.WriteString(dim(fmt.Sprintf("  +%d more", len(p.Todos)-end)) + "\n")
	}
	return todoPanelStyle.Width(max(m.width, 10)).Render(strings.TrimRight(b.String(), "\n"))
}

func todoPanelWindow(todos []todoPanelTodo) (int, int) {
	if len(todos) <= todoPanelMaxRows {
		return 0, len(todos)
	}
	active := -1
	for i, t := range todos {
		if t.Status == "in_progress" {
			active = i
			break
		}
	}
	if active < 0 {
		return 0, todoPanelMaxRows
	}
	start := max(active-todoPanelMaxRows/2, 0)
	if maxStart := len(todos) - todoPanelMaxRows; start > maxStart {
		start = maxStart
	}
	return start, start + todoPanelMaxRows
}

// truncateSubject trims a tool subject so the approval banner fits one line.
func truncateSubject(s string, width int) string {
	max := width - 28
	if max < 16 {
		max = 16
	}
	return ansi.Truncate(s, max, "…")
}

// wrapStatusLine wraps a status line to `width` visible columns, ANSI-aware,
// so text that exceeds one row flows onto additional lines instead of being
// truncated with an ellipsis. Wrapping is permissive — spaces are preferred
// break points — and works within the alt-screen view so there is no scrollback
// artifact.
func wrapStatusLine(s string, width int) string {
	if width <= 0 || s == "" {
		return s
	}
	return ansi.Hardwrap(s, width, true)
}

// computeStatusLineCount returns the number of terminal rows the status block
// (working line + first status line + optional data band) will occupy after
// wrapping to `width`. It mirrors the construction in View() so the reserved
// height matches the rendered height exactly — the load-bearing invariant for
// bottomRows().
// Use the same width (m.width) that View() passes to wrapStatusLine.
func (m chatTUI) computeStatusLineCount(width int) int {
	if m.ctrl == nil {
		return 3
	}
	shellMode := strings.HasPrefix(strings.TrimSpace(m.input.Value()), "!")
	cancelRequested := m.cancelRequested()

	modeTag := " " + m.modeTagText() + " "
	if shellMode {
		modeTag = " Shell "
	}
	primaryStatus := m.primaryStatusLine(modeTag, shellMode, cancelRequested)
	statusBlock := m.renderStatusBlock(primaryStatus, width)

	working := m.runningWorkingLine(cancelRequested, false)

	// Count wrapped rows for every piece that View() renders as wrapped.
	var lines int
	if m.state == tuiRunning {

		lines += strings.Count(wrapStatusLine(working, width), "\n") + 1
	}
	lines += strings.Count(statusBlock, "\n") + 1
	return lines
}

// The composer grows with its content up to this comfort cap. The effective
// cap is lowered for short terminals by syncInputHeightLimit, after which the
// textarea scrolls internally and keeps the caret visible.
const maxInputRows = 8

const foldedPasteMinChars = 1000
const foldedPasteMinLines = 5

type pastedBlock struct {
	label string
	text  string
	image bool // an image attachment: expands to its bare @ref, not a wrapped block
}

func (m *chatTUI) chooserTyping() bool {
	return m.chooser != nil && m.chooser.typing
}

// inputHeightLimit returns the number of visible textarea rows that fit without
// letting the complete composer block consume more than half the terminal or
// pushing the transcript below its minimum useful height. Panel and wrapped
// status rows are treated as fixed bottom chrome and remain outside the input
// viewport.
func (m chatTUI) inputHeightLimit() int {
	if m.height <= 0 {
		return maxInputRows
	}

	limit := maxInputRows

	halfScreen := max(1, m.height/2-composerBorderRows)
	limit = min(limit, halfScreen)

	fixedBottomRows := m.bottomRows()
	if !m.hideComposer() {
		fixedBottomRows -= m.input.Height() + composerBorderRows
	}
	available := max(1, m.height-fixedBottomRows-composerBorderRows-minTranscriptRows)
	return max(1, min(limit, available))
}

func (m *chatTUI) syncInputHeightLimit() {
	limit := m.inputHeightLimit()
	if m.input.MaxHeight == limit {
		return
	}
	m.followComposerCursor()
	m.input.MaxHeight = limit

	m.input.SetWidth(max(m.width-4, 1))
}

func (m *chatTUI) growInputToFit() {
	if m.input.DynamicHeight {
		return
	}
	lines := min(max(strings.Count(m.input.Value(), "\n")+1, 1), maxInputRows)
	if lines != m.input.Height() {
		m.input.SetHeight(lines)
	}
}

// modeToggleKey reports whether s is a recognized Shift+Tab encoding for the
// plan/approval mode cycle. Terminals may emit either "shift+tab" or CSI-Z
// "backtab" (#6660); both must hit cycleMode.
func modeToggleKey(s string) bool {
	switch s {
	case "shift+tab", "backtab":
		return true
	default:
		return false
	}
}

// cycleMode handles the Shift+Tab gesture using the same three safe modes users
// see in Claude Code: Ask → Auto → Plan → Ask. YOLO stays outside this cycle and
// remains an explicit Ctrl+Y choice.
func (m *chatTUI) cycleMode() {
	if m.ctrl == nil || m.ctrl.ToolApprovalMode() == control.ToolApprovalYolo {
		return
	}
	switch {
	case m.planMode:
		m.planMode = false
		m.ctrl.SetToolApprovalMode(control.ToolApprovalAsk)
	case m.ctrl.ToolApprovalMode() == control.ToolApprovalDontAsk:
		m.ctrl.SetToolApprovalMode(control.ToolApprovalAsk)
	case m.ctrl.ToolApprovalMode() == control.ToolApprovalAsk:
		m.ctrl.SetToolApprovalMode(control.ToolApprovalAuto)
	case m.ctrl.ToolApprovalMode() == control.ToolApprovalAuto:
		m.planMode = true
		m.ctrl.SetToolApprovalMode(control.ToolApprovalAsk)
		m.ctrl.ClearGoal()
	}
	m.ctrl.SetPlanMode(m.planMode)
}

func (m chatTUI) desktopShortcutLayout() bool {
	return m.cfg != nil && m.cfg.UIShortcutLayout() == "desktop"
}

func (m *chatTUI) toggleYoloMode() {
	if m.ctrl == nil {
		return
	}
	if m.ctrl.ToolApprovalMode() == control.ToolApprovalYolo {
		restore := m.yoloRestoreToolApprovalMode
		if restore != control.ToolApprovalAuto {
			restore = control.ToolApprovalAsk
		}
		m.ctrl.SetToolApprovalMode(restore)
		m.yoloRestoreToolApprovalMode = ""
		return
	}
	restore := m.ctrl.ToolApprovalMode()
	if restore != control.ToolApprovalAuto {
		restore = control.ToolApprovalAsk
	}
	m.yoloRestoreToolApprovalMode = restore
	m.ctrl.SetToolApprovalMode(control.ToolApprovalYolo)
}

func (m chatTUI) modeTagText() string {
	goalMode := strings.TrimSpace(m.ctrl.Goal()) != "" && m.ctrl.GoalStatus() == control.GoalStatusRunning
	toolApprovalMode := m.ctrl.ToolApprovalMode()
	if m.desktopShortcutLayout() {
		switch {
		case m.planMode && toolApprovalMode == control.ToolApprovalYolo:
			return "Plan+YOLO"
		case goalMode && toolApprovalMode == control.ToolApprovalYolo:
			return "Goal+YOLO"
		case toolApprovalMode == control.ToolApprovalYolo:
			return "YOLO"
		case m.planMode:
			return "Plan"
		case goalMode && toolApprovalMode == control.ToolApprovalAuto:
			return "Goal+Auto"
		case goalMode:
			return "Goal"
		case toolApprovalMode == control.ToolApprovalAuto:
			return "Auto"
		case toolApprovalMode == control.ToolApprovalDontAsk:
			return "Don't Ask"
		default:
			return "Ask"
		}
	}
	switch {
	case m.planMode && toolApprovalMode == control.ToolApprovalYolo:
		return "Plan+YOLO"
	case m.planMode && toolApprovalMode == control.ToolApprovalAuto:
		return "Plan+Approve"
	case goalMode && toolApprovalMode == control.ToolApprovalYolo:
		return "Goal+YOLO"
	case goalMode && toolApprovalMode == control.ToolApprovalAuto:
		return "Goal+Approve"
	case toolApprovalMode == control.ToolApprovalYolo:
		return "YOLO"
	case toolApprovalMode == control.ToolApprovalAuto:
		return "Auto+Approve"
	case toolApprovalMode == control.ToolApprovalDontAsk:
		return "Don't Ask"
	case m.planMode:
		return "Plan"
	case goalMode:
		return "Goal"
	default:
		return "Auto"
	}
}

func (m *chatTUI) toggleVerboseReasoning(notify bool) {
	m.showReasoning = !m.showReasoning
	var saveErr error
	if m.cfg != nil {
		_ = m.cfg.SetShowReasoning(m.showReasoning)
		path := config.SourcePath()
		if path == "" {
			path = "reasonix.toml"
		}
		saveErr = config.EditConfigFile(path, func(cfg *config.Config) error {
			return cfg.SetShowReasoning(m.showReasoning)
		})
	}
	if !notify {
		return
	}
	suffix := ""
	if saveErr != nil {
		suffix = "\npreference was not saved: " + saveErr.Error()
	}
	if m.showReasoning {
		m.notice("verbose on — thinking text will be shown" + suffix)
	} else {
		m.notice("verbose off — thinking text will stay collapsed" + suffix)
	}
}

// toggleMouseCapture flips whether Reasonix owns the mouse. It's session-only
// (unlike /verbose, this accommodates the terminal/multiplexer at hand rather
// than recording a lasting preference) — mirrors nativeScrollback, which is
// likewise never persisted to config. Clears any in-app selection/scrollbar
// drag in flight so a stale one can't be found mid-gesture once the terminal
// starts intercepting the events that would have finished it.
func (m *chatTUI) toggleMouseCapture() {
	m.mouseCaptureOff = !m.mouseCaptureOff
	m.sel = selection{}
	m.composerSel = composerSelection{}
	m.scrollbarDrag = false
	m.autoScroll = 0
	if m.mouseCaptureOff {
		m.notice(i18n.M.MouseCaptureOffHint)
	} else {
		m.notice(i18n.M.MouseCaptureOnHint)
	}
}

// startTurn commits the user bubble to scrollback, resets the turn accumulator,
// and kicks off the controller turn. `sent` goes to the model uncomposed (the
// controller frames it with any plan marker); `displayed` is what the transcript
// shows, and `restore` is what Esc puts back while the bubble is still deferred.
func (m *chatTUI) startTurn(sent, displayed, restore string) tea.Cmd {
	return m.startTurnWithRaw(sent, displayed, restore, sent)
}

// startTurnWithRaw is startTurn plus an explicit unresolved user prompt. This
// keeps reference-expanded model input separate from the text shown/restored by
// the frontend.
func (m *chatTUI) startTurnWithRaw(sent, displayed, restore, raw string) tea.Cmd {
	return m.startControllerTurn(displayed, restore, func() { m.ctrl.SendWithRaw(sent, raw) })
}

// startControllerTurn owns the TUI-side turn setup for controller entry points.
// Most prompts use SendWithRaw; slash-invoked skills use SubmitDisplay so the
// controller can choose inline vs isolated subagent execution from the live
// skill's RunAs metadata without the TUI reimplementing that policy.
func (m *chatTUI) startControllerTurn(displayed, restore string, start func()) tea.Cmd {

	m.commitReasoning()
	m.commitPending()

	m.pendingRestore = restore
	m.pendingPastes = m.pasteLabelsIn(restore)
	m.bubbleStartIdx = len(m.transcript)
	m.commitLine("")
	m.commitTranscriptSource(transcriptSource{
		kind: transcriptSourceUser, raw: displayed, planMode: m.planMode,
	})
	m.bubblePending = true
	m.turnDiscarded = false

	m.state = tuiRunning
	m.runStart = time.Now()
	m.elapsed = 0
	m.turnTokens = 0

	m.noteWatchdogRunning()
	start()
	return tea.Batch(m.spinner.Tick, elapsedTick())
}

// confirmBubbleSent marks the already-echoed user bubble as really sent once a
// turn's first response packet arrives, so Esc no longer un-sends it (it cancels
// the stream instead). Also called defensively at turn end. A no-op once confirmed.
func (m *chatTUI) confirmBubbleSent() {
	if !m.bubblePending {
		return
	}
	m.bubblePending = false
	m.pendingRestore = ""
}

// unsendPending "un-sends" the in-flight turn while the server hasn't replied yet
// (bubblePending): it pops the echoed bubble back off the transcript, restores the
// just-sent text to the input box, and cancels the request — marking the turn
// discarded so its already-buffered events reach nothing. Once a packet has arrived
// the bubble is confirmed and this path isn't taken (Esc cancels normally instead).
func (m *chatTUI) unsendPending() {
	m.input.SetValue(m.pendingRestore)
	m.growInputToFit()
	m.truncateTranscriptBlocks(m.bubbleStartIdx)
	m.transcriptDirty = true
	m.bubblePending = false
	m.pendingRestore = ""
	m.pendingPastes = nil
	m.turnDiscarded = true
	m.ctrl.Cancel()
}

const (
	composerBorderRows = 2
	minTranscriptRows  = 3
)
