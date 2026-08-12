package cli

import (
	"fmt"
	"os"
	"strings"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/textarea"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"reasonix/internal/i18n"
	"reasonix/internal/plugin"
	"reasonix/internal/sessioninbox"
)

func transcriptContentWidth(termW int, nativeScrollback bool) int {
	if !nativeScrollback {
		termW--
	}
	return max(termW, 1)
}

// mouseCaptureOffByDefault lets a user opt out of in-app mouse capture for
// every run (e.g. a terminal/multiplexer combo where the native right-click
// menu and click-drag selection matter more than the scrollbar and
// wheel-scroll) without having to type "/mouse" each session.
func mouseCaptureOffByDefault() bool {
	v := strings.TrimSpace(os.Getenv("REASONIX_DISABLE_MOUSE"))
	return v != "" && v != "0"
}

func configureChatTextarea(ti *textarea.Model) {

	ti.SetPromptFunc(composerPromptWidth, func(info textarea.PromptInfo) string {
		if info.LineNumber != 0 {
			return ""
		}
		if info.Focused {
			return accent("❯ ")
		}
		return dim("❯ ")
	})
	ti.CharLimit = 16384

	ti.Placeholder = ""
	ti.DynamicHeight = true
	ti.MinHeight = 1
	ti.MaxHeight = maxInputRows
	ti.MaxContentHeight = ti.CharLimit
	ti.SetHeight(1)
	ti.ShowLineNumbers = false
	applyTextareaTheme(ti)

	ti.SetVirtualCursor(false)

	ti.KeyMap.InsertNewline = key.NewBinding(key.WithKeys("alt+enter", "ctrl+j", "shift+enter"))

	ti.KeyMap.WordForward = key.NewBinding(key.WithKeys("alt+right", "alt+f", "ctrl+right"))
	ti.KeyMap.WordBackward = key.NewBinding(key.WithKeys("alt+left", "alt+b", "ctrl+left"))

	ti.KeyMap.DeleteWordBackward = key.NewBinding(key.WithKeys("alt+backspace", "ctrl+w", "ctrl+backspace"))
	ti.Focus()
}

func (m *chatTUI) refreshInputPlaceholder() {
	if m.chooserTyping() {
		m.input.Placeholder = i18n.M.AskTypeSomething
		return
	}
	m.input.Placeholder = ""
}

func isTermuxTerminal() bool {
	if os.Getenv("TERMUX_VERSION") != "" || os.Getenv("TERMUX_APP_PID") != "" || os.Getenv("TERMUX__PREFIX") != "" {
		return true
	}
	return strings.Contains(os.Getenv("PREFIX"), "/com.termux/")
}

var detectTermuxTerminal = isTermuxTerminal

func (m *chatTUI) rememberSubmittedInput(input string) {
	if strings.TrimSpace(input) == "" {
		return
	}
	if len(m.submittedInputs) == 0 || m.submittedInputs[len(m.submittedInputs)-1] != input {
		m.submittedInputs = append(m.submittedInputs, input)
	}
	m.submittedInputCursor = -1
	m.submittedInputDraft = ""
}

func (m *chatTUI) recallSubmittedInput(delta int) bool {
	if len(m.submittedInputs) == 0 {
		return false
	}
	cursor := m.submittedInputCursor
	if cursor < 0 {
		if delta > 0 {
			return false
		}
		if m.input.Line() != 0 {
			return false
		}
		m.submittedInputDraft = m.input.Value()
		cursor = len(m.submittedInputs) - 1
	} else {
		cursor += delta
	}

	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(m.submittedInputs) {
		m.submittedInputCursor = -1
		m.input.SetValue(m.submittedInputDraft)
		m.growInputToFit()
		return true
	}
	m.submittedInputCursor = cursor
	m.input.SetValue(m.submittedInputs[cursor])
	m.growInputToFit()
	return true
}

