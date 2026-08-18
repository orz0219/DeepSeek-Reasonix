package control

import (
	"log/slog"

	"reasonix/internal/event"
	"reasonix/internal/memory"
	"reasonix/internal/provider"
)

// enqueueConsolidation submits an immutable snapshot to the background
// consolidation worker. It returns immediately — consolidation never blocks
// the session lifecycle.
func (c *Controller) enqueueConsolidation(reason memory.ConsolidationReason) {
	if c.consolidationWorker == nil {
		return
	}
	if c.executor == nil {
		return
	}

	sessionID := c.parentSessionID()
	if sessionID == "" {
		return
	}

	messages := c.History()
	if len(messages) == 0 {
		return
	}

	// Project: only user and assistant messages with content, no local-only.
	projected := projectMessagesForMemory(messages)
	if len(projected) == 0 {
		return
	}

	// Load existing memories for suppression detection.
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

	// Detect session-level suppression hints from user messages only.
	var userMsgs []string
	for _, m := range messages {
		if m.Role == provider.RoleUser && m.Content != "" {
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

	job := memory.ConsolidationJob{
		SessionID:        sessionID,
		Reason:           reason,
		Messages:         projected,
		ExistingMemories: existing,
		SuppressedKeys:   suppressedKeys,
	}

	if err := c.consolidationWorker.Enqueue(job); err != nil {
		slog.Warn("controller: enqueue consolidation failed", "session", sessionID, "err", err)
		c.sink.Emit(event.Event{
			Kind:  event.Notice,
			Level: event.LevelWarn,
			Text:  "Memory consolidation enqueue failed: " + err.Error(),
		})
	}
}

// projectMessagesForMemory filters session messages to only user and assistant
// content suitable for memory consolidation. System, tool, synthetic, and
// local-only messages are excluded.
func projectMessagesForMemory(messages []provider.Message) []string {
	projected := make([]string, 0, len(messages))
	for _, m := range messages {
		if m.LocalOnly {
			continue
		}
		if m.Role != provider.RoleUser && m.Role != provider.RoleAssistant {
			continue
		}
		if m.Content == "" {
			continue
		}
		projected = append(projected, m.Content)
	}
	return projected
}
