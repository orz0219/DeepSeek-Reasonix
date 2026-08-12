package main

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"reasonix/internal/agent"
	"reasonix/internal/provider"
)

func historyWindowContainsTodoWrite(msgs []provider.Message) bool {
	for _, msg := range msgs {
		for _, call := range msg.ToolCalls {
			if call.Name == "todo_write" {
				return true
			}
		}
	}
	return false
}

// countRoleBefore counts messages with role in [0, lo).
func countRoleBefore(roles []provider.Role, lo int, role provider.Role) int {
	if lo > len(roles) {
		lo = len(roles)
	}
	count := 0
	for i := range lo {
		if roles[i] == role {
			count++
		}
	}
	return count
}

// extendHistoryToolResults fills in tool results for window tool calls whose
// result message lies past the window's newer edge (an intra-turn page cut),
// so tool-call summaries match the full-conversion output. It keeps no result
// body except one whose call is actually visible, but deliberately scans past
// arbitrary non-tool traffic: correctness cannot depend on a result arriving
// within a guessed distance.
func extendHistoryToolResults(src *historySliceSource, window []provider.Message, hi int, toolResults map[string]provider.Message) error {
	var want map[string]bool
	for _, m := range window {
		for _, tc := range m.ToolCalls {
			if tc.ID == "" {
				continue
			}
			if _, ok := toolResults[tc.ID]; ok {
				continue
			}
			if want == nil {
				want = map[string]bool{}
			}
			want[tc.ID] = true
		}
	}
	if len(want) == 0 {
		return nil
	}
	for i := hi; i < src.total && len(want) > 0; i++ {
		if src.roles[i] != provider.RoleTool {
			continue
		}
		msgs, err := src.fetch(i, i+1)
		if err != nil {
			return err
		}
		if len(msgs) != 1 {
			return fmt.Errorf("tool result window length %d, want 1", len(msgs))
		}
		m := msgs[0]
		if m.ToolCallID != "" && want[m.ToolCallID] {
			toolResults[m.ToolCallID] = m
			delete(want, m.ToolCallID)
		}
	}
	return nil
}

// forEachHistorySourceChunk decodes a bounded contiguous message window at a
// time. It is the common primitive for the cross-page lookups below; callers
// retain only their derived state, never the full transcript.
func forEachHistorySourceChunk(src *historySliceSource, end int, visit func([]provider.Message) error) error {
	if end > src.total {
		end = src.total
	}
	for lo := 0; lo < end; lo += historyLookupChunkMessages {
		hi := min(lo+historyLookupChunkMessages, end)
		msgs, err := src.fetch(lo, hi)
		if err != nil {
			return err
		}
		if len(msgs) != hi-lo {
			return fmt.Errorf("history lookup window length %d, want %d", len(msgs), hi-lo)
		}
		if err := visit(msgs); err != nil {
			return err
		}
	}
	return nil
}

