package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"slices"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/control"
	"reasonix/internal/event"
	"reasonix/internal/provider"
)

func historyMessagesWithPlannerDisplays(msgs []provider.Message, resolveUserContent func(string) string, plannerTurns []plannerDisplayTurn, checkpointTurns map[int]int) []HistoryMessage {
	replayedTodoArgs := historyTodoArgsWithCompleteSteps(msgs)
	toolResults := historyToolResultsByID(msgs)
	return historyMessagesWithPlannerDisplaysAndLookups(msgs, resolveUserContent, plannerTurns, checkpointTurns, replayedTodoArgs, toolResults)
}

// historyMessageConvertState carries the cross-message state of a provider→
// HistoryMessage conversion pass: the planner-display queue (consumed in order
// per user-text hash) and the canonical-turn suppression a planner interrupt
// notice arms. Keeping it explicit lets the windowed history slice API convert
// one message at a time with exactly the same semantics as a full pass.
type historyMessageConvertState struct {
	plannerByUserHash     map[string][]plannerDisplayTurn
	suppressCanonicalTurn bool
}

func newHistoryMessageConvertState(plannerTurns []plannerDisplayTurn) *historyMessageConvertState {
	return &historyMessageConvertState{plannerByUserHash: plannerTurnsByUserHash(plannerTurns)}
}

func historyMessagesWithPlannerDisplaysAndLookups(
	msgs []provider.Message,
	resolveUserContent func(string) string,
	plannerTurns []plannerDisplayTurn,
	checkpointTurns map[int]int,
	replayedTodoArgs map[string]string,
	toolResults map[string]provider.Message,
) []HistoryMessage {
	out := make([]HistoryMessage, 0, len(msgs))
	state := newHistoryMessageConvertState(plannerTurns)
	for index, m := range msgs {
		out = append(out, state.convertHistoryMessage(index, m, resolveUserContent, checkpointTurns, replayedTodoArgs, toolResults)...)
	}
	return out
}

