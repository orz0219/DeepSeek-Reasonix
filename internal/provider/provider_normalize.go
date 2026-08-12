package provider

import (
	"encoding/json"
	"slices"
	"strings"
)

// interruptedToolResult stands in for a tool result that never landed — an
// assistant tool_calls turn whose execution was cut short (interrupt, crash) and
// later resumed. Sending such a turn unanswered trips the OpenAI/DeepSeek 400
// "An assistant message with 'tool_calls' must be followed by tool messages
// responding to each 'tool_call_id'".
const interruptedToolResult = "[no result: the previous turn was interrupted before this tool call completed]"

// SanitizeToolPairing is the provider-side alias for NormalizeMessages. It repairs
// a history so it satisfies the tool-call contract the OpenAI-compatible and
// Anthropic APIs enforce (every assistant tool_calls answered, no orphan tool
// messages, truncated args closed) right before sending it to the wire — without
// touching the stored session. Kept as a distinct name so call sites read as
// "defensive wire prep" rather than "session mutation".
func SanitizeToolPairing(msgs []Message) []Message { return NormalizeMessages(msgs) }

// NormalizeMessages repairs a conversation history so it satisfies the tool-call
// contract the OpenAI-compatible and Anthropic APIs enforce: every assistant
// tool_calls entry must be answered by a following tool message for its id, and a
// tool message must follow such a call. It backfills a placeholder result for any
// unanswered call (so the turn stays intact), drops orphan tool messages,
// backfills empty tool-call names from their results (#4727 — old sessions saved
// before adde2d3e can carry an empty name), and closes truncated call-argument
// JSON (DeepSeek 400s on replayed half-streamed args, #3953).
//
// This is the wire-safe entry point for provider requests. Stored session loads
// use NormalizeSessionMessages so they can share the assistant-turn repairs
// without deleting standalone tool messages that must round-trip through
// reasonix --resume.
//
// A well-formed history — no unanswered calls, no orphan results, no empty tool-
// call names, no truncated args — returns the input slice unchanged (same backing
// array, zero allocation). This keeps the prefix-cache key stable for healthy
// sessions and makes repeated normalization cheap.
func NormalizeMessages(msgs []Message) []Message {
	return normalizeMessages(msgs, true)
}

// NormalizeSessionMessages applies only repairs that are safe to persist in a
// saved session. It shares assistant-turn repairs with NormalizeMessages, but
// preserves existing tool messages instead of dropping or reordering them so
// Save/LoadSession remains a byte-for-byte conversation round trip for histories
// that were already on disk.
func NormalizeSessionMessages(msgs []Message) []Message {
	return normalizeMessages(attachStandaloneDecisionReceipts(msgs), false)
}

// attachStandaloneDecisionReceipts migrates the short-lived receipt encoding
// that stored a LocalOnly assistant message between an assistant tool call and
// its result. Folding that metadata into the latest assistant message repairs
// already-written sessions before tool-pair normalization can fabricate a
// placeholder. Healthy histories return the original slice unchanged.
func attachStandaloneDecisionReceipts(msgs []Message) []Message {
	target := -1
	needsMigration := false
	for i, m := range msgs {
		switch {
		case m.Role == RoleUser && !m.LocalOnly:
			target = -1
		case m.Role == RoleAssistant && !m.LocalOnly:
			target = i
		case target >= 0 && m.LocalOnly && m.DecisionReceipt != nil:
			needsMigration = true
		}
		if needsMigration {
			break
		}
	}
	if !needsMigration {
		return msgs
	}

	out := make([]Message, 0, len(msgs))
	target = -1
	for _, m := range msgs {
		switch {
		case m.Role == RoleUser && !m.LocalOnly:
			target = -1
		case m.Role == RoleAssistant && !m.LocalOnly:
			out = append(out, m)
			target = len(out) - 1
			continue
		case target >= 0 && m.LocalOnly && m.DecisionReceipt != nil:
			receipts := append([]*DecisionReceipt(nil), out[target].DecisionReceipts...)
			out[target].DecisionReceipts = append(receipts, m.DecisionReceipt)
			continue
		}
		out = append(out, m)
	}
	return out
}

