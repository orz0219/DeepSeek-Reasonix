package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/agent"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/i18n"
	"reasonix/internal/migration"
	"reasonix/internal/outputstyle"
	"reasonix/internal/provider"
	"reasonix/internal/skill"
)

// runSlashCommand handles "/<cmd> <args>" input. Local commands queue their
// output to scrollback; MCP prompt / custom commands resolve to a model turn.
func (m *chatTUI) runSlashCommand(input string) tea.Cmd {
	typedCmd := strings.TrimSpace(strings.SplitN(input, " ", 2)[0])

	if strings.HasPrefix(typedCmd, "/mcp__") {
		return m.runMCPPrompt(input)
	}
	cmd := canonicalBuiltinSlashCommand(typedCmd)

	switch cmd {
	case "/compact":
		m.echoLocalCommand(input)

		focus := strings.TrimSpace(strings.TrimPrefix(input, typedCmd))
		return func() tea.Msg { return compactDoneMsg{err: m.ctrl.Compact(context.Background(), focus)} }
	case "/context":
		return m.showContextReport(input)
	case "/new":
		m.echoLocalCommand(input)
		if err := m.ctrl.NewSession(); err != nil {
			m.notice(fmt.Sprintf("%s: %v", i18n.M.SlashNewFailed, err))
			return nil
		}
		m.followSessionLease()

		m.resetFreshContextView(false)
		m.notice(i18n.M.SlashNewDone)
	case "/clear":
		m.echoLocalCommand(input)
		m.clearConfirm = &clearConfirm{confirm: 1}
	case "/cls":
		m.echoLocalCommand(input)
		m.finalizeStreamed()
		m.clearTranscriptDisplay()
		m.commitLine(strings.TrimRight(
			renderTUIBanner(m.label, "", transcriptContentWidth(m.width, m.nativeScrollback)), "\n"))
		m.transcriptDirty = true
		m.forceGotoBottom = true
		m.notice(i18n.M.SlashClsDone)
	case "/resume":
		m.runResumeCommand(input)
	case "/status":
		m.echoLocalCommand(input)
		m.showStatusDetails()
	case "/rename":
		m.runRenameCommand(input)
	case "/todo":
		m.echoLocalCommand(input)

		m.todoArgs = ""
		m.notice(i18n.M.SlashTodoCleared)
	case "/verbose":
		m.toggleVerboseReasoning(true)
	case "/mouse":
		m.toggleMouseCapture()
	case "/sandbox":
		m.echoLocalCommand(input)
		m.showSandboxStatus()
	case "/effort":
		return m.runEffortCommand(input)
	case "/preset", "/work-mode", "/profile":
		m.echoLocalCommand(input)
		return m.runPresetCommand(input)
	case "/reasoning-language":
		m.echoLocalCommand(input)
		m.runReasoningLanguageCommand(input)
	case "/rewind":
		m.echoLocalCommand(input)
		m.openRewind()
	case "/tree":
		m.echoLocalCommand(input)
		m.showBranchTree()
	case "/branch":
		m.echoLocalCommand(input)
		m.runBranchCommand(input)
	case "/switch":
		m.echoLocalCommand(input)
		m.runSwitchCommand(input)
	case "/mcp":
		m.echoLocalCommand(input)
		m.runMCPSubcommand(input)
	case "/remote":
		m.echoLocalCommand(input)
		m.showRemoteHosts()
	case "/plugin", "/plugins":
		m.echoLocalCommand(input)
		m.runPluginSubcommand(input)
	case "/model":
		m.echoLocalCommand(input)
		m.runModelSubcommand(input)
		if m.pendingModelSwitch != nil {
			return m.pendingModelSwitch
		}
	case "/provider":
		m.echoLocalCommand(input)
		m.runProviderCommand(input)
		if m.pendingModelSwitch != nil {
			return m.pendingModelSwitch
		}
	case "/skill", "/skills":
		m.echoLocalCommand(input)
		m.runSkillSubcommand(input)
		if m.pendingModelSwitch != nil {
			return m.pendingModelSwitch
		}
	case "/hooks":
		m.echoLocalCommand(input)
		m.runHooksSubcommand(input)
	case "/reload-cmd":
		m.echoLocalCommand(input)
		if m.ctrl == nil {
			m.notice("controller not ready")
			return nil
		}
		if m.ctrl.Running() {
			m.notice("wait for the current turn to finish, then retry /reload-cmd")
			return nil
		}
		prev := len(m.commands)
		err := m.ctrl.ReloadCommands(context.Background())
		m.commands = m.ctrl.Commands()
		m.invalidateSlashCatalog()
		m.updateCompletion()
		if err != nil {
			m.notice("reload-cmd: " + err.Error())
			return nil
		}
		m.notice(fmt.Sprintf("commands reloaded: %d → %d commands", prev, len(m.commands)))

	case "/reload":
		m.echoLocalCommand(input)
		return m.runReloadCommand()

	case "/paste-image":
		return m.beginClipboardImagePaste()
	case "/output-style", "/output-styles":
		m.echoLocalCommand(input)
		styles := outputstyle.List(outputstyle.Dirs())
		if len(styles) == 0 {
			m.notice(i18n.M.OutputStyleNone)
		} else {
			m.commitLine(renderOutputStyles(m.width, styles, m.outputStyle))
		}
	case "/diff-fold":
		m.echoLocalCommand(input)
		if m.diffMaxLines == 0 {
			m.diffMaxLines = diffFoldLimit
			m.notice(fmt.Sprintf(i18n.M.DiffFoldEnabledFmt, diffFoldLimit))
		} else {
			m.diffMaxLines = 0
			m.notice(i18n.M.DiffFoldDisabled)
		}
	case "/theme":
		m.echoLocalCommand(input)
		return m.runThemeSubcommand(input)
	case "/language":
		m.echoLocalCommand(input)
		return m.runLanguageSubcommand(input)
	case "/currency":
		m.echoLocalCommand(input)
		return m.runCurrencySubcommand(input)
	case "/help", "/web":
		return m.runHelpOrWebSlash(input, typedCmd)
	case "/memory":
		m.echoLocalCommand(input)
		m.showMemory(input)
	case "/migrate", "/migration":
		m.echoLocalCommand(input)
		migration.RunLegacyRescueCommand(strings.TrimSpace(strings.TrimPrefix(input, typedCmd)), event.FuncSink(func(e event.Event) {
			if e.Kind == event.Notice {
				m.notice(e.Text)
			}
		}))
	case "/goal":
		return m.runGoalSubcommand(input)
	case "/remember":
		m.rememberNote(strings.TrimSpace(strings.TrimPrefix(input, typedCmd)))
	case "/quit", "/exit":
		return shutdownNow
	case "/copy":
		return m.runCopyCommand(input)
	case "/export":
		m.runExportCommand(input)
	case "/forget":
		m.forgetMemory(strings.TrimSpace(strings.TrimPrefix(input, typedCmd)))
	default:
		if control.IsBuiltinDocsSlash(typedCmd, m.commands, m.skills) {
			query := strings.TrimSpace(strings.TrimPrefix(input, typedCmd))
			if query != "" {
				return m.startControllerTurn(input, input, func() { m.ctrl.SubmitDisplay(input, input) })
			}
			m.echoLocalCommand(input)
			text, err := control.DocsCommandOverviewFor(typedCmd)
			if err != nil {
				m.notice("docs: " + err.Error())
			} else {
				m.commitLine(text)
			}
			return nil
		}

		if sent, ok := m.ctrl.CustomCommand(input); ok {
			return m.startTurn(sent, input, input)
		}
		if _, ok := m.ctrl.RunSkill(input); ok {
			fields := strings.Fields(input)
			name := strings.TrimPrefix(fields[0], "/")
			for _, sk := range m.ctrl.Skills() {
				if sk.Name == name && sk.RunAs == skill.RunSubagent && len(fields) == 1 {
					m.echoLocalCommand(input)
					m.notice("usage: /" + name + " <task>")
					return nil
				}
			}
			return m.startControllerTurn(input, input, func() { m.ctrl.SubmitDisplay(input, input) })
		}

		if action, ok := matchExtensionAction(m.ctrl, typedCmd); ok {
			m.echoLocalCommand(input)
			return m.runExtensionAction(action.Slash, parseExtensionActionArgs(strings.Fields(input)[1:]))
		}

		m.notice(fmt.Sprintf("%s: %s — %s", i18n.M.SlashUnknown, cmd, i18n.M.SlashUnknownSentAsMessage))
		return m.startTurn(input, input, input)
	}
	return nil
}

