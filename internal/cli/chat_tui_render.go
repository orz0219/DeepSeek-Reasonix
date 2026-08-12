package cli

import (
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/i18n"
)

func (m *chatTUI) clearTranscriptDisplay() {
	if m.pendingCommit != nil {
		*m.pendingCommit = (*m.pendingCommit)[:0]
	}
	m.transcript = nil
	m.transcriptSources = nil
	m.clearWrapCache()
	m.viewport.SetContent("")
	m.shellOutputs = make(map[string]string)
	m.shellExpanded = make(map[string]bool)
	m.shellTranscriptIdx = make(map[string]int)
	m.toolLineCountByID = make(map[string]int)
	m.subagentProgressIdx = make(map[string]int)
	m.subagentProgress = make(map[string]*cliSubagentProgress)
	m.toolStreamID = ""
	m.toolStreamIdx = -1
	m.toolTail = nil
	m.toolPartial = ""
	m.toolLineCount = 0
}

// scrollChunkHeight is the largest block (in lines) finalize prints at once in
// native-scrollback mode, leaving room for the pinned bottom frame.
func (m chatTUI) scrollChunkHeight() int {
	if m.height <= 0 {
		return 100
	}
	if n := m.height - m.bottomRows(); n > 1 {
		return n
	}
	return 1
}

// chunkLines splits s into blocks of at most n lines each, preserving order and
// line content. A single block is returned when it already fits.
func chunkLines(s string, n int) []string {
	if n < 1 {
		n = 1
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return []string{s}
	}
	var out []string
	for i := 0; i < len(lines); i += n {
		end := min(i+n, len(lines))
		out = append(out, strings.Join(lines[i:end], "\n"))
	}
	return out
}

// clampWidth hard-breaks any line wider than width so no scrollback line wraps
// in the terminal. bubbletea's inline renderer estimates how far to scroll for
// each printed block from each line's width (insertAbove: offset += width/w); an
// over-wide line that the terminal wraps throws that estimate off and drifts the
// pinned input box off-screen. Lines already within width are left byte-for-byte
// untouched (chunkByWidth preserves content and ANSI), so rendered tables and the
// wrapped answer — which the markdown renderer already fit to width — are safe;
// only stray long lines (tool-dispatch args, unwrapped code) get broken.
func clampWidth(s string, width int) string {
	if width <= 0 {
		return s
	}

	return ansi.Hardwrap(s, width, false)
}

// commitLine queues one finalized block for the next scrollback flush.
func (m *chatTUI) commitLine(s string) {
	*m.pendingCommit = append(*m.pendingCommit, s)
	m.appendTranscriptBlock(s, transcriptSource{kind: transcriptSourceFixed})
}

// commitSpacer separates the next block (a thinking marker or a tool line) from
// the previous one with a single blank line, skipping it at the top of the
// transcript or when a blank already trails so spacers never double up.
func (m *chatTUI) commitSpacer() {
	if n := len(m.transcript); n > 0 && strings.TrimSpace(m.transcript[n-1]) != "" {
		m.commitLine("")
	}
}

// bottomRows is the terminal-row height of the pinned bottom region: any open
// bottom panels (todo / approval / chooser / rewind / completion), the composer
// when visible, and the two fixed status rows. Full-screen managers such as MCP
// and skills normally render inside the main transcript area; in native
// scrollback mode they join the bottom rail because there is no main viewport.
func (m chatTUI) bottomRows() int {
	rows := 0
	for _, s := range []string{
		m.renderTodoPanel(),
		m.renderApprovalBanner(),
		m.renderChooser(),
		m.renderRewind(),
		m.renderMCPImport(),
		m.renderResumePicker(),
		m.renderQuickPicker(),
		m.renderCopyPicker(),
		m.renderCompletion(),
	} {
		if s != "" {
			rows += strings.Count(s, "\n") + 1
		}
	}

	if m.nativeScrollback {
		if main := m.renderMainManager(); main != "" {
			rows += strings.Count(main, "\n") + 1
		}
	}
	if footer := m.renderMainManagerFooter(); footer != "" {
		rows += strings.Count(footer, "\n") + 1
	}
	if !m.hideComposer() {
		rows += m.input.Height() + 2
	}
	if m.statusLineCount > 0 {
		return rows + m.statusLineCount
	}
	return rows + 2
}