func normalizeMessages(msgs []Message, dropOrphanTools bool) []Message {
	if normalized, ok := tryNormalizeFastPath(msgs, dropOrphanTools); ok {
		return normalized
	}
	out := make([]Message, 0, len(msgs))
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.LocalOnly {
			if !dropOrphanTools {
				out = append(out, m)
			}
			i++
			continue
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < len(msgs) && msgs[j].Role == RoleTool && !msgs[j].LocalOnly {
				j++
			}

			calls := backfillToolCallNames(m.ToolCalls, msgs[i+1:j])
			m.ToolCalls = calls
			out = append(out, repairToolCallArgs(m))
			if dropOrphanTools {
				out = append(out, pairToolResults(calls, msgs[i+1:j])...)
			} else {
				out = append(out, sessionToolResults(calls, msgs[i+1:j])...)
			}
			i = j
			continue
		}
		if m.Role == RoleTool {
			if !dropOrphanTools {
				out = append(out, m)
			}

			i++
			continue
		}
		out = append(out, m)
		i++
	}
	return out
}

// tryNormalizeFastPath reports whether msgs needs no repair and, if so, returns
// it as-is so the caller can skip allocating. Healthy tool-call/tool-result
// turns pass through unchanged; malformed turns take the slow path.
func tryNormalizeFastPath(msgs []Message, dropOrphanTools bool) ([]Message, bool) {
	for i := 0; i < len(msgs); {
		m := msgs[i]
		if m.LocalOnly {
			if dropOrphanTools {
				return nil, false
			}
			i++
			continue
		}
		if m.Role == RoleAssistant && len(m.ToolCalls) > 0 {
			j := i + 1
			for j < len(msgs) && msgs[j].Role == RoleTool && !msgs[j].LocalOnly {
				j++
			}
			if !toolTurnWellFormed(m.ToolCalls, msgs[i+1:j]) || needsToolCallArgRepair(m.ToolCalls) {
				return nil, false
			}
			i = j
			continue
		}
		if m.Role == RoleTool && dropOrphanTools {
			return nil, false
		}
		i++
	}
	return msgs, true
}

func toolTurnWellFormed(calls []ToolCall, results []Message) bool {
	if len(calls) != len(results) {
		return false
	}
	for _, tc := range calls {
		if tc.Name == "" {
			return false
		}
	}
	for k, tc := range calls {
		if results[k].ToolCallID != tc.ID {
			return false
		}
		if results[k].Name != tc.Name {
			return false
		}
	}
	return true
}

func needsToolCallArgRepair(calls []ToolCall) bool {
	for _, tc := range calls {
		if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
			return true
		}
	}
	return false
}

// repairToolCallArgs returns m with any undecodable tool-call Arguments closed
// into valid JSON (copy-on-write; the caller's history is never mutated). Empty
// arguments pass through — some gateways send "" for no-arg tools.
func repairToolCallArgs(m Message) Message {
	broken := false
	for _, tc := range m.ToolCalls {
		if tc.Arguments != "" && !json.Valid([]byte(tc.Arguments)) {
			broken = true
			break
		}
	}
	if !broken {
		return m
	}
	calls := make([]ToolCall, len(m.ToolCalls))
	copy(calls, m.ToolCalls)
	for i := range calls {
		if calls[i].Arguments == "" || json.Valid([]byte(calls[i].Arguments)) {
			continue
		}
		calls[i].Arguments = closeTruncatedJSON(calls[i].Arguments)
	}
	m.ToolCalls = calls
	return m
}

