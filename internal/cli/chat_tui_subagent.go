package cli

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"reasonix/internal/event"
	"reasonix/internal/i18n"
)

// cliSubagentProgress is the per-child live state backing one fixed transcript
// slot. Everything here is in-memory only: the persisted sub-agent transcript
// remains the source of truth after a restart.
type cliSubagentProgress struct {
	phase            string
	startedAt        time.Time
	lastActive       time.Time
	reasoning        string // bounded ≤ subagentPreviewMax, UTF-8-safe tail
	text             string
	notice           string
	truncated        bool
	durationMs       int64
	terminal         bool
	lastPrintedPhase string // native-scrollback dedupe
	verboseLastPrint time.Time
}

func subagentPhaseTerminal(phase string) bool {
	switch phase {
	case "completed", "failed", "cancelled":
		return true
	}
	return false
}

// cliPreviewTail keeps the most recent maxBytes of s at a rune boundary.
func cliPreviewTail(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[len(s)-maxBytes:]
	for len(s) > 0 && !utf8.RuneStart(s[0]) {
		s = s[1:]
	}
	return s
}

// streamSubagentProgress routes reserved ToolProgress channels into per-child
// progress state instead of the single live tool stream: each child keeps its
// own phase, elapsed, recent activity, and (verbose-only) preview tails.
func (m *chatTUI) streamSubagentProgress(t event.Tool) {
	if t.ID == "" {
		return
	}
	sp := m.subagentProgress[t.ID]
	if sp == nil {
		sp = &cliSubagentProgress{startedAt: time.Now()}
		m.subagentProgress[t.ID] = sp
	}
	sp.lastActive = time.Now()
	switch t.Name {
	case event.SubagentProgressStatusName:
		sp.phase = t.Output
		if t.DurationMs > 0 {
			sp.durationMs = t.DurationMs
		}
		sp.terminal = subagentPhaseTerminal(t.Output)
		m.renderSubagentProgress(t.ID)
	case event.SubagentProgressReasoningName:
		sp.reasoning = cliPreviewTail(sp.reasoning+t.Output, subagentPreviewMax)
		sp.truncated = sp.truncated || t.Truncated
		if m.showReasoning {
			m.renderSubagentProgress(t.ID)
		}
	case event.SubagentProgressTextName:
		sp.text = cliPreviewTail(sp.text+t.Output, subagentPreviewMax)
		sp.truncated = sp.truncated || t.Truncated
		if m.showReasoning {
			m.renderSubagentProgress(t.ID)
		}
	case event.SubagentProgressNoticeName:
		sp.notice = cliPreviewTail(sp.notice+t.Output, subagentNoticeMax)
		sp.truncated = sp.truncated || t.Truncated
		if m.showReasoning {
			m.renderSubagentProgress(t.ID)
		}
	}
}

// renderSubagentProgress redraws a child's progress block. Alt-screen TUIs
// rewrite the fixed transcript slot in place (created on first sight under the
// current transcript end); native-scrollback terminals print a status line
// only on phase changes and terminal, since printed output cannot be rewritten.
func (m *chatTUI) renderSubagentProgress(id string) {
	sp := m.subagentProgress[id]
	if sp == nil || sp.phase == "" {
		return
	}
	if m.nativeScrollback {
		m.printSubagentProgressScrollback(id, sp)
		return
	}
	idx, ok := m.subagentProgressIdx[id]
	if !ok {
		idx = len(m.transcript)
		m.subagentProgressIdx[id] = idx
		m.commitLine(m.subagentProgressBlock(id, sp))
		return
	}
	m.setTranscriptBlock(idx, m.subagentProgressBlock(id, sp), transcriptSource{kind: transcriptSourceSubagentProgress, raw: id})
}

// tickSubagentProgress refreshes the elapsed / recent-activity fields of live
// progress blocks once a second (mirrors tickToolRunning), so a child that
// produces no events still reads as alive.
func (m *chatTUI) tickSubagentProgress() {
	if m.nativeScrollback {
		return
	}
	for id, sp := range m.subagentProgress {
		if sp.terminal || sp.phase == "" {
			continue
		}
		idx, ok := m.subagentProgressIdx[id]
		if !ok {
			continue
		}
		m.setTranscriptBlock(idx, m.subagentProgressBlock(id, sp), transcriptSource{kind: transcriptSourceSubagentProgress, raw: id})
	}
}

