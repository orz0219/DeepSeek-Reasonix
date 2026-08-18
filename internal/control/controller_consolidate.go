package control

import (
	"context"
	"fmt"
	"log/slog"

	"reasonix/internal/event"
	"reasonix/internal/memory"
)

// consolidateSession runs automatic memory consolidation for the current
// session. It is a no-op when no consolidator is configured or the session
// has no content. Failures are logged and emitted as events but never
// propagate to the caller — consolidation is auxiliary, not part of the
// session's main success path.
func (c *Controller) consolidateSession(ctx context.Context) {
	if c.consolidator == nil {
		return
	}
	if c.executor == nil {
		return
	}
	// Singleflight: only one consolidation per logical session.
	c.consolidateMu.Lock()
	if c.consolidateDone {
		c.consolidateMu.Unlock()
		return
	}
	c.consolidateDone = true
	c.consolidateMu.Unlock()

	sessionID := c.parentSessionID()
	if sessionID == "" {
		return
	}

	messages := c.History()
	if len(messages) == 0 {
		return
	}

	// Project messages to text for the consolidator.
	projected := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.Content != "" {
			projected = append(projected, m.Content)
		}
	}
	if len(projected) == 0 {
		return
	}

	// Load existing memories for dedup context.
	var existing []memory.Memory
	if mem := c.Memory(); mem != nil {
		existing = mem.Store.ListAll()
	}

	// Load workspace-level suppressions.
	var suppressedKeys []string
	if mem := c.Memory(); mem != nil {
		for _, sub := range mem.Store.LoadSuppressions() {
			suppressedKeys = append(suppressedKeys, sub.SubjectKey)
		}
	}

	// Detect session-level suppression hints from user messages.
	var userMsgs []string
	for _, m := range messages {
		if m.Role == "user" && m.Content != "" {
			userMsgs = append(userMsgs, m.Content)
		}
	}
	sessionHints := memory.DetectSuppression(userMsgs)
	if len(sessionHints) > 0 && c.Memory() != nil {
		store := c.Memory().Store
		for _, hint := range sessionHints {
			_ = store.SaveSuppression(memory.Suppression{
				SubjectKey: hint,
				SessionID:  sessionID,
				Reason:     "user expression: " + hint,
			})
			suppressedKeys = append(suppressedKeys, hint)
		}
	}

	input := memory.ConsolidationInput{
		Workspace:        c.workspaceRoot,
		SessionID:        sessionID,
		SessionPath:      c.SessionPath(),
		Messages:         projected,
		ExistingMemories: existing,
		SuppressedKeys:   suppressedKeys,
	}

	result, err := c.consolidator.Consolidate(ctx, input)
	if err != nil {
		slog.Warn("controller: memory consolidation failed", "session", sessionID, "err", err)
		c.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  "Memory consolidation failed: " + err.Error(),
		})
		return
	}

	// Emit telemetry.
	c.sink.Emit(event.Event{
		Kind:  event.Notice,
		Level: event.LevelInfo,
		Text:  formatConsolidationResult(result),
	})
}

// resetConsolidation resets the singleflight guard for a new session.
func (c *Controller) resetConsolidation() {
	c.consolidateMu.Lock()
	c.consolidateDone = false
	c.consolidateMu.Unlock()
}

func formatConsolidationResult(r memory.ConsolidationResult) string {
	if r.Error != "" {
		return "Memory consolidation error: " + r.Error
	}
	if len(r.Added) == 0 && len(r.Updated) == 0 && len(r.Archived) == 0 {
		return "Memory consolidation: no new memories"
	}
	return "Memory consolidation: added=" + itoa(len(r.Added)) +
		" updated=" + itoa(len(r.Updated)) +
		" archived=" + itoa(len(r.Archived)) +
		" ignored=" + itoa(r.Ignored)
}

func itoa(n int) string {
	return fmt.Sprint(n)
}