// hideComposer is the single ownership gate for the bottom composer.
//
// Rule for new CLI panels:
//   - If a panel is modal and keystrokes navigate/confirm/cancel the panel, hide
//     the composer so users do not see an inactive chat input.
//   - If a panel is input-owned (autocomplete, or chooser free-text mode), keep
//     the composer visible because the textarea is the active control.
//
// Whenever a new slash-command overlay or approval-style prompt is added, update
// this function and the modal layout tests together. Otherwise the panel may
// reserve rows for a composer that cannot receive input, leaving a confusing
// blank/bordered area at the bottom of the TUI.
func (m chatTUI) hideComposer() bool {
	if m.mcp != nil || m.clearConfirm != nil || m.mcpImport != nil || m.skillPick != nil || m.resumePick != nil || m.quickPick != nil || m.copyPick != nil || m.rewind != nil || m.pendingApproval != nil {
		return true
	}
	return m.chooser != nil && !m.chooser.typing
}

// transcriptHeight is the row budget left for the transcript viewport once the
// pinned bottom region is accounted for (at least one row).
func (m chatTUI) transcriptHeight() int {
	if h := m.height - m.bottomRows(); h > 1 {
		return h
	}
	return 1
}

func (m chatTUI) renderMainManager() string {
	if card := m.renderMCPManager(); card != "" {
		return card
	}
	if card := m.renderClearConfirm(); card != "" {
		return card
	}
	return m.renderSkillPicker()
}

func managerContentPanelStyle(width int) lipgloss.Style {
	return choicePanelStyle.
		Border(lipgloss.NormalBorder(), true, false, false, false).
		Width(width)
}

func managerFooterPanelStyle(width int) lipgloss.Style {
	return choicePanelStyle.
		Border(lipgloss.NormalBorder(), false, false, true, false).
		Width(width)
}

func (m chatTUI) renderMainManagerFooter() string {
	hint := ""
	switch {
	case m.mcp != nil:
		hint = m.mcp.footerHint()
	case m.clearConfirm != nil:
		hint = "Enter confirm · y clear · n/Esc cancel"
	case m.skillPick != nil:
		hint = m.skillPickerFooterHint()
	}
	if strings.TrimSpace(hint) == "" {
		return ""
	}
	w := max(viewWidth(m.width), 40)
	return managerFooterPanelStyle(w).Render(dim(hint))
}

func (m chatTUI) renderTranscriptWithMainManager(card string) string {
	h := m.viewport.Height()
	if h <= 0 {
		return ""
	}
	cw := m.viewport.Width()
	if cw <= 0 {
		cw = max(m.width-1, 1)
	}

	cardLines := strings.Split(strings.TrimRight(card, "\n"), "\n")
	if len(cardLines) > h {
		cardLines = cardLines[:h]
	}
	maxTranscriptRows := h - len(cardLines)
	if maxTranscriptRows > 0 && len(cardLines) > 0 && len(m.wrappedLines) > 0 {
		maxTranscriptRows--
	}

	var rows []string
	if maxTranscriptRows > 0 {
		lines := m.wrappedLines
		start := max(0, len(lines)-maxTranscriptRows)
		rows = append(rows, lines[start:]...)
	}
	if len(rows) > 0 && len(cardLines) > 0 {
		rows = append(rows, "")
	}
	rows = append(rows, cardLines...)
	for len(rows) < h {
		rows = append(rows, "")
	}
	for i, row := range rows {
		rows[i] = padRight(ansi.Cut(row, 0, cw), cw)
	}
	return strings.Join(rows, "\n")
}