// historyTodoArgsForSource derives completed todo state in two bounded passes:
// first discover successful calls anywhere in the transcript, then replay the
// todo stream. The legacy converter does the same work over an in-memory
// slice; doing it here prevents a page cut from displaying stale todo items.
func historyTodoArgsForSource(src *historySliceSource) (map[string]string, error) {
	successful := map[string]bool{}
	if err := forEachHistorySourceChunk(src, src.total, func(msgs []provider.Message) error {
		for _, msg := range msgs {
			if msg.Role == provider.RoleTool && msg.ToolCallID != "" && !historyToolResultFailed(msg.Content) {
				successful[msg.ToolCallID] = true
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	state := newHistoryTodoArgsState(successful)
	if err := forEachHistorySourceChunk(src, src.total, func(msgs []provider.Message) error {
		for _, msg := range msgs {
			state.consume(msg)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return state.out, nil
}

// primeHistoryPlannerState consumes the non-rendered prefix so planner
// displays remain FIFO per duplicated user text and an interrupt's canonical
// suppression crosses page boundaries exactly as in a full conversion.
func primeHistoryPlannerState(src *historySliceSource, state *historyMessageConvertState, end int, resolver func(string) string) error {
	if end <= 0 || len(state.plannerByUserHash) == 0 {
		return nil
	}
	return forEachHistorySourceChunk(src, end, func(msgs []provider.Message) error {
		for _, msg := range msgs {
			state.consumeHistoryPlannerState(msg, resolver)
		}
		return nil
	})
}

// newHistoryEntry builds one entry, replacing oversized string fields with
// preview + ref. entryID is the fully-built entry ID (message- or row-form).
func newHistoryEntry(src *historySliceSource, entryID string, msgIndex, sub int, row HistoryMessage) HistoryEntry {
	entry := HistoryEntry{
		EntryID: entryID,
		Turn:    src.turns[msgIndex],
		Order:   msgIndex,
		Message: row,
		Refs:    []HistoryContentRef{},
	}
	addRef := func(field, toolCallID string, size, chunks int) {
		entry.Refs = append(entry.Refs, HistoryContentRef{
			EntryID:    entryID,
			Field:      field,
			Size:       size,
			Chunks:     chunks,
			ToolCallID: toolCallID,
			Revision:   src.revision,
			RevKnown:   src.revKnown,
			Digest:     src.digest,
		})
	}
	m := &entry.Message
	m.Content = truncateHistoryField(m.Content, "content", "", addRef)
	m.Reasoning = truncateHistoryField(m.Reasoning, "reasoning", "", addRef)
	m.SubmitText = truncateHistoryField(m.SubmitText, "submitText", "", addRef)
	m.Detail = truncateHistoryField(m.Detail, "detail", "", addRef)
	m.Code = truncateHistoryField(m.Code, "code", "", addRef)
	m.Summary = truncateHistoryField(m.Summary, "summary", "", addRef)
	m.Archive = truncateHistoryField(m.Archive, "archive", "", addRef)
	m.ToolResultError = truncateHistoryField(m.ToolResultError, "toolResultError", "", addRef)
	for i := range m.ToolCalls {
		tc := &m.ToolCalls[i]
		tc.Arguments = truncateHistoryField(tc.Arguments, "toolArguments", tc.ID, addRef)
		tc.Subject = truncateHistoryField(tc.Subject, "toolSubject", tc.ID, addRef)
		tc.Summary = truncateHistoryField(tc.Summary, "toolSummary", tc.ID, addRef)
		tc.Diff = truncateHistoryField(tc.Diff, "toolDiff", tc.ID, addRef)
	}
	return entry
}

// truncateHistoryField replaces value with a rune-safe preview and registers
// a content ref when it exceeds the inline threshold.
func truncateHistoryField(value, field, toolCallID string, addRef func(field, toolCallID string, size, chunks int)) string {
	if len(value) <= historyInlineRefThreshold {
		return value
	}
	addRef(field, toolCallID, len(value), historyContentChunkCount(value))
	return clipStringBytes(value, historyFieldPreviewBytes)
}

// inlineBytes approximates the JSON payload contributed by the entry's inline
// string fields (post-truncation), for the byte budget.
func (e HistoryEntry) inlineBytes() int {
	m := e.Message
	n := len(m.Content) + len(m.Detail) + len(m.Code) + len(m.SubmitText) +
		len(m.Reasoning) + len(m.Summary) + len(m.Archive) + len(m.ToolResultError) +
		len(m.ToolCallID) + len(m.ToolName) + len(m.Role)
	for _, tc := range m.ToolCalls {
		n += len(tc.Arguments) + len(tc.Subject) + len(tc.Summary) + len(tc.Diff) + len(tc.ID) + len(tc.Name)
	}
	return n
}

// historyWindowWithPersistedTimes is the window-scoped form of
// historyProviderMessagesWithPersistedTimes: userOffset is the number of
// user-role messages before the window, keeping the ordinal alignment with
// the persisted user-message records.
func historyWindowWithPersistedTimes(msgs []provider.Message, sessionPath string, userOffset int) []provider.Message {
	if len(msgs) == 0 || strings.TrimSpace(sessionPath) == "" {
		return msgs
	}
	needsPersistedTime := false
	for _, msg := range msgs {
		if msg.Role == provider.RoleUser && msg.CreatedAt <= 0 && agent.IsUserAuthoredTurn(agent.UserMessageText(msg)) {
			needsPersistedTime = true
			break
		}
	}
	if !needsPersistedTime {
		return msgs
	}
	users, err := agent.LoadSessionUserMessages(sessionPath)
	if err != nil || len(users) <= userOffset {
		return msgs
	}
	out := append([]provider.Message(nil), msgs...)
	userIndex := userOffset
	for i := range out {
		if out[i].Role != provider.RoleUser {
			continue
		}
		if userIndex >= len(users) {
			break
		}
		user := users[userIndex]
		userIndex++
		if out[i].CreatedAt <= 0 && !user.At.IsZero() {
			out[i].CreatedAt = user.At.UnixMilli()
		}
	}
	return out
}

// historyContentChunkCount returns the number of rune-aligned ≤256KiB chunks
// for s. The empty string is one empty chunk.
func historyContentChunkCount(s string) int {
	if len(s) == 0 {
		return 1
	}
	chunks := 0
	for off := 0; off < len(s); chunks++ {
		off = historyContentChunkEnd(s, off)
	}
	return chunks
}

// historyContentChunkEnd returns the end offset of the chunk starting at off:
// off+256KiB backed off to a rune boundary.
func historyContentChunkEnd(s string, off int) int {
	end := off + historyContentChunkBytes
	if end >= len(s) {
		return len(s)
	}
	for end > off && !utf8.RuneStart(s[end]) {
		end--
	}
	return end
}

// historyContentChunkAt returns chunk index (0-based) of s and the total
// chunk count, splitting on rune boundaries.
func historyContentChunkAt(s string, index int) (string, int) {
	chunks := historyContentChunkCount(s)
	if index < 0 {
		index = 0
	}
	off := 0
	for i := 0; i < index && off < len(s); i++ {
		off = historyContentChunkEnd(s, off)
	}
	if off >= len(s) {
		return "", chunks
	}
	return s[off:historyContentChunkEnd(s, off)], chunks
}