func (m *chatTUI) resetSubmittedInputRecall() {
	m.submittedInputCursor = -1
	m.submittedInputDraft = ""
}

// navigateQueue moves through the durable inbox during tuiRunning.
// delta < 0 means ↑ (older), delta > 0 means ↓ (newer). Returns true if the
// input was updated. Bodies are loaded by ID only for the selected row.
func (m *chatTUI) navigateQueue(delta int) bool {
	items := m.inboxPreviews()
	if len(items) == 0 {
		return false
	}
	cursor := m.queueEditCursor
	if cursor < 0 {
		if delta > 0 {
			return false
		}

		m.queueEditDraft = m.input.Value()
		cursor = len(items) - 1
	} else {
		cursor += delta
	}

	if cursor < 0 {
		cursor = 0
	}
	if cursor >= len(items) {

		m.queueEditCursor = -1
		m.inboxSelectedID = ""
		m.input.SetValue(m.queueEditDraft)
		m.growInputToFit()
		return true
	}
	m.queueEditCursor = cursor
	m.inboxSelectedID = items[cursor].ID

	if _, env, err := m.ctrl.ReadInboxItem(items[cursor].ID); err == nil {
		m.input.SetValue(env.SubmitText)
	} else {
		m.input.SetValue(items[cursor].Preview)
	}
	m.growInputToFit()
	return true
}

// resetQueueNavigation resets the queue browsing cursor so the user returns to
// normal input mode. Any in-progress edit is discarded (the queued item keeps
// its previous value).
func (m *chatTUI) resetQueueNavigation() {
	m.queueEditCursor = -1
	m.queueEditDraft = ""
	m.inboxSelectedID = ""
	m.queueConfirmDelete = false
}

// renderQueueIndicator renders up to three bounded inbox previews above the
// input when instructions are queued. Full bodies are never materialised here.
func (m chatTUI) renderQueueIndicator() string {
	items := m.inboxPreviews()
	if len(items) == 0 {
		return ""
	}
	queueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	highlightStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("250"))
	var lines []string

	limit := min(len(items), 3)
	for i := range limit {
		it := items[i]
		preview := it.Preview
		if preview == "" {
			preview = "(empty)"
		}

		if r := []rune(preview); len(r) > 50 {
			preview = string(r[:47]) + "…"
		}
		cursor := " "
		style := queueStyle
		if m.queueEditCursor == i || m.inboxSelectedID == it.ID {
			cursor = "▸"
			style = highlightStyle
		}
		mark := ""
		switch it.State {
		case sessioninbox.StateUncertain:
			mark = " ?"
		case sessioninbox.StateBlocked:
			mark = " !"
		case sessioninbox.StateRunning, sessioninbox.StateSteerAccepted, sessioninbox.StateSteerConsumed:
			mark = " …"
		}
		lines = append(lines, style.Render(fmt.Sprintf("  %s [%d]%s %s", cursor, it.Pos, mark, preview)))
	}
	if more := len(items) - limit; more > 0 {
		lines = append(lines, queueStyle.Render(fmt.Sprintf("  … +%d more (/queue list)", more)))
	}
	if m.inboxSnap().Paused {
		lines = append(lines, queueStyle.Render("  ⏸ inbox paused (space to resume)"))
	}
	return strings.Join(lines, "\n")
}

// prompts returns the MCP prompts discovered at startup (nil when no plugins).
func (m *chatTUI) prompts() []plugin.Prompt {
	if m.host == nil {
		return nil
	}
	return m.host.Prompts()
}

func (m chatTUI) Init() tea.Cmd {
	return tea.Batch(
		textarea.Blink,
		waitForAgentEvent(m.eventCh),
		fetchBalance(m.ctrl),
		m.runStatusline(),
		m.refreshGitStatus(),
	)
}

func suspendWithMouseReset() tea.Cmd {
	return tea.Sequence(tea.Raw(resetMouseTracking), tea.Suspend)
}