// reasoningViewMax bounds the live thinking buffer the streamed block renders
// from. Re-rendering the full chain of thought on every delta was O(n²) (a 2k-
// token thought churned ~4.7GB); rendering only the trailing window keeps each
// delta O(1). The full text still lives in m.reasoning for verbose mode.
const reasoningViewMax = 4096

// reasoningTailLines caps how many trailing visual lines the live block shows.
const reasoningTailLines = 12

// streamReasoning appends a chunk and rewrites the live reasoning block from a
// bounded trailing view (mirrors streamToolOutput), so the chain of thought is
// visible while the model works without re-rendering the whole thing per token.
func (m *chatTUI) streamReasoning(chunk string) {
	m.reasoning.WriteString(chunk)
	if m.reasoningTextIdx < 0 {
		return
	}
	m.reasoningView = append(m.reasoningView, chunk...)
	if len(m.reasoningView) > reasoningViewMax {
		drop := len(m.reasoningView) - reasoningViewMax
		for drop < len(m.reasoningView) && !utf8.RuneStart(m.reasoningView[drop]) {
			drop++
		}
		m.reasoningView = m.reasoningView[:copy(m.reasoningView, m.reasoningView[drop:])]
	}
	raw := string(m.reasoningView)
	m.setTranscriptBlock(m.reasoningTextIdx, reasoningBlock(raw, m.width, reasoningTailLines), transcriptSource{
		kind: transcriptSourceReasoning, raw: raw, maxLines: reasoningTailLines,
	})
}

