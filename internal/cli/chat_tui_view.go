package cli

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"

	"reasonix/internal/event"
	"reasonix/internal/i18n"
)

func (m chatTUI) View() tea.View {
	if m.themeSweep != nil {
		v := tea.NewView(m.themeSweep.render())
		if !m.nativeScrollback {
			v.AltScreen = true
			if m.mouseCaptureOff {
				v.MouseMode = tea.MouseModeNone
			} else {
				v.MouseMode = tea.MouseModeCellMotion
			}
		}
		return v
	}
	boxW := max(m.width, 10)
	hideComposer := m.hideComposer()
	shellMode := strings.HasPrefix(strings.TrimSpace(m.input.Value()), "!")
	cancelRequested := m.cancelRequested()
	var box string
	if !hideComposer {
		style := inputBoxStyle.Width(boxW)
		if shellMode {
			style = withThemeBorderFG(style, statusShellColor)
		}
		box = style.Render(m.renderComposerInput())
	}

	var modeTag string
	if shellMode {
		modeTag = modeTagStyle(statusShellColor, modeTagLight).Render("Shell")
	} else {
		background := statusAutoColor
		foreground := modeTagDark
		switch {
		case m.ctrl.AutoApproveTools():
			background = statusYoloColor
			foreground = modeTagLight
		case m.planMode:
			background = statusPlanColor
			foreground = modeTagLight
		}
		modeTag = modeTagStyle(background, foreground).Render(m.modeTagText())
	}

	primaryStatus := m.primaryStatusLine(modeTag, shellMode, cancelRequested)

	working := m.runningWorkingLine(cancelRequested, true)
	// Bottom region pinned under the transcript viewport: optional panels, the
	// composer when visible, then the two status rows. Its height feeds
	// transcriptHeight so the viewport above fills exactly the rest of the screen.
	var parts []string
	rowsAboveBox := 0
	if todo := m.renderTodoPanel(); todo != "" {
		parts = append(parts, todo)
		rowsAboveBox += strings.Count(todo, "\n") + 1
	}
	if banner := m.renderApprovalBanner(); banner != "" {
		parts = append(parts, banner)
		rowsAboveBox += strings.Count(banner, "\n") + 1
	}
	if card := m.renderChooser(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if card := m.renderRewind(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if card := m.renderMCPImport(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if card := m.renderResumePicker(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if card := m.renderQuickPicker(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if card := m.renderCopyPicker(); card != "" {
		parts = append(parts, card)
		rowsAboveBox += strings.Count(card, "\n") + 1
	}
	if menu := m.renderCompletion(); menu != "" {
		parts = append(parts, menu)
		rowsAboveBox += strings.Count(menu, "\n") + 1
	}
	if m.nativeScrollback {
		if card := m.renderMainManager(); card != "" {
			parts = append(parts, card)
			rowsAboveBox += strings.Count(card, "\n") + 1
		}
	}

	if working != "" {
		parts = append(parts, workingStyle.Width(boxW).MaxWidth(boxW).Render(wrapStatusLine(working, boxW)))
		rowsAboveBox++
	}
	if footer := m.renderMainManagerFooter(); footer != "" {
		parts = append(parts, footer)
		rowsAboveBox += strings.Count(footer, "\n") + 1
	}
	statusBlock := m.renderStatusBlock(primaryStatus, boxW)
	if !hideComposer {
		if qi := m.renderQueueIndicator(); qi != "" {
			parts = append(parts, qi)
			rowsAboveBox += strings.Count(qi, "\n") + 1
		}
		parts = append(parts, box)
	}
	parts = append(parts, statusBlockStyle.Width(boxW).MaxWidth(boxW).Render(statusBlock))

	if m.nativeScrollback {
		v := tea.NewView(strings.Join(parts, "\n"))
		if !hideComposer {
			if cur := m.composerCursor(); cur != nil {
				cur.X += 1
				cur.Y += rowsAboveBox + 1
				v.Cursor = clampCursorToTerminal(cur, m.width, m.height)
			}
		}
		return v
	}

	mainArea := m.renderTranscript()
	if card := m.renderMainManager(); card != "" {
		mainArea = m.renderTranscriptWithMainManager(card)
	}
	v := tea.NewView(mainArea + "\n" + strings.Join(parts, "\n"))
	v.AltScreen = true
	if m.mouseCaptureOff {

		v.MouseMode = tea.MouseModeNone
	} else {
		v.MouseMode = tea.MouseModeCellMotion
	}

	if !hideComposer {
		if cur := m.composerCursor(); cur != nil {
			cur.X += 1
			cur.Y += m.viewport.Height() + rowsAboveBox + 1
			v.Cursor = clampCursorToTerminal(cur, m.width, m.height)
		}
	}
	return v
}

// clampCursorToTerminal keeps the reported caret inside [0,w) × [0,h).
func clampCursorToTerminal(cur *tea.Cursor, width, height int) *tea.Cursor {
	if cur == nil {
		return nil
	}
	if width > 0 {
		if cur.X < 0 {
			cur.X = 0
		}
		if cur.X >= width {
			cur.X = width - 1
		}
	}
	if height > 0 {
		if cur.Y < 0 {
			cur.Y = 0
		}
		if cur.Y >= height {
			cur.Y = height - 1
		}
	}
	return cur
}

// compactionCardLines renders a finished compaction as a titled card: a header
// with the message count and trigger, then the structured summary under a dim
// gutter so it reads as one block in scrollback. The summary is also the new
// context base, so this card is the user's window into exactly what was kept.
func compactionCardLines(c event.Compaction) []string {
	trigger := c.Trigger
	switch c.Trigger {
	case "auto":
		trigger = i18n.M.CompactionAuto
	case "manual":
		trigger = i18n.M.CompactionManual
	}
	header := fmt.Sprintf("%s · %d %s · %s", i18n.M.CompactionTitle, c.Messages, i18n.M.CompactionUnit, trigger)
	lines := []string{accent("◆ " + header)}
	for ln := range strings.SplitSeq(strings.TrimRight(c.Summary, "\n"), "\n") {
		lines = append(lines, dim("  │ "+ln))
	}
	if c.Archive != "" {
		lines = append(lines, dim("  │ archived "+c.Archive))
	}
	return lines
}

// contextTag renders the prompt-vs-context-window gauge for the status line,
// framed around the auto-compaction threshold: it shows how much headroom is
// left until the next compaction, and colours by proximity to that point rather
// than the raw window. Falls back to a plain percentage when compaction is disabled.
func (m chatTUI) contextTag() string {
	used, window := m.ctrl.ContextSnapshot()
	if used == 0 || window == 0 {
		return ""
	}
	pct := used * 100 / window
	ratio := m.ctrl.CompactRatio()
	if ratio <= 0 || ratio >= 1 {

		body := fmt.Sprintf("%s / %s ctx (%d%%)", shortTokens(used), shortTokens(window), pct)
		switch {
		case pct >= 85:
			return themeStyle(activeCLITheme.danger).Render(body)
		case pct >= 60:
			return themeStyle(activeCLITheme.warn).Render(body)
		default:
			return dim(body)
		}
	}
	threshold := int(ratio * 100)

	left := max(threshold-pct, 0)
	body := fmt.Sprintf("%s ctx (%d%%) · %d%% to compact", shortTokens(used), pct, left)
	switch {
	case pct >= threshold:
		return themeStyle(activeCLITheme.danger).Render(fmt.Sprintf("%s ctx (%d%%) · compacting soon", shortTokens(used), pct))
	case left <= 10:
		return themeStyle(activeCLITheme.warn).Render(body)
	default:
		return dim(body)
	}
}

func cacheRateLabel(format string, hit, denom int) string {
	if denom <= 0 {
		return ""
	}
	return fmt.Sprintf(format, fmt.Sprintf("%.2f%%", float64(hit)*100/float64(denom)))
}

// cacheTag renders both prompt cache-hit rates for the status line —
// "turn hit 88.00% · avg 78.00%": the single-turn rate (latest turn, the higher/steeper
// number on a non-compacting DeepSeek session) and the session-aggregate rate
// Σhit/Σ(hit+miss) (the steadier, cost-oriented number that matches the legacy
// dashboard). "" before any cache tokens have been reported.
func (m chatTUI) cacheStatus() (body string, rate float64, ok bool) {
	now := ""
	nowRate := 0.0
	if u := m.ctrl.LastUsage(); u != nil {

		now = cacheRateLabel(i18n.M.ChatStatusCacheNowFmt, u.CacheHitTokens, u.CacheHitTokens+u.CacheMissTokens)
		if denom := u.CacheHitTokens + u.CacheMissTokens; denom > 0 {
			nowRate = float64(u.CacheHitTokens) * 100 / float64(denom)
		}
	}
	avg := ""
	avgRate := 0.0
	if hit, miss := m.ctrl.SessionCache(); hit+miss > 0 {
		avg = cacheRateLabel(i18n.M.ChatStatusCacheAvgFmt, hit, hit+miss)
		avgRate = float64(hit) * 100 / float64(hit+miss)
	}
	switch {
	case now != "" && avg != "":
		return now + " · " + avg, avgRate, true
	case now != "":
		return now, nowRate, true
	case avg != "":
		return avg, avgRate, true
	}
	return "", 0, false
}

func (m chatTUI) cacheTag() string {
	body, _, ok := m.cacheStatus()
	if !ok {
		return ""
	}
	return dim(body)
}

// jobsTag shows the count of running background jobs in the status line. Job
// start/finish emit Notices that arrive on eventCh and re-render the frame, so
// the count stays current without a dedicated tick.
func (m chatTUI) jobsTag() string {
	n := len(m.ctrl.Jobs())
	if n == 0 {
		return ""
	}
	return dim(fmt.Sprintf("⚙ %d", n))
}

func (m chatTUI) workModeTag() string {
	if m.runtimeProfile == "" {
		return ""
	}
	return dim(fmt.Sprintf(i18n.M.WorkModeStatusFmt, runtimeProfileDisplay(m.runtimeProfile)))
}

func (m chatTUI) effortTag() string {
	if m.effortLevel == "" {
		return ""
	}
	value := footerValue(m.effortLevel)
	if m.effortLevel != "auto" {
		value = themeStyle(activeCLITheme.info).Bold(true).Render(m.effortLevel)
	}
	return footerMetric(i18n.M.ChatStatusEffortLabel, value)
}

// mouseTag is a persistent status-line marker while mouseCaptureOff is on, so
// the loss of in-app scrollbar/wheel-scroll/drag-select reads as a deliberate
// state rather than a bug the user has to guess at.
func (m chatTUI) mouseTag() string {
	if !m.mouseCaptureOff {
		return ""
	}
	return dim(i18n.M.MouseCaptureTag)
}

// shortTokens prints token counts compactly: 1_500 → "1.5K", 142_000 → "142.0K", 1_000_000 → "1.0M".
func shortTokens(n int) string {
	switch {
	case n >= 999_950:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// turnPhaseStatusLabel maps host turn_phase values to a short status label.
// Empty when the phase is unknown so callers fall back to the default thinking line.
func turnPhaseStatusLabel(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case "working":
		return i18n.M.TurnPhaseWorking
	case "checking":
		return i18n.M.TurnPhaseChecking
	case "verifying":
		return i18n.M.TurnPhaseVerifying
	case "reviewing":
		return i18n.M.TurnPhaseReviewing
	default:
		return ""
	}
}

// formatCompletionSummaryLine renders a content-free quality summary for TUI scrollback.
func formatCompletionSummaryLine(c *event.CompletionSummaryInfo) string {
	if c == nil {
		return ""
	}
	preset := strings.TrimSpace(c.Preset)
	if preset == "" {
		preset = "balanced"
	}
	line := fmt.Sprintf("%s · %s · mut=%d · checks %d✓/%d✗/%d⊘",
		preset, c.Verdict, c.Mutations, c.ChecksPassed, c.ChecksFailed, c.ChecksSuppressed)
	if c.Review != "" && c.Review != "none" {
		line += " · review=" + c.Review
	}
	if len(c.GapKinds) > 0 {
		line += " · gaps=" + strings.Join(c.GapKinds, ",")
	}
	if c.ConstraintDegraded {
		line += " · constraints"
	}
	return line
}

func completionSummaryNeedsAttention(c *event.CompletionSummaryInfo) bool {
	if c == nil {
		return false
	}
	verdict := strings.ToLower(strings.TrimSpace(c.Verdict))
	review := strings.ToLower(strings.TrimSpace(c.Review))
	return verdict == "partial" || verdict == "blocked" ||
		c.ChecksFailed > 0 || c.ChecksSuppressed > 0 ||
		review == "warned" || review == "failed" || review == "unavailable" ||
		len(c.GapKinds) > 0 || c.ConstraintDegraded
}

func completionSummaryWarning(c *event.CompletionSummaryInfo) string {
	if c != nil && strings.EqualFold(strings.TrimSpace(c.Verdict), "blocked") {
		return i18n.M.CompletionSummaryBlocked
	}
	return i18n.M.CompletionSummaryNeedsAttention
}