// showStatusDetails keeps diagnostics available without permanently crowding
// the two-line composer footer.
func (m *chatTUI) showStatusDetails() {
	var lines []string
	lines = append(lines, viewHeader("%s", "Session status"))
	mode := "Ask"
	if m.ctrl != nil {
		mode = m.modeTagText()
	}
	lines = append(lines, "  mode       "+mode)
	model := strings.TrimSpace(m.modelRef)
	if model == "" {
		model = strings.TrimSpace(m.label)
	}
	if model != "" {
		lines = append(lines, "  model      "+model)
	}
	if m.ctrl != nil {
		if tag := m.contextTag(); tag != "" {
			lines = append(lines, "  context    "+tag)
		}
	}
	if tag := m.workModeTag(); tag != "" {
		lines = append(lines, "  profile    "+tag)
	}
	if m.effortLevel != "" {

		lines = append(lines, "  effort     effort "+m.effortLevel)
	}
	if m.ctrl != nil {
		if tag := m.cacheTag(); tag != "" {
			lines = append(lines, "  cache      "+tag)
		}
	}
	if tag := m.gitTag(); tag != "" {
		lines = append(lines, "  git        "+tag)
	}
	if m.ctrl != nil {
		if tag := m.jobsTag(); tag != "" {
			lines = append(lines, "  jobs       "+tag)
		}
	}
	if m.balance != "" {
		lines = append(lines, "  balance    "+m.balance)
	}
	if tag := m.mouseTag(); tag != "" {
		lines = append(lines, "  mouse      "+tag)
	}
	lines = append(lines, "  config     "+activeConfigTag())
	m.commitLine(strings.Join(lines, "\n"))
}