// reasoningBlock renders raw thinking text as dim, width-wrapped lines under a
// "⎿" connector that ties the block to the "▎ thinking…" marker above it. A
// positive maxLines keeps only the trailing visual lines (the live view); 0
// renders all (verbose collapse).
func reasoningBlock(raw string, width, maxLines int) string {
	w := max(width-len([]rune(connector)), 8)
	var lines []string
	for ln := range strings.SplitSeq(strings.TrimRight(raw, "\n"), "\n") {
		for wl := range strings.SplitSeq(ansi.Wrap(expandTabs(ln), w, ""), "\n") {
			lines = append(lines, dim(wl))
		}
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return connectorBlock(lines)
}

// toolStreamTailLines caps how many trailing output lines a running tool shows;
// the live block scrolls within this window so a chatty build doesn't flood.
const toolStreamTailLines = 8

// shellPreviewLines is how many lines of shell output to show by default after
// the command finishes. Ctrl+B toggles the full output.
const shellPreviewLines = 10

// shellExpandMaxLines caps how many lines Ctrl+B shows in expanded mode, so a
// very large output (e.g. thousands of lines) doesn't hang the TUI or push the
// input box off-screen.
const shellExpandMaxLines = 200

// streamToolOutput appends a chunk of a running tool's output and re-renders its
// live block (the last toolStreamTailLines lines) under the tool card, opening
// the block on the first chunk. Mirrors streamReasoning.
func (m *chatTUI) streamToolOutput(id, chunk string) {
	if id == "" {
		return
	}
	if m.toolStreamID != id {

		if existingIdx, ok := m.shellTranscriptIdx[id]; ok && existingIdx >= 0 && existingIdx < len(m.transcript) {

			if m.toolStreamID != "" && m.toolStreamID != id {
				n := m.toolLineCount
				if m.toolPartial != "" {
					n++
				}
				if n > 0 {
					m.toolLineCountByID[m.toolStreamID] = n
				}
			}
			m.toolStreamID = id
			m.toolStreamIdx = existingIdx
			m.toolTail = m.toolTail[:0]
			m.toolPartial = ""
			m.toolLineCount = 0
		} else {

			m.collapseToolOutput(m.toolStreamID, "")
			m.toolStreamID = id
			m.toolTail = m.toolTail[:0]
			m.toolPartial = ""
			m.toolLineCount = 0
			if m.nativeScrollback {
				m.toolStreamIdx = -1
			} else {
				m.toolStreamIdx = len(m.transcript)
				m.commitLine("")
			}
		}
	}

	if strings.HasPrefix(id, "shell-") {
		m.shellOutputs[id] += chunk
	}

	data := m.toolPartial + chunk
	for {
		i := strings.IndexByte(data, '\n')
		if i < 0 {
			break
		}
		m.pushToolLine(strings.TrimRight(data[:i], "\r"))
		data = data[i+1:]
	}
	m.toolPartial = data

	vis := m.toolTail
	if m.toolPartial != "" {
		vis = append(append([]string{}, m.toolTail...), m.toolPartial)
	}
	if m.nativeScrollback {
		return
	}
	lines := make([]string, len(vis))
	for i, ln := range vis {
		lines[i] = dim(clampPlain(ln, m.width-len([]rune(connector))))
	}
	m.rewriteTranscriptBlock(m.toolStreamIdx, connectorBlock(lines))
}

// pushToolLine appends a completed output line to the bounded tail, dropping the
// oldest when it exceeds the window (the backing array stays ≤ window+1).
func (m *chatTUI) pushToolLine(line string) {
	m.toolLineCount++
	m.toolTail = append(m.toolTail, line)
	if len(m.toolTail) > toolStreamTailLines {
		copy(m.toolTail, m.toolTail[1:])
		m.toolTail = m.toolTail[:toolStreamTailLines]
	}
}

// cliSubagentProgress is the per-child live state backing one fixed transcript
// slot. Everything here is in-memory only: the persisted sub-agent transcript
// remains the source of truth after a restart.

// bounded ≤ subagentPreviewMax, UTF-8-safe tail

// native-scrollback dedupe

// cliPreviewTail keeps the most recent maxBytes of s at a rune boundary.

// streamSubagentProgress routes reserved ToolProgress channels into per-child
// progress state instead of the single live tool stream: each child keeps its
// own phase, elapsed, recent activity, and (verbose-only) preview tails.

// renderSubagentProgress redraws a child's progress block. Alt-screen TUIs
// rewrite the fixed transcript slot in place (created on first sight under the
// current transcript end); native-scrollback terminals print a status line
// only on phase changes and terminal, since printed output cannot be rewritten.

// tickSubagentProgress refreshes the elapsed / recent-activity fields of live
// progress blocks once a second (mirrors tickToolRunning), so a child that
// produces no events still reads as alive.

// subagentProgressBlock renders one child's progress block. The default line
// shows phase, running elapsed, and recent activity; verbose mode adds the
// bounded reasoning/text/notice tails above it. Terminal children collapse to
// a one-line summary (the preview survives in memory for verbose re-render).

// subagentPreviewBlock renders a bounded trailing window of a preview channel
// as dim, width-wrapped lines carrying a small glyph marker.

// subagentPhaseLabel maps a reserved status value to its localized label.

// printSubagentProgressScrollback queues a status line for native-scrollback
// terminals, which cannot rewrite printed output (finalized blocks drain via
// pendingCommit like every other scrollback commit): status lines appear on
// phase changes and terminal only, and verbose previews are throttled to at
// most one print per 2s per child.

// collapseToolOutput replaces a finished tool's live block with a dim
// "⎿ N lines" summary, so the scrollback keeps a marker of the run without the
// full output (which the model already received). For shell commands ("shell-"
// prefix), it shows the first shellPreviewLines with a Ctrl+B hint instead.
// No-op when id isn't streaming. resultOutput (the ToolResult's final output)
// is the last-resort line-count source when the live state was already reset.
func (m *chatTUI) collapseToolOutput(id, resultOutput string) {
	if m.nativeScrollback {
		if id == "" || m.toolStreamID != id {
			return
		}
		n := m.toolLineCount
		if m.toolPartial != "" {
			n++
		}
		if n > 0 {
			if full, ok := m.shellOutputs[id]; ok {
				lines := strings.Split(strings.TrimRight(full, "\n"), "\n")
				total := len(lines)
				if total > shellPreviewLines {
					preview := make([]string, shellPreviewLines+1)
					for i := range shellPreviewLines {
						preview[i] = dim(clampPlain(lines[i], m.width-len([]rune(connector))))
					}
					preview[shellPreviewLines] = dim(fmt.Sprintf("… %d more lines (Ctrl+B)", total-shellPreviewLines))
					m.commitLine(connectorBlock(preview))
				} else {
					rendered := make([]string, total)
					for i, ln := range lines {
						rendered[i] = dim(clampPlain(ln, m.width-len([]rune(connector))))
					}
					m.commitLine(connectorBlock(rendered))
				}
				m.shellTranscriptIdx[id] = len(m.transcript) - 1
			} else {
				m.commitLine(connectorBlock([]string{dim(fmt.Sprintf("%d lines", n))}))
			}
		}
		m.toolStreamIdx = -1
		m.toolStreamID = ""
		m.toolTail = m.toolTail[:0]
		m.toolPartial = ""
		m.toolLineCount = 0
		return
	}
	if m.toolStreamIdx < 0 || id == "" || m.toolStreamID != id {

		if idx, ok := m.shellTranscriptIdx[id]; ok && idx >= 0 && idx < len(m.transcript) {
			m.collapseShellSlot(id, idx, resultOutput)
		}
		return
	}
	m.collapseShellSlot(id, m.toolStreamIdx, resultOutput)
	m.toolStreamIdx = -1
	m.toolStreamID = ""
	m.toolTail = m.toolTail[:0]
	m.toolPartial = ""
	m.toolLineCount = 0
}

// collapseShellSlot finalises a tool's live block at idx. Used both by the
// active-tool path (idx == toolStreamIdx, streaming state intact) and the
// late-result path (idx recorded in shellTranscriptIdx at dispatch). Line-count
// sources, in order: live streaming state, shellOutputs ("shell-" ids only),
// the per-id count stashed by streamToolOutput, then the ToolResult's output.
func (m *chatTUI) collapseShellSlot(id string, idx int, resultOutput string) {
	m.transcriptDirty = true
	n := -1
	if id == m.toolStreamID {

		n = m.toolLineCount
		if m.toolPartial != "" {
			n++
		}
		if resultOutput != "" {
			fromResult := len(strings.Split(strings.TrimRight(resultOutput, "\n"), "\n"))
			if fromResult > n {
				n = fromResult
			}
		}
	}
	if n < 0 {
		if full, ok := m.shellOutputs[id]; ok {
			n = len(strings.Split(strings.TrimRight(full, "\n"), "\n"))
		} else if c, ok := m.toolLineCountByID[id]; ok {
			n = c
		} else if resultOutput != "" {
			n = len(strings.Split(strings.TrimRight(resultOutput, "\n"), "\n"))
		}
	}
	if n < 0 {

		n = 0
	}
	if n == 0 {

		m.rewriteTranscriptBlock(idx, "")
		return
	}
	if full, ok := m.shellOutputs[id]; ok {

		lines := strings.Split(strings.TrimRight(full, "\n"), "\n")
		total := len(lines)
		if total > shellPreviewLines {
			preview := make([]string, shellPreviewLines+1)
			for i := range shellPreviewLines {
				preview[i] = dim(clampPlain(lines[i], m.width-len([]rune(connector))))
			}
			preview[shellPreviewLines] = dim(fmt.Sprintf("… %d more lines (Ctrl+B)", total-shellPreviewLines))
			m.rewriteTranscriptBlock(idx, connectorBlock(preview))
		} else {
			rendered := make([]string, total)
			for i, ln := range lines {
				rendered[i] = dim(clampPlain(ln, m.width-len([]rune(connector))))
			}
			m.rewriteTranscriptBlock(idx, connectorBlock(rendered))
		}
	} else {
		m.rewriteTranscriptBlock(idx, connectorBlock([]string{dim(fmt.Sprintf("%d lines", n))}))
	}
	m.shellTranscriptIdx[id] = idx
}

// toggleShellOutput expands or collapses the output of the most recent shell
// command. When expanded, up to shellExpandMaxLines lines are shown; when
// collapsed, only the first shellPreviewLines are shown. Called on Ctrl+B.
func (m *chatTUI) toggleShellOutput() {
	// Find the most recent shell output that has a transcript entry.
	var lastID string
	lastIdx := -1
	for id, idx := range m.shellTranscriptIdx {
		if idx >= 0 && idx < len(m.transcript) && idx > lastIdx {
			lastID = id
			lastIdx = idx
		}
	}
	if lastID == "" {
		return
	}
	full, ok := m.shellOutputs[lastID]
	if !ok {
		return
	}
	lines := strings.Split(strings.TrimRight(full, "\n"), "\n")
	total := len(lines)
	innerW := m.width - len([]rune(connector))
	if innerW < 10 {
		innerW = 80
	}

	if m.shellExpanded[lastID] {

		m.shellExpanded[lastID] = false
		if total > shellPreviewLines {
			preview := make([]string, shellPreviewLines+1)
			for i := range shellPreviewLines {
				preview[i] = dim(clampPlain(lines[i], innerW))
			}
			preview[shellPreviewLines] = dim(fmt.Sprintf("… %d more lines (Ctrl+B)", total-shellPreviewLines))
			m.rewriteTranscriptBlock(lastIdx, connectorBlock(preview))
		}
	} else {

		m.shellExpanded[lastID] = true
		show := min(total, shellExpandMaxLines)
		rendered := make([]string, show)
		for i := range show {
			rendered[i] = dim(clampPlain(lines[i], innerW))
		}
		if total > shellExpandMaxLines {
			rendered = append(rendered, dim(fmt.Sprintf("… %d more lines", total-shellExpandMaxLines)))
		}
		m.rewriteTranscriptBlock(lastIdx, connectorBlock(rendered))
	}
	if m.nativeScrollback {
		m.commitLine(m.transcript[lastIdx])
	}
}

// toolWorkingFrames is the braille spinner cycled once per second on the
// "⎿ working · Ns" line of a tool that hasn't streamed output yet.
var toolWorkingFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// beginToolRunning opens an empty live block under a just-dispatched tool card,
// keyed by the call id. tickToolRunning fills it with a "working · Ns" line each
// second; if the tool later streams output, streamToolOutput reuses the same
// block; collapseToolOutput closes it on the result.
func (m *chatTUI) beginToolRunning(id string) {
	if id == "" {
		return
	}
	m.toolStreamID = id
	m.toolTail = m.toolTail[:0]
	m.toolPartial = ""
	m.toolLineCount = 0

	delete(m.shellOutputs, id)
	m.toolStreamStart = time.Now()
	m.toolStreamFrame = 0
	if m.nativeScrollback {
		m.toolStreamIdx = -1
		return
	}
	m.toolStreamIdx = len(m.transcript)
	m.commitLine(connectorBlock([]string{dim(fmt.Sprintf(i18n.M.ChatToolWorkingFmt, toolWorkingFrames[0], 0))}))

	m.shellTranscriptIdx[id] = m.toolStreamIdx
}

// tickToolRunning re-renders the working line of a tool that's dispatched but
// hasn't produced output yet. A no-op once output streams in or no tool runs.
func (m *chatTUI) tickToolRunning() {
	if m.nativeScrollback {
		return
	}
	if m.toolStreamIdx < 0 || m.toolLineCount != 0 || m.toolPartial != "" {
		return
	}
	m.toolStreamFrame++
	frame := toolWorkingFrames[m.toolStreamFrame%len(toolWorkingFrames)]
	secs := int(time.Since(m.toolStreamStart).Seconds())
	m.rewriteTranscriptBlock(m.toolStreamIdx, connectorBlock([]string{dim(fmt.Sprintf(i18n.M.ChatToolWorkingFmt, frame, secs))}))
}

// commitReasoning closes the live thinking block: the "▎ thinking…" marker is
// rewritten to a dim "▎ thought for Ns" summary and the streamed text below it is
// removed (collapsed) — kept only in verbose mode. The viewport re-wraps from
// m.transcript, so the change is flagged via transcriptDirty.
func (m *chatTUI) commitReasoning() {
	if m.reasoningNative {
		if strings.TrimSpace(m.reasoning.String()) != "" || !m.thinkStart.IsZero() {
			secs := int(time.Since(m.thinkStart).Seconds())
			m.commitSpacer()
			m.commitLine(dim(fmt.Sprintf("  ▎ "+i18n.M.ChatThoughtForFmt, secs)))
			if m.showReasoning && strings.TrimSpace(m.reasoning.String()) != "" {
				m.commitLine(reasoningBlock(m.reasoning.String(), m.width, 0))
			}
		}
		m.reasoning.Reset()
		m.reasoningView = m.reasoningView[:0]
		m.reasoningNative = false
		m.thinkStart = time.Time{}
		return
	}
	if m.reasoningLineIdx < 0 {
		return
	}
	secs := int(time.Since(m.thinkStart).Seconds())
	m.setTranscriptBlock(m.reasoningLineIdx, dim(fmt.Sprintf("  ▎ "+i18n.M.ChatThoughtForFmt, secs)), transcriptSource{kind: transcriptSourceFixed})
	if m.reasoningTextIdx >= 0 {
		if m.showReasoning && strings.TrimSpace(m.reasoning.String()) != "" {
			raw := m.reasoning.String()
			m.setTranscriptBlock(m.reasoningTextIdx, reasoningBlock(raw, m.width, 0), transcriptSource{
				kind: transcriptSourceReasoning, raw: raw,
			})
		} else {
			m.removeTranscriptBlock(m.reasoningTextIdx)
		}
	}
	m.transcriptDirty = true
	m.reasoning.Reset()
	m.reasoningView = m.reasoningView[:0]
	m.reasoningLineIdx = -1
	m.reasoningTextIdx = -1
}

// subagentPreviewMax bounds each child's retained reasoning/text preview tail
// (verbose mode renders from it); the notice tail is smaller.
const (
	subagentPreviewMax = 4096
	subagentNoticeMax  = 2048
	// subagentPreviewTailLines caps the trailing visual lines of a preview.
	subagentPreviewTailLines = 12
)

var (
	// Input box: only top + bottom borders, no sides. The concrete colors are
	// refreshed from the active CLI theme during startup.
	inputBoxStyle    lipgloss.Style
	todoPanelStyle   lipgloss.Style
	statusBlockStyle lipgloss.Style
	workingStyle     lipgloss.Style
)

func interruptedTurnDisplayNotice() string {
	return i18n.M.InterruptedRecovery
}

// renderTUIBanner is the title + tip + optional missing-key warning printed once
// at the top of the session.
func renderTUIBanner(label, missing string, width int) string {
	var b strings.Builder
	b.WriteString(accent("◆") + " " + bold("reasonix") + "  " + dim("· "+label) + "\n")
	b.WriteString(dim("  "+i18n.M.ChatTip) + "\n")
	if missing != "" {
		b.WriteString(wrapForViewport("  ! "+missing, width, activeCLITheme.warn) + "\n")
	}
	return b.String()
}

// wrapForViewport hard-wraps text to fit width columns and colours every line.
func wrapForViewport(text string, width int, fg cliColor) string {
	if width <= 0 {
		width = 80
	}
	return themeStyle(fg).Width(width).Render(text)
}

// renderUserBubble renders the just-submitted prompt as a transcript line. Keep
// it visually lighter than the real bottom composer so a fresh session does not
// look like it has a second input box in the transcript.
func renderUserBubble(line string, width int, planMode bool) string {
	line = displayLineForImageRefs(line)
	prefix := "› "
	if planMode {
		prefix = "› [plan] "
	}
	if !colorOn() {
		return "│ " + prefix + line
	}
	return "  " + accent(prefix+line)
}

func displayLineForImageRefs(line string) string {
	idx := 0
	out := cliImageRefRe.ReplaceAllStringFunc(line, func(_ string) string {
		idx++
		return " [image" + strconv.Itoa(idx) + "]"
	})
	return strings.TrimSpace(out)
}
