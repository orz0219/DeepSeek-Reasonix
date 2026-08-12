package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"reasonix/internal/evidence"
	"reasonix/internal/provider"
	"reasonix/internal/shellparse"
	"reasonix/internal/tool"
)

func (a *Agent) repeatedSuccessBlock(call provider.ToolCall, t tool.Tool) (string, bool) {
	sig, ok := repeatSuccessSignature(call, t)
	if !ok || a.turn.repeatSuccessCounts == nil {
		return "", false
	}
	count := a.turn.repeatSuccessCounts[sig]
	if count < repeatSuccessBreakThreshold {
		return "", false
	}
	return fmt.Sprintf(
		"blocked: [loop guard] %q has already succeeded %d times with the same write-like arguments in this user turn. Re-running it is unlikely to help and may burn tokens or repeat file writes. Change approach: use edit_file or multi_edit for file changes, verify with a read/test command, or explain the blocker in your final answer.",
		call.Name, count), true
}

func (a *Agent) staleAnchorEditBlock(call provider.ToolCall) (string, bool) {
	if a.task.ledger == nil || !anchorBasedEditTool(call.Name) {
		return "", false
	}
	rec := evidence.ReceiptFromToolCall(call.Name, json.RawMessage(call.Arguments), true, false)
	if len(rec.Paths) == 0 {
		return "", false
	}
	writeIndex, ok := a.task.ledger.LatestSuccessfulWriteIndex(rec.Paths)
	if !ok || a.task.ledger.HasSuccessfulAnchorRefreshReadAfter(rec.Paths, writeIndex) {
		return "", false
	}
	return fmt.Sprintf(
		"blocked: [fresh read required] %q targets %s, which was already modified earlier this turn. Re-read the current file with read_file without offset/limit before another range deletion, or use multi_edit with exact replacements when possible. This prevents stale start/end anchors from selecting an unintended destructive span.",
		call.Name, strings.Join(rec.Paths, ", ")), true
}

func anchorBasedEditTool(name string) bool {
	switch name {

	case "delete_range":
		return true
	default:
		return false
	}
}

func (a *Agent) recordRepeatSuccess(call provider.ToolCall, t tool.Tool) {
	sig, ok := repeatSuccessSignature(call, t)
	if !ok {
		return
	}
	if a.turn.repeatSuccessCounts == nil {
		a.turn.repeatSuccessCounts = make(map[string]int)
	}
	a.turn.repeatSuccessCounts[sig]++
}

func repeatSuccessSignature(call provider.ToolCall, t tool.Tool) (string, bool) {
	if t.ReadOnly() {
		return "", false
	}
	switch call.Name {
	case "write_file", "edit_file", "multi_edit", "move_file", "notebook_edit":
		return call.Name + "\x00" + canonicalToolArgs(call.Arguments), true
	case "bash":
		var p struct {
			Command         string `json:"command"`
			RunInBackground bool   `json:"run_in_background"`
		}
		if err := json.Unmarshal([]byte(call.Arguments), &p); err != nil {
			return "", false
		}
		if p.RunInBackground || !isShellFileWriteCommand(p.Command) {
			return "", false
		}
		return "bash\x00" + normalizeShellCommand(p.Command), true
	default:
		return "", false
	}
}

func canonicalToolArgs(raw string) string {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return strings.TrimSpace(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return strings.TrimSpace(raw)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, b); err != nil {
		return string(b)
	}
	return compact.String()
}

func normalizeShellCommand(command string) string {
	if fields, malformed := shellparse.StaticFields(command); malformed == "" && len(fields) > 0 {
		return strings.Join(fields, " ")
	}
	return strings.Join(strings.Fields(command), " ")
}

func isShellFileWriteCommand(command string) bool {
	lower := strings.ToLower(command)
	switch {
	case shellPythonOpenWrites(lower):
		return true
	case strings.Contains(lower, "set-content") || strings.Contains(lower, "add-content") || strings.Contains(lower, "out-file"):
		return true
	case strings.Contains(lower, "sed -i") || strings.Contains(lower, "perl -pi"):
		return true
	case hasShellWriteRedirect(command):
		return true
	default:
		return false
	}
}

func shellPythonOpenWrites(lower string) bool {
	if !strings.Contains(lower, "open(") {
		return false
	}
	if strings.Contains(lower, ".write(") {
		return true
	}
	for _, marker := range []string{", 'w", `, "w`, ", 'a", `, "a`, ", 'x", `, "x`, "mode='w", `mode="w`, "mode='a", `mode="a`, "mode='x", `mode="x`} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasShellWriteRedirect(command string) bool {
	file, err := shellparse.ParseBash(command)
	if err == nil {
		hasWrite := false
		syntax.Walk(file, func(node syntax.Node) bool {
			redir, ok := node.(*syntax.Redirect)
			if !ok {
				return true
			}
			if bashRedirectWritesFile(command, redir) {
				hasWrite = true
				return false
			}
			return true
		})
		return hasWrite
	}
	return hasShellWriteRedirectFallback(command)
}

func bashRedirectWritesFile(source string, redir *syntax.Redirect) bool {
	if redir == nil {
		return false
	}
	switch redir.Op {
	case syntax.RdrOut, syntax.AppOut, syntax.RdrClob, syntax.AppClob,
		syntax.RdrAll, syntax.RdrAllClob, syntax.AppAll, syntax.AppAllClob,
		syntax.RdrInOut:
		return !redirectWordIsNullSink(source, redir.Word)
	default:
		return false
	}
}

func redirectWordIsNullSink(source string, word *syntax.Word) bool {
	if word == nil {
		return false
	}
	if value, ok := shellparse.StaticWord(word); ok {
		if isNullSinkWord(strings.TrimSpace(value)) {
			return true
		}
	}
	value := strings.TrimSpace(redirectWordSource(source, word))
	if isNullSinkWord(value) {
		return true
	}
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
		return isNullSinkWord(value[1 : len(value)-1])
	}
	return false
}

func isNullSinkWord(value string) bool {
	if value == "/dev/null" {
		return true
	}
	return strings.EqualFold(value, "$null") || strings.EqualFold(value, "nul")
}

func redirectWordSource(source string, word *syntax.Word) string {
	if word == nil || !word.Pos().IsValid() || !word.End().IsValid() {
		return ""
	}
	start := int(word.Pos().Offset())
	end := int(word.End().Offset())
	if start < 0 || end < start || end > len(source) {
		return ""
	}
	return source[start:end]
}

func hasShellWriteRedirectFallback(command string) bool {
	var quote rune
	var prev rune
	for _, r := range command {
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			prev = r
			continue
		}
		if r == '\'' || r == '"' {
			quote = r
			prev = r
			continue
		}
		if r == '>' {
			if prev == '2' {
				prev = r
				continue
			}
			return true
		}
		prev = r
	}
	return false
}

// isBackgroundTaskCall reports whether a `task` call set run_in_background, so a
// fire-and-return dispatch isn't mistaken for a sub-agent that has stopped.
func isBackgroundTaskCall(args string) bool {
	var p struct {
		RunInBackground bool `json:"run_in_background"`
	}
	_ = json.Unmarshal([]byte(args), &p)
	return p.RunInBackground
}

// toolReadOnly reports a tool's ReadOnly classification by name (false for an
// unknown tool), for stamping early ToolDispatch events.
func (a *Agent) toolReadOnly(name string) bool {
	t, _, ambiguous := a.svc.tools.ResolveCall(name)
	return t != nil && len(ambiguous) == 0 && t.ReadOnly()
}