// activeConfigTag names the config file actually in effect. A ./reasonix.toml
// outranks the user-global file, so a session started in a directory holding
// one silently ignores global edits unless the source is visible (#3317).
func activeConfigTag() string {
	path := config.SourcePath()
	if path == "" {
		return "(defaults — no config file)"
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return displayPath(path)
	}
	return displayPath(abs)
}

func (m *chatTUI) runGoalSubcommand(input string) tea.Cmd {
	cmd, ok := control.ParseGoalCommand(input)
	if !ok {
		m.echoLocalCommand(input)
		m.notice(i18n.M.GoalEmpty)
		return nil
	}
	switch m.noticeDeprecatedGoalBudget(cmd); cmd.Action {
	case control.GoalCommandSet:
		return m.setGoalCommand(cmd, input)
	case control.GoalCommandClear:
		m.echoLocalCommand(input)
		m.ctrl.ClearGoal()
		m.notice(i18n.M.GoalCleared)
	case control.GoalCommandPause:
		m.echoLocalCommand(input)
		if !m.ctrl.PauseGoal() {
			m.notice(i18n.M.GoalNotRunning)
		}
	case control.GoalCommandResume:
		m.echoLocalCommand(input)
		if !m.ctrl.ResumeGoal() {
			m.notice(i18n.M.GoalNotPaused)
		}
	default:
		m.echoLocalCommand(input)
		goal := m.ctrl.Goal()
		if strings.TrimSpace(goal) == "" {
			m.notice(i18n.M.GoalEmpty)
			break
		}
		m.notice(fmt.Sprintf(i18n.M.GoalCurrentFmt, goal))
		rt := m.ctrl.GoalRuntime()
		m.notice(fmt.Sprintf(i18n.M.GoalRuntimeFmt,
			rt.TurnsUsed, rt.RequestsUsed, rt.TokensUsed,
			control.GoalWorkDurationText(rt.WorkDurationMs)))
		if rt.LastReason != "" {
			m.notice(fmt.Sprintf("%s: %s", i18n.M.GoalRuntimeLastReason, rt.LastReason))
		}
		if rt.StopCause != "" {
			m.notice(fmt.Sprintf(i18n.M.GoalPausedFmt, rt.StopCause))
		}
	}
	return nil
}

