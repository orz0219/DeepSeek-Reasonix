package agent

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"reasonix/internal/provider"
)

// firstLine returns s up to its first newline — a one-line failure summary for
// the display Err, while the full error stays in the model-facing output.
func firstLine(s string) string {
	if before, _, ok := strings.Cut(s, "\n"); ok {
		return before
	}
	return s
}

// truncateToolOutput is the first-visible hard cap for a tool result. Under-cap
// bodies are returned byte-identical. Over-cap bodies keep a tool-aware head and
// tail under maxToolOutputBytes; the full original is stored separately as
// RawContent by the session writer. The bounded form is stable for the message
// lifetime and is never re-truncated by later maintenance.
func truncateToolOutput(s string) (string, string) {
	return truncateToolOutputFor(s, "", "")
}

// truncateToolOutputFor is the tool-aware first-visible limiter. toolName and
// toolCallID populate the truncation marker so the model can re-fetch.
func truncateToolOutputFor(s, toolName, toolCallID string) (string, string) {
	if len(s) <= maxToolOutputBytes {
		return s, ""
	}
	strategy := snipStrategy{head: 40, tail: 40, headChars: 8000, tailChars: 8000}
	switch {
	case toolName == "bash" || toolName == "shell" || strings.Contains(toolName, "bash"):
		strategy = snipStrategy{head: 40, tail: 40, headChars: 8000, tailChars: 8000}
	case toolName == "read_file" || toolName == "web_fetch" || strings.Contains(toolName, "read"):
		strategy = snipStrategy{head: 120, tail: 12, headChars: 12000, tailChars: 2000}
	case toolName == "grep" || toolName == "glob" || toolName == "ls" || toolName == "list_dir":
		strategy = snipStrategy{head: 80, tail: 8, headChars: 10000, tailChars: 1000}
	}
	headKeep := strategy.headChars
	tailKeep := strategy.tailChars
	if headKeep+tailKeep > maxToolOutputBytes-512 {
		headKeep = maxToolOutputBytes * 2 / 3
		tailKeep = maxToolOutputBytes - headKeep - 512
	}
	if headKeep < 1024 {
		headKeep = maxToolOutputBytes / 2
		tailKeep = maxToolOutputBytes / 2
	}

	lower := strings.ToLower(s)
	if strings.Contains(lower, "error:") || strings.Contains(lower, "panic:") || strings.Contains(lower, "fatal:") {
		tailKeep = max(tailKeep, maxToolOutputBytes/3)
		if headKeep+tailKeep > maxToolOutputBytes-512 {
			headKeep = maxToolOutputBytes - 512 - tailKeep
		}
	}
	head := snapToRuneBoundary(s, 0, headKeep)
	tail := snapToRuneBoundary(s, len(s)-tailKeep, len(s))
	omitted := len(s) - len(head) - len(tail)
	namePart := toolName
	if namePart == "" {
		namePart = "tool"
	}
	idPart := toolCallID
	if idPart == "" {
		idPart = "-"
	}
	notice := fmt.Sprintf("tool output truncated: %d of %d bytes elided", omitted, len(s))
	marker := fmt.Sprintf(
		"\n\n…[truncated tool=%s call_id=%s original_bytes=%d kept_bytes=%d — full original retained in canonical transcript; re-read or retry with narrower args]…\n\n",
		namePart, idPart, len(s), len(head)+len(tail),
	)
	body := head + marker + tail
	if len(body) > maxToolOutputBytes {
		overflow := len(body) - maxToolOutputBytes
		trimHead := overflow / 2
		trimTail := overflow - trimHead
		if trimHead < len(head) {
			head = snapToRuneBoundary(head, 0, len(head)-trimHead)
		}
		if trimTail < len(tail) {
			tail = snapToRuneBoundary(tail, trimTail, len(tail))
		}
		body = head + marker + tail
	}
	return body, notice
}

// snapToRuneBoundary returns s[lo:hi] with the bounds nudged outward until
// both land on rune-start positions.
func snapToRuneBoundary(s string, lo, hi int) string {
	for lo > 0 && !utf8.RuneStart(s[lo]) {
		lo--
	}
	for hi < len(s) && !utf8.RuneStart(s[hi]) {
		hi++
	}
	return s[lo:hi]
}

// finishReasonMessage maps an abnormal finish_reason to a one-line warning,
// returning ok=false for the normal terminations ("stop", "tool_calls") and a
// nil usage. The sink renders the message; the "! " prefix is presentation.
func finishReasonMessage(u *provider.Usage) (string, bool) {
	if u == nil {
		return "", false
	}
	switch u.FinishReason {
	case "length":
		return "response truncated: hit max output tokens", true
	case finishReasonClientReasoningLimit:
		return "response stopped: hit the client reasoning safety limit", true
	case "content_filter":
		return "response blocked by content filter", true
	case "repetition_truncation":
		return "response truncated: model repetition detected", true
	default:
		return "", false
	}
}
