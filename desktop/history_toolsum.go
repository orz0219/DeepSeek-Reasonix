package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"reasonix/internal/evidence"
	"reasonix/internal/provider"
)

const historyToolPreviewLimit = 2_000

func historyToolCall(tc provider.ToolCall, args string, result provider.Message) HistoryToolCall {
	call := HistoryToolCall{
		ID:               tc.ID,
		Name:             tc.Name,
		ResolvedName:     tc.ResolvedName,
		CapabilityID:     tc.CapabilityID,
		ResolvedReadOnly: tc.ResolvedReadOnly,
		Subject:          historyToolSubject(tc.Name, args),
		Summary:          historyToolSummary(tc.Name, args, result.Content),
		Diff:             tc.Diff,
		Added:            tc.Added,
		Removed:          tc.Removed,
	}
	if tc.Name == "todo_write" {
		call.Arguments = args
		return call
	}
	if tc.ID == "" {
		call.Arguments = args
		return call
	}
	if args != "" {
		call.ArgumentsArchived = true
	}
	return call
}

func historyToolResultsByID(msgs []provider.Message) map[string]provider.Message {
	out := map[string]provider.Message{}
	for _, msg := range msgs {
		if msg.Role != provider.RoleTool || msg.ToolCallID == "" {
			continue
		}
		out[msg.ToolCallID] = msg
	}
	return out
}

func historyToolResultContent(content string, canArchive bool) (display string, archived bool, errPreview string) {
	if content == "" {
		return "", false, ""
	}
	if !canArchive {
		if historyToolResultFailed(content) {
			return content, false, content
		}
		return content, false, ""
	}
	if historyToolResultFailed(content) {
		display = clipHistoryToolPreview(strings.TrimSpace(content))
		return display, display != content, display
	}
	return "", true, ""
}

func clipHistoryToolPreview(s string) string {
	if len(s) <= historyToolPreviewLimit {
		return s
	}
	return strings.TrimSpace(clipStringBytes(s, historyToolPreviewLimit)) + "\n..."
}

func historyToolSubject(name, args string) string {
	a := parseHistoryToolArgs(args)
	var subject string
	switch name {
	case "bash":
		subject = historyArgString(a, "command")
	case "grep", "glob":
		subject = firstNonEmpty(historyArgString(a, "pattern"), historyArgString(a, "path"))
	case "web_fetch":
		subject = historyArgString(a, "url")
	case "task":
		subject = firstNonEmpty(historyArgString(a, "description"), historyArgString(a, "prompt"))
	case "run_skill":
		subject = historyArgString(a, "name")
	case "move_file":
		src := historyArgString(a, "source_path")
		dst := historyArgString(a, "destination_path")
		if src != "" && dst != "" {
			subject = src + " -> " + dst
		} else {
			subject = firstNonEmpty(src, dst)
		}
	case "remember":
		subject = firstNonEmpty(historyArgString(a, "name"), historyArgString(a, "description"))
	case "todo_write", "exit_plan_mode":
		subject = ""
	default:
		subject = firstNonEmpty(historyArgString(a, "path"), historyArgString(a, "file_path"))
	}
	return clipSingleLine(subject, 240)
}

func historyToolSummary(name, args, output string) string {
	if historyToolResultFailed(output) {
		return ""
	}
	a := parseHistoryToolArgs(args)
	switch name {
	case "write_file":
		if content := historyArgString(a, "content"); content != "" {
			return fmt.Sprintf("%d lines", historyLineCount(content))
		}
	case "edit_file":
		oldText := historyArgString(a, "old_string")
		newText := historyArgString(a, "new_string")
		if oldText != "" || newText != "" {
			return fmt.Sprintf("%d -> %d lines", historyLineCount(oldText), historyLineCount(newText))
		}
	case "multi_edit":
		if edits, ok := a["edits"].([]any); ok && len(edits) > 0 {
			return fmt.Sprintf("%d edits", len(edits))
		}
	}
	if output == "" {
		return ""
	}
	switch name {
	case "read_file":
		if strings.HasPrefix(output, "(empty file)") {
			return "empty file"
		}
		if arrows := strings.Count(output, "→"); arrows > 0 {
			return fmt.Sprintf("%d lines", arrows)
		}
		return fmt.Sprintf("%d lines", historyLineCount(output))
	case "grep":
		return fmt.Sprintf("%d matches", historyNonEmptyLineCount(output))
	case "glob":
		return fmt.Sprintf("%d files", historyNonEmptyLineCount(output))
	case "ls":
		return fmt.Sprintf("%d entries", historyNonEmptyLineCount(output))
	case "web_fetch":
		return clipSingleLine(strings.SplitN(output, "\n", 2)[0], 80)
	case "bash":
		if strings.TrimSpace(output) == "" {
			return "no output"
		}
		return fmt.Sprintf("%d lines", historyLineCount(output))
	default:
		return ""
	}
}