// convertHistoryMessage converts one provider message into its 0..n history
// rows. index is the message's position in the coordinate system of
// checkpointTurns (window-relative for the legacy full-pass callers, absolute
// for the windowed slice API).
func (state *historyMessageConvertState) convertHistoryMessage(
	index int,
	m provider.Message,
	resolveUserContent func(string) string,
	checkpointTurns map[int]int,
	replayedTodoArgs map[string]string,
	toolResults map[string]provider.Message,
) []HistoryMessage {
	var out []HistoryMessage
	if m.DecisionReceipt != nil {
		return append(out, HistoryMessage{
			Role:            "notice",
			Code:            event.NoticeCodeDecisionReceipt,
			Level:           "info",
			DecisionReceipt: cloneDecisionReceipt(m.DecisionReceipt),
		})
	}
	if m.LocalOnly {
		if steerText, isSteer := agent.SteerText(agent.UserMessageText(m)); isSteer {
			return append(out, HistoryMessage{
				Role:    "notice",
				Content: agent.UnappliedSteerNotice(steerText),
				Code:    event.NoticeCodeUnappliedSteer,
				Level:   "warn",
			})
		}
	}
	if state.suppressCanonicalTurn {
		if m.Role != provider.RoleUser || !agent.IsUserAuthoredTurn(agent.UserMessageText(m)) {
			return out
		}
		state.suppressCanonicalTurn = false
	}
	content := m.Content
	var checkpointTurn *int
	if m.Role == provider.RoleUser {

		if steerText, isSteer := agent.SteerText(agent.UserMessageText(m)); isSteer {
			return append(out, HistoryMessage{Role: "notice", Content: "↪ " + steerText})
		}
		content = historyUserDisplayContent(m, resolveUserContent)
		if control.IsSyntheticUserMessage(content) {
			return out
		}
		if turn, ok := checkpointTurns[index]; ok {
			turnCopy := turn
			checkpointTurn = &turnCopy
		}
	}
	reasoning := ""
	if m.Role == provider.RoleAssistant || m.LocalOnly {
		reasoning = m.ReasoningContent
	}
	displayRole := string(m.Role)
	if m.LocalOnly {
		displayRole = "assistant"
	}
	hm := HistoryMessage{Role: displayRole, Content: content, CheckpointTurn: checkpointTurn, CreatedAt: m.CreatedAt, Reasoning: reasoning, WorkDurationMs: m.WorkDurationMs}
	if m.Role == provider.RoleAssistant && len(m.MemoryCitations) > 0 {
		hm.MemoryCitations = append([]provider.MemoryCitation(nil), m.MemoryCitations...)
	}
	if m.Role == provider.RoleUser && content != m.Content {
		replay := historyReplayUserContent(m.Content)
		if agent.ContainsMemoryCompilerExecution(m.Content) {

			if strings.HasPrefix(strings.TrimSpace(replay), "/") && replay != content {
				hm.SubmitText = replay
			}
		} else if replay != content {
			hm.SubmitText = replay
		}
	}
	if (m.Role == provider.RoleAssistant || m.LocalOnly) && len(m.ToolCalls) > 0 {
		hm.ToolCalls = make([]HistoryToolCall, len(m.ToolCalls))
		for i, tc := range m.ToolCalls {
			args := tc.Arguments
			if tc.Name == "todo_write" {
				if replayed, ok := replayedTodoArgs[tc.ID]; ok {
					args = replayed
				}
			}
			hm.ToolCalls[i] = historyToolCall(tc, args, toolResults[tc.ID])
		}
	}
	if m.Role == provider.RoleTool && !m.LocalOnly {
		hm.ToolCallID = m.ToolCallID
		hm.ToolName = m.Name
		hm.Content, hm.ToolResultArchived, hm.ToolResultError = historyToolResultContent(m.Content, m.ToolCallID != "")
		hm.Execution = m.ToolExecution
	}
	hasVisibleLocalContent := strings.TrimSpace(hm.Content) != "" || strings.TrimSpace(hm.Reasoning) != "" || len(hm.ToolCalls) > 0 || (!m.LocalOnly && m.Role == provider.RoleTool)
	if !m.LocalOnly || hasVisibleLocalContent {
		out = append(out, hm)
	}
	for _, receipt := range m.DecisionReceipts {
		if receipt == nil {
			continue
		}
		out = append(out, HistoryMessage{
			Role:            "notice",
			Code:            event.NoticeCodeDecisionReceipt,
			Level:           "info",
			DecisionReceipt: cloneDecisionReceipt(receipt),
		})
	}
	if m.LocalOnly && m.InterruptedTurn != nil {
		out = append(out, HistoryMessage{
			Role: "notice", Level: "info", Code: event.NoticeCodeCancelledTurn,
			Content: "This turn was interrupted. Partial output is kept for reference; only completed tool pairs and a bounded recovery summary enter the next model turn. Inspect the workspace before continuing or reverting changes.",
		})
	}
	if m.Role == provider.RoleUser {
		key := messageDisplayKey(agent.UserMessageText(m))
		if turns := state.plannerByUserHash[key]; len(turns) > 0 {
			out = append(out, cloneHistoryMessages(turns[0].Messages)...)
			state.suppressCanonicalTurn = plannerDisplaySuppressesCanonical(turns[0])
			state.plannerByUserHash[key] = turns[1:]
		}
	}
	return out
}

// consumeHistoryPlannerState advances only the cross-message planner state.
// Windowed pages call it for the prefix they do not render, so repeated user
// text and a planner interrupt at a page boundary behave exactly as one full
// conversion pass. Keep the early returns in lock-step with
// convertHistoryMessage: those rows never reach the planner attachment at its
// tail.
func (state *historyMessageConvertState) consumeHistoryPlannerState(m provider.Message, resolveUserContent func(string) string) {
	if m.DecisionReceipt != nil {
		return
	}
	if m.LocalOnly {
		if _, isSteer := agent.SteerText(agent.UserMessageText(m)); isSteer {
			return
		}
	}
	if state.suppressCanonicalTurn {
		if m.Role != provider.RoleUser || !agent.IsUserAuthoredTurn(agent.UserMessageText(m)) {
			return
		}
		state.suppressCanonicalTurn = false
	}
	if m.Role != provider.RoleUser {
		return
	}
	if control.IsSyntheticUserMessage(historyUserDisplayContent(m, resolveUserContent)) {
		return
	}
	key := messageDisplayKey(agent.UserMessageText(m))
	if turns := state.plannerByUserHash[key]; len(turns) > 0 {
		state.suppressCanonicalTurn = plannerDisplaySuppressesCanonical(turns[0])
		state.plannerByUserHash[key] = turns[1:]
	}
}