// subagentProgressBlock renders one child's progress block. The default line
// shows phase, running elapsed, and recent activity; verbose mode adds the
// bounded reasoning/text/notice tails above it. Terminal children collapse to
// a one-line summary (the preview survives in memory for verbose re-render).
func (m *chatTUI) subagentProgressBlock(id string, sp *cliSubagentProgress) string {
	var lines []string
	if m.showReasoning {
		if sp.reasoning != "" {
			lines = append(lines, subagentPreviewBlock(i18n.M.ChatSubagentPreviewLabel, sp.reasoning, m.width, subagentPreviewTailLines))
		}
		if sp.text != "" {
			lines = append(lines, subagentPreviewBlock("✎", sp.text, m.width, subagentPreviewTailLines))
		}
		if sp.notice != "" {
			lines = append(lines, subagentPreviewBlock("!", sp.notice, m.width, subagentPreviewTailLines))
		}
		if sp.truncated {
			lines = append(lines, dim("… preview truncated"))
		}
	}
	label := subagentPhaseLabel(sp.phase)
	switch sp.phase {
	case "completed":
		label = green(label + " ✓")
	case "failed":
		label = red(label + " ✗")
	case "cancelled":
		label = dim(label + " ⊘")
	case "retrying":
		label = yellow(label)
	}
	if sp.terminal {
		secs := sp.durationMs / 1000
		if secs <= 0 && !sp.startedAt.IsZero() {
			secs = int64(time.Since(sp.startedAt).Seconds())
		}
		lines = append(lines, fmt.Sprintf(i18n.M.ChatSubagentProgressDoneFmt, label, secs))
	} else {
		elapsed := int64(0)
		idle := int64(0)
		if !sp.startedAt.IsZero() {
			elapsed = int64(time.Since(sp.startedAt).Seconds())
		}
		if !sp.lastActive.IsZero() {
			idle = int64(time.Since(sp.lastActive).Seconds())
		}
		lines = append(lines, fmt.Sprintf(i18n.M.ChatSubagentProgressFmt, label, elapsed, idle))
	}
	return connectorBlock(lines)
}

// subagentPreviewBlock renders a bounded trailing window of a preview channel
// as dim, width-wrapped lines carrying a small glyph marker.
func subagentPreviewBlock(glyph, raw string, width, maxLines int) string {
	w := max(width-len([]rune(connector)), 8)
	var lines []string
	first := true
	for ln := range strings.SplitSeq(strings.TrimRight(raw, "\n"), "\n") {
		if first {
			ln = glyph + " " + ln
			first = false
		}
		for wl := range strings.SplitSeq(ansi.Wrap(expandTabs(ln), w, ""), "\n") {
			lines = append(lines, dim(wl))
		}
	}
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	return strings.Join(lines, "\n")
}

// subagentPhaseLabel maps a reserved status value to its localized label.
func subagentPhaseLabel(phase string) string {
	switch phase {
	case "queued":
		return i18n.M.ChatSubagentPhaseQueued
	case "running":
		return i18n.M.ChatSubagentPhaseRunning
	case "reasoning":
		return i18n.M.ChatSubagentPhaseReasoning
	case "responding":
		return i18n.M.ChatSubagentPhaseResponding
	case "tool":
		return i18n.M.ChatSubagentPhaseTool
	case "retrying":
		return i18n.M.ChatSubagentPhaseRetrying
	case "completed":
		return i18n.M.ChatSubagentPhaseCompleted
	case "failed":
		return i18n.M.ChatSubagentPhaseFailed
	case "cancelled":
		return i18n.M.ChatSubagentPhaseCancelled
	}
	return phase
}

// printSubagentProgressScrollback queues a status line for native-scrollback
// terminals, which cannot rewrite printed output (finalized blocks drain via
// pendingCommit like every other scrollback commit): status lines appear on
// phase changes and terminal only, and verbose previews are throttled to at
// most one print per 2s per child.
func (m *chatTUI) printSubagentProgressScrollback(id string, sp *cliSubagentProgress) {
	if m.pendingCommit == nil {
		return
	}
	block := m.subagentProgressBlock(id, sp)
	if sp.terminal {
		*m.pendingCommit = append(*m.pendingCommit, block)
		sp.lastPrintedPhase = sp.phase
		sp.verboseLastPrint = time.Now()
		return
	}
	if sp.phase != sp.lastPrintedPhase {
		*m.pendingCommit = append(*m.pendingCommit, block)
		sp.lastPrintedPhase = sp.phase
		sp.verboseLastPrint = time.Now()
		return
	}
	if m.showReasoning && time.Since(sp.verboseLastPrint) >= 2*time.Second && (sp.reasoning != "" || sp.text != "" || sp.notice != "") {
		*m.pendingCommit = append(*m.pendingCommit, block)
		sp.verboseLastPrint = time.Now()
	}
}