func parseHistoryToolArgs(args string) map[string]any {
	if args == "" {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(args), &out); err != nil {
		return map[string]any{}
	}
	return out
}

func historyArgString(args map[string]any, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func historyLineCount(s string) int {
	if s == "" {
		return 0
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}

func historyNonEmptyLineCount(s string) int {
	count := 0
	for line := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(line) != "" {
			count++
		}
	}
	return count
}

func clipSingleLine(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if len(s) <= max {
		return s
	}
	if max <= 3 {
		return clipStringBytes(s, max)
	}
	return clipStringBytes(s, max-3) + "..."
}

func clipStringBytes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	for max > 0 && !utf8.RuneStart(s[max]) {
		max--
	}
	return s[:max]
}

func historyTodoArgsWithCompleteSteps(msgs []provider.Message) map[string]string {
	successful := successfulHistoryToolCallIDs(msgs)
	state := newHistoryTodoArgsState(successful)
	for _, m := range msgs {
		state.consume(m)
	}
	return state.out
}

// historyTodoArgsState retains only the derived todo state needed to render a
// todo_write call. It lets windowed history compute the same result as the
// legacy full conversion while streaming messages in bounded chunks.
type historyTodoArgsState struct {
	successful   map[string]bool
	out          map[string]string
	todos        []evidence.TodoItem
	latestTodoID string
}

func newHistoryTodoArgsState(successful map[string]bool) *historyTodoArgsState {
	return &historyTodoArgsState{successful: successful, out: map[string]string{}}
}

func (state *historyTodoArgsState) consume(m provider.Message) {
	for _, tc := range m.ToolCalls {
		if tc.ID == "" || !state.successful[tc.ID] {
			continue
		}
		switch tc.Name {
		case "todo_write":
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)
			if len(rec.Todos) == 0 {
				continue
			}
			state.todos = evidence.NormalizeSerialTodos(rec.Todos)
			state.latestTodoID = tc.ID
			if args, ok := todoArgsJSON(state.todos); ok {
				state.out[state.latestTodoID] = args
			}
		case "complete_step":
			if state.latestTodoID == "" || len(state.todos) == 0 {
				continue
			}
			rec := evidence.ReceiptFromToolCall(tc.Name, json.RawMessage(tc.Arguments), true, true)
			match, ok := evidence.MatchStep(rec.Step, state.todos)
			if !ok || !evidence.AdvanceSerialTodo(state.todos, match.Index-1) {
				continue
			}
			if args, ok := todoArgsJSON(state.todos); ok {
				state.out[state.latestTodoID] = args
			}
		}
	}
}

func successfulHistoryToolCallIDs(msgs []provider.Message) map[string]bool {
	successful := map[string]bool{}
	for _, msg := range msgs {
		if msg.Role != provider.RoleTool || msg.ToolCallID == "" {
			continue
		}
		if !historyToolResultFailed(msg.Content) {
			successful[msg.ToolCallID] = true
		}
	}
	return successful
}

func historyToolResultFailed(content string) bool {
	content = strings.TrimSpace(content)
	return strings.HasPrefix(content, "error:") ||
		strings.HasPrefix(content, "blocked:") ||
		strings.HasPrefix(content, "Error:") ||
		strings.HasPrefix(content, "[error")
}

func todoArgsJSON(todos []evidence.TodoItem) (string, bool) {
	b, err := json.Marshal(map[string]any{"todos": todos})
	if err != nil {
		return "", false
	}
	return string(b), true
}