// runCopyCommand copies the Nth-latest assistant message from the current turn
// (after the last user message) to the clipboard.
//
//   - "/copy"   — shows a numbered list of assistant messages to choose from.
//   - "/copy N" — copies the Nth message directly (1 = most recent).
//
// Counting does not cross user message boundaries.
func (m *chatTUI) runCopyCommand(input string) tea.Cmd {
	m.echoLocalCommand(input)

	arg := strings.TrimSpace(strings.TrimPrefix(input, "/copy"))
	if n, err := strconv.Atoi(arg); err == nil && n > 0 {
		msgs := m.ctrl.History()
		parts := copyAssistantParts(msgs)
		if len(parts) == 0 {
			m.notice(i18n.M.SlashCopyEmpty)
			return nil
		}

		idx := len(parts) - n
		if idx < 0 || idx >= len(parts) {
			m.notice(i18n.M.SlashCopyEmpty)
			return nil
		}
		return copyToClipboard(parts[idx])
	}
	m.openCopyPicker()
	return nil
}

// firstLine returns the first non-empty line of s, truncated to 80 runes.
func firstLine(s string) string {
	for line := range strings.SplitSeq(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			runes := []rune(t)
			if len(runes) > 80 {
				return string(runes[:77]) + "..."
			}
			return t
		}
	}
	return "..."
}

// copyAssistantParts returns the Content of assistant messages after the last
// user message in msgs, skipping empty strings and model placeholders ("…", "...").
// The result is chronological (oldest first).
func copyAssistantParts(msgs []provider.Message) []string {
	lastUserIdx := -1
	for i, v := range slices.Backward(msgs) {
		if v.Role == provider.RoleUser {
			lastUserIdx = i
			break
		}
	}
	start := lastUserIdx + 1
	if lastUserIdx < 0 {
		start = 0
	}
	var parts []string
	for i := start; i < len(msgs); i++ {
		if msgs[i].Role != provider.RoleAssistant {
			continue
		}
		c := strings.TrimSpace(msgs[i].Content)
		if c == "" || c == "..." || c == "…" {
			continue
		}
		parts = append(parts, c)
	}
	return parts
}

// runExportCommand exports the entire session as a markdown file, excluding
// system messages, reasoning/thinking content, and tool calls/results.
func (m *chatTUI) runExportCommand(input string) {
	m.echoLocalCommand(input)
	msgs := m.ctrl.History()
	if len(msgs) == 0 {
		m.notice(i18n.M.SlashExportEmpty)
		return
	}

	var b strings.Builder
	b.WriteString("# reasonix session\n\n")
	lastRole := provider.Role("")
	exportedMessages := 0
	for _, msg := range msgs {
		switch msg.Role {
		case provider.RoleUser:

			if _, isSteer := agent.SteerText(msg.Content); isSteer {
				continue
			}
			content := exportUserContent(msg.Content)
			if content == "" {
				continue
			}
			if lastRole != provider.RoleUser {
				b.WriteString("## User\n\n")
			}
			b.WriteString(content)
			b.WriteString("\n\n")
			exportedMessages++
			lastRole = provider.RoleUser
		case provider.RoleAssistant:
			content := strings.TrimSpace(msg.Content)
			if content == "" {
				continue
			}
			if lastRole != provider.RoleAssistant {
				b.WriteString("## Assistant\n\n")
			}
			b.WriteString(content)
			b.WriteString("\n\n")
			exportedMessages++
			lastRole = provider.RoleAssistant
		}
	}
	if exportedMessages == 0 {
		m.notice(i18n.M.SlashExportEmpty)
		return
	}

	dir := "."
	if m.ctrl != nil {
		if wr := m.ctrl.WorkspaceRoot(); wr != "" {
			dir = wr
		}
	}
	ts := time.Now().Format("20060102-150405")
	filename := fmt.Sprintf("session-%s.md", ts)
	path := filepath.Join(dir, filename)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		m.notice(fmt.Sprintf("%s: %v", i18n.M.SlashUnknown, err))
		return
	}
	m.notice(fmt.Sprintf(i18n.M.SlashExportDoneFmt, path))
}

func exportUserContent(content string) string {
	content = control.StripComposePrefixes(content)
	content = control.StripReferencedContextPrefix(content)
	return strings.TrimSpace(content)
}

func (m *chatTUI) echoLocalCommand(input string) {
	input = strings.TrimSpace(input)
	if input == "" {
		return
	}
	m.commitLine(dim("  › " + input))
}

// commandNames renders the custom command list for /help, "" when there are none.
func (m *chatTUI) commandNames() string {
	names := make([]string, 0, len(m.commands))
	for _, c := range m.commands {
		if !c.Hidden {
			names = append(names, "/"+c.Name)
		}
	}
	return strings.Join(names, " · ")
}