func cloneDecisionReceipt(in *provider.DecisionReceipt) *provider.DecisionReceipt {
	if in == nil {
		return nil
	}
	copy := *in
	return &copy
}

func plannerDisplaySuppressesCanonical(turn plannerDisplayTurn) bool {
	for _, message := range turn.Messages {
		if message.Role == "notice" && message.Code == event.NoticeCodeCancelledTurn {
			return true
		}
	}
	return false
}

func historyPageFromProviderMessages(
	msgs []provider.Message,
	resolveUserContent func(string) string,
	plannerTurns []plannerDisplayTurn,
	checkpointTurns map[int]int,
	beforeTurn, limit int,
) HistoryPage {
	limit = normalizeHistoryPageLimit(limit)
	totalTurns := visibleHistoryUserTurns(msgs, resolveUserContent)
	if beforeTurn <= 0 || beforeTurn > totalTurns {
		beforeTurn = totalTurns
	}
	startTurn := max(beforeTurn-limit, 0)
	page := HistoryPage{
		StartTurn:  startTurn,
		EndTurn:    beforeTurn,
		TotalTurns: totalTurns,
		HasOlder:   startTurn > 0,
	}
	if len(msgs) == 0 || startTurn >= beforeTurn {
		page.Messages = []HistoryMessage{}
		return page
	}
	pageMessages, originalIndexes := providerMessagesForVisibleTurnRange(msgs, resolveUserContent, startTurn, beforeTurn)
	page.Messages = historyMessagesWithPlannerDisplaysAndLookups(
		pageMessages,
		resolveUserContent,
		plannerTurns,
		checkpointTurnsForProviderWindow(checkpointTurns, originalIndexes),
		historyTodoArgsWithCompleteSteps(msgs),
		historyToolResultsByID(msgs),
	)
	return page
}

func visibleHistoryUserTurns(msgs []provider.Message, resolveUserContent func(string) string) int {
	total := 0
	for _, msg := range msgs {
		if isVisibleHistoryUser(msg, resolveUserContent) {
			total++
		}
	}
	return total
}

func isVisibleHistoryUser(msg provider.Message, resolveUserContent func(string) string) bool {
	if msg.Role != provider.RoleUser {
		return false
	}
	content := agent.UserMessageText(msg)
	if _, isSteer := agent.SteerText(content); isSteer {
		return false
	}
	content = historyUserDisplayContent(msg, resolveUserContent)
	return !control.IsSyntheticUserMessage(content)
}

func providerMessagesForVisibleTurnRange(msgs []provider.Message, resolveUserContent func(string) string, startTurn, endTurn int) ([]provider.Message, []int) {
	out := make([]provider.Message, 0, len(msgs))
	indexes := make([]int, 0, len(msgs))
	turn := -1
	for index, msg := range msgs {
		if isVisibleHistoryUser(msg, resolveUserContent) {
			turn++
		}
		if turn < 0 {
			if startTurn == 0 {
				out = append(out, msg)
				indexes = append(indexes, index)
			}
			continue
		}
		if turn >= startTurn && turn < endTurn {
			out = append(out, msg)
			indexes = append(indexes, index)
		}
	}
	return out, indexes
}

func checkpointTurnsForProviderWindow(checkpointTurns map[int]int, originalIndexes []int) map[int]int {
	if len(checkpointTurns) == 0 || len(originalIndexes) == 0 {
		return nil
	}
	out := map[int]int{}
	for pageIndex, originalIndex := range originalIndexes {
		if turn, ok := checkpointTurns[originalIndex]; ok {
			out[pageIndex] = turn
		}
	}
	return out
}

func plannerTurnsByUserHash(turns []plannerDisplayTurn) map[string][]plannerDisplayTurn {
	out := map[string][]plannerDisplayTurn{}
	for _, turn := range turns {
		if strings.TrimSpace(turn.UserHash) == "" || len(turn.Messages) == 0 {
			continue
		}
		out[turn.UserHash] = append(out[turn.UserHash], turn)
	}
	return out
}