// closeTruncatedJSON best-effort completes a JSON document cut off mid-stream
// (unterminated string, open braces, dangling comma/colon); anything still
// invalid after closing degrades to "{}".
func closeTruncatedJSON(s string) string {
	var stack []byte
	inStr, esc := false, false
	for i := range len(s) {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			stack = append(stack, '}')
		case '[':
			stack = append(stack, ']')
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	out := s
	if esc {
		out = out[:len(out)-1]
	}
	if inStr {
		out += `"`
	}
	trimmed := strings.TrimRight(out, " \t\r\n")
	switch {
	case strings.HasSuffix(trimmed, ","):
		out = trimmed[:len(trimmed)-1]
	case strings.HasSuffix(trimmed, ":"):
		out = trimmed + "null"
	}
	for _, v := range slices.Backward(stack) {
		out += string(v)
	}
	if !json.Valid([]byte(out)) {
		return "{}"
	}
	return out
}

// pairToolResults answers each tool_call with its result, backfilling a
// placeholder for any unanswered one. Distinct non-empty ids pair by id (so
// reordered results re-sort to call order); empty or duplicate ids pair by
// position instead — some gateways stream tool calls by index with no id, and a
// map keyed on id would collapse those results into one (call order is preserved
// because the loop appends results in call order).
func pairToolResults(calls []ToolCall, avail []Message) []Message {
	out := make([]Message, 0, len(calls))
	if idDistinct(calls) {
		byID := make(map[string]Message, len(avail))
		for _, r := range avail {
			byID[r.ToolCallID] = r
		}
		for _, tc := range calls {
			if r, ok := byID[tc.ID]; ok {
				r.Name = tc.Name
				out = append(out, r)
			} else {
				out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
			}
		}
		return out
	}
	for k, tc := range calls {
		if k < len(avail) {
			r := avail[k]
			r.ToolCallID = tc.ID
			r.Name = tc.Name
			out = append(out, r)
		} else {
			out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
		}
	}
	return out
}

// sessionToolResults preserves every stored tool result and appends placeholders
// only for calls that have no recorded answer. Load-time normalization must not
// drop or reorder user history; provider sends can still use pairToolResults for
// strict wire formatting.
func sessionToolResults(calls []ToolCall, avail []Message) []Message {
	out := append([]Message(nil), avail...)
	if idDistinct(calls) {
		answered := make(map[string]struct{}, len(avail))
		for _, r := range avail {
			answered[r.ToolCallID] = struct{}{}
		}
		for _, tc := range calls {
			if _, ok := answered[tc.ID]; !ok {
				out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
			}
		}
		return out
	}
	for k := len(avail); k < len(calls); k++ {
		tc := calls[k]
		out = append(out, Message{Role: RoleTool, ToolCallID: tc.ID, Name: tc.Name, Content: interruptedToolResult})
	}
	return out
}

// backfillToolCallNames returns calls with any empty Name filled in from the
// matching tool result (by id, then by position). Old sessions (#4727) may have
// saved assistant tool-calls with an empty name; backfilling gives the model
// useful context during replay. The common case (no empty names) returns the
// input unchanged without allocating. Unpaired calls keep their empty name,
// which the wire-format fix (openai.go) handles gracefully.
func backfillToolCallNames(calls []ToolCall, results []Message) []ToolCall {
	missing := false
	for _, c := range calls {
		if c.Name == "" {
			missing = true
			break
		}
	}
	if !missing {
		return calls
	}
	out := make([]ToolCall, len(calls))
	copy(out, calls)
	if idDistinct(calls) {
		byID := make(map[string]string, len(results))
		for _, r := range results {
			if r.Name != "" {
				byID[r.ToolCallID] = r.Name
			}
		}
		for k := range out {
			if out[k].Name == "" {
				if n, ok := byID[out[k].ID]; ok {
					out[k].Name = n
				}
			}
		}
		return out
	}

	for k := range out {
		if out[k].Name == "" && k < len(results) {
			out[k].Name = results[k].Name
		}
	}
	return out
}

// idDistinct reports whether every call carries a non-empty id unique within the
// batch — the condition under which id-keyed pairing is safe.
func idDistinct(calls []ToolCall) bool {
	seen := make(map[string]struct{}, len(calls))
	for _, tc := range calls {
		if tc.ID == "" {
			return false
		}
		if _, dup := seen[tc.ID]; dup {
			return false
		}
		seen[tc.ID] = struct{}{}
	}
	return true
}