func cloneHistoryMessages(in []HistoryMessage) []HistoryMessage {
	if len(in) == 0 {
		return nil
	}
	out := make([]HistoryMessage, len(in))
	copy(out, in)
	for i := range out {
		if len(in[i].MemoryCitations) > 0 {
			out[i].MemoryCitations = append([]provider.MemoryCitation(nil), in[i].MemoryCitations...)
		}
		if len(in[i].ToolCalls) > 0 {
			out[i].ToolCalls = append([]HistoryToolCall(nil), in[i].ToolCalls...)
		}
	}
	return out
}

// historyTodoArgsState retains only the derived todo state needed to render a
// todo_write call. It lets windowed history compute the same result as the
// legacy full conversion while streaming messages in bounded chunks.

func previewSessionMessages(sessionDir, path string) ([]HistoryMessage, error) {
	sessionPath, _, err := validateSessionPath(sessionDir, path)
	if err != nil {
		return nil, err
	}
	if out, ok, err := previewEventSessionMessages(sessionPath); ok || err != nil {
		return out, err
	}
	loaded, err := agent.LoadSession(sessionPath)
	if err != nil {
		return nil, err
	}
	return historyMessagesWithPlannerDisplays(
		historyProviderMessagesWithPersistedTimes(loaded.Snapshot(), sessionPath),
		sessionDisplayResolver(sessionDir, sessionPath),
		sessionPlannerDisplayTurns(sessionDir, sessionPath),
		nil,
	), nil
}

func previewSessionPage(sessionDir, path string, beforeTurn, limit int) (HistoryPage, error) {
	sessionPath, _, err := validateSessionPath(sessionDir, path)
	if err != nil {
		return HistoryPage{}, err
	}
	if out, ok, err := previewEventSessionMessages(sessionPath); ok || err != nil {
		if err != nil {
			return HistoryPage{}, err
		}
		return historyPageFromMessages(out, beforeTurn, limit), nil
	}
	loaded, err := agent.LoadSession(sessionPath)
	if err != nil {
		return HistoryPage{}, err
	}
	msgs := loaded.Snapshot()
	digest, _ := agent.ContentDigestForMessages(msgs)
	return historyPageWithFingerprint(historyPageFromProviderMessages(
		historyProviderMessagesWithPersistedTimes(msgs, sessionPath),
		sessionDisplayResolver(sessionDir, sessionPath),
		sessionPlannerDisplayTurns(sessionDir, sessionPath),
		nil,
		beforeTurn,
		limit,
	), sessionPath, digest), nil
}

type previewEventRecord struct {
	Kind             string                    `json:"kind"`
	Type             string                    `json:"type"`
	Role             string                    `json:"role"`
	TS               json.RawMessage           `json:"ts"`
	Time             json.RawMessage           `json:"time"`
	Timestamp        json.RawMessage           `json:"timestamp"`
	CreatedAt        json.RawMessage           `json:"createdAt"`
	CreatedAtSnake   json.RawMessage           `json:"created_at"`
	UpdatedAt        json.RawMessage           `json:"updatedAt"`
	UpdatedAtSnake   json.RawMessage           `json:"updated_at"`
	Text             string                    `json:"text"`
	Detail           string                    `json:"detail"`
	Code             string                    `json:"code"`
	Content          string                    `json:"content"`
	Reasoning        string                    `json:"reasoning"`
	ReasoningContent string                    `json:"reasoningContent"`
	MemoryCitations  []provider.MemoryCitation `json:"memoryCitations"`
	Level            string                    `json:"level"`
	ToolCalls        []previewToolCall         `json:"toolCalls"`
	CallID           string                    `json:"callId"`
	ToolCallID       string                    `json:"toolCallId"`
	ToolName         string                    `json:"toolName"`
	Name             string                    `json:"name"`
	Output           string                    `json:"output"`
	Compaction       *previewCompaction        `json:"compaction"`
	Trigger          string                    `json:"trigger"`
	Messages         int                       `json:"messages"`
	Summary          string                    `json:"summary"`
	Archive          string                    `json:"archive"`
}

type previewToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Function  struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type previewCompaction struct {
	Trigger  string `json:"trigger"`
	Messages int    `json:"messages"`
	Summary  string `json:"summary"`
	Archive  string `json:"archive"`
}

func previewEventSessionMessages(path string) ([]HistoryMessage, bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	dec := json.NewDecoder(f)
	out := []HistoryMessage{}
	toolName := map[string]string{}
	sawEvent := false
	for {
		var rec previewEventRecord
		if err := dec.Decode(&rec); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			if sawEvent {
				return out, true, nil
			}
			return nil, false, nil
		}
		eventName := strings.TrimSpace(rec.Kind)
		if eventName == "" {
			eventName = strings.TrimSpace(rec.Type)
		}
		if eventName == "" {
			continue
		}
		sawEvent = true
		switch eventName {
		case "user.message":
			if rec.Text != "" {
				hm := HistoryMessage{Role: "user", Content: rec.Text}
				if at, ok := promptHistoryEventMillis(rec); ok {
					hm.CreatedAt = at
				}
				out = append(out, hm)
			}
		case "model.final":
			hm := HistoryMessage{Role: "assistant", Content: rec.Content, Reasoning: firstNonEmpty(rec.Reasoning, rec.ReasoningContent)}
			if len(rec.MemoryCitations) > 0 {
				hm.MemoryCitations = append([]provider.MemoryCitation(nil), rec.MemoryCitations...)
			}
			for _, tc := range rec.ToolCalls {
				id := tc.ID
				name := firstNonEmpty(tc.Name, tc.Function.Name)
				args := firstNonEmpty(tc.Arguments, tc.Function.Arguments)
				hm.ToolCalls = append(hm.ToolCalls, historyToolCall(provider.ToolCall{ID: id, Name: name, Arguments: args}, args, provider.Message{}))
				if id != "" {
					toolName[id] = name
				}
			}
			out = append(out, hm)
		case "tool.result":
			callID := firstNonEmpty(rec.CallID, rec.ToolCallID)
			content := firstNonEmpty(rec.Output, rec.Content)
			display, archived, errPreview := historyToolResultContent(content, callID != "")
			if len(out) > 0 && callID != "" {
				updateHistoryToolCallSummary(out, callID, content)
			}
			out = append(out, HistoryMessage{
				Role:               "tool",
				ToolCallID:         callID,
				ToolName:           firstNonEmpty(rec.ToolName, rec.Name, toolName[callID]),
				Content:            display,
				ToolResultArchived: archived,
				ToolResultError:    errPreview,
			})
		case "phase":
			out = append(out, HistoryMessage{Role: "phase", Content: firstNonEmpty(rec.Text, rec.Content)})
		case "notice":
			level := rec.Level
			if level != "warn" {
				level = "info"
			}
			out = append(out, HistoryMessage{Role: "notice", Level: level, Content: firstNonEmpty(rec.Text, rec.Content), Detail: rec.Detail, Code: rec.Code})
		case "compaction_started":
			c := rec.compactionPayload()
			out = append(out, HistoryMessage{Role: "compaction", Pending: true, Trigger: c.Trigger})
		case "compaction_done":
			c := rec.compactionPayload()
			out = append(out, HistoryMessage{
				Role:     "compaction",
				Trigger:  c.Trigger,
				Messages: c.Messages,
				Summary:  c.Summary,
				Archive:  c.Archive,
			})
		}
	}
	return out, sawEvent, nil
}

func (r previewEventRecord) compactionPayload() previewCompaction {
	if r.Compaction != nil {
		return *r.Compaction
	}
	return previewCompaction{Trigger: r.Trigger, Messages: r.Messages, Summary: r.Summary, Archive: r.Archive}
}

func updateHistoryToolCallSummary(out []HistoryMessage, callID, output string) {
	if callID == "" {
		return
	}
	for _, v := range slices.Backward(out) {
		for j := range v.ToolCalls {
			call := &v.ToolCalls[j]
			if call.ID != callID {
				continue
			}
			if call.Summary == "" {
				call.Summary = historyToolSummary(call.Name, call.Arguments, output)
			}
			return
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
