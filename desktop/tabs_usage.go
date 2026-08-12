package main

import (
	"log/slog"
	"maps"
	"reasonix/internal/billing"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/provider"
	"slices"
	"strings"
	"time"
)

func (t *WorkspaceTab) recordReadFile(rec readFileRecord) {
	t.telemMu.Lock()
	t.readTelemetry = append(t.readTelemetry, rec)
	t.telemMu.Unlock()
}

func (t *WorkspaceTab) recordTurnStarted(now int64) {
	t.telemMu.Lock()
	if t.usageTelemetry.activeTurnStartedAt == 0 {
		t.usageTelemetry.activeTurnStartedAt = now
	}
	t.telemMu.Unlock()
}

func (t *WorkspaceTab) recordTurnDone(now int64) {
	t.telemMu.Lock()
	if started := t.usageTelemetry.activeTurnStartedAt; started > 0 && now >= started {
		t.usageTelemetry.ElapsedMs += now - started
		t.usageTelemetry.activeTurnStartedAt = 0
	}
	t.telemMu.Unlock()
}

// contextTelemetryFromUsage returns the latest-attempt context shape for
// rebind-surviving Last* telemetry fields. Prefer Context* when set (multi-
// attempt sampling recovery); otherwise fall back to billable totals / the
// per-event cache delta already computed for this Usage event.
//
// When a Context shape is present, ContextCacheHit/Miss are kept even if both
// are zero — many providers omit cache splits, and falling back to the
// event's aggregated cache would re-inflate multi-attempt totals.
func contextTelemetryFromUsage(u *provider.Usage, eventCacheHit, eventCacheMiss int) (prompt, completion, reasoning, hit, miss int) {
	if u == nil {
		return 0, 0, 0, eventCacheHit, eventCacheMiss
	}
	if u.ContextPromptTokens > 0 || u.ContextCompletionTokens > 0 {
		return u.ContextPromptTokens, u.ContextCompletionTokens, u.ContextReasoningTokens,
			u.ContextCacheHitTokens, u.ContextCacheMissTokens
	}
	return u.PromptTokens, u.CompletionTokens, u.ReasoningTokens, eventCacheHit, eventCacheMiss
}

func (t *WorkspaceTab) recordUsage(e event.Event) {
	if e.Usage == nil {
		return
	}
	u := e.Usage
	source := strings.TrimSpace(e.UsageSource)
	if source == "" {
		source = event.UsageSourceExecutor
	}
	t.telemMu.Lock()
	t.usageTelemetry.PromptTokens += u.PromptTokens
	t.usageTelemetry.CompletionTokens += u.CompletionTokens
	t.usageTelemetry.TotalTokens += u.TotalTokens
	t.usageTelemetry.ReasoningTokens += u.ReasoningTokens
	cacheHitTokens, cacheMissTokens := t.usageTelemetry.cacheTokenDelta(source, u, e.SessionHit, e.SessionMiss)
	t.usageTelemetry.CacheHitTokens += cacheHitTokens
	t.usageTelemetry.CacheMissTokens += cacheMissTokens
	t.usageTelemetry.CacheWriteTokens += u.CacheWriteTokens
	t.usageTelemetry.CacheWriteBilledTokens += u.CacheWriteBilledTokens
	t.usageTelemetry.Estimated = t.usageTelemetry.Estimated || u.Estimated
	requestCount := u.RequestCount
	if requestCount <= 0 {
		requestCount = 1
	}
	t.usageTelemetry.RequestCount += requestCount
	if source == event.UsageSourceExecutor {

		prompt, completion, reasoning, hit, miss := contextTelemetryFromUsage(u, cacheHitTokens, cacheMissTokens)
		t.usageTelemetry.LastUsedTokens = prompt + completion
		t.usageTelemetry.LastPromptTokens = prompt
		t.usageTelemetry.LastCompletionTokens = completion
		t.usageTelemetry.LastReasoningTokens = reasoning
		t.usageTelemetry.LastCacheHitTokens = hit
		t.usageTelemetry.LastCacheMissTokens = miss
		t.usageTelemetry.LastEstimated = u.Estimated
	}
	if t.usageTelemetry.Sources == nil {
		t.usageTelemetry.Sources = map[string]usageSourceStats{}
	}
	src := t.usageTelemetry.Sources[source]
	src.PromptTokens += u.PromptTokens
	src.CompletionTokens += u.CompletionTokens
	src.TotalTokens += u.TotalTokens
	src.ReasoningTokens += u.ReasoningTokens
	src.CacheHitTokens += cacheHitTokens
	src.CacheMissTokens += cacheMissTokens
	src.CacheWriteTokens += u.CacheWriteTokens
	src.CacheWriteBilledTokens += u.CacheWriteBilledTokens
	src.Estimated = src.Estimated || u.Estimated
	src.RequestCount += requestCount

	q := e.CostQuote
	if q == nil && e.Pricing != nil {
		q = event.EnsureCostQuote(e, nil)
	}
	if q != nil {
		if t.usageTelemetry.CostLedger == nil {
			t.usageTelemetry.CostLedger = billing.NewLedger()
		}
		tokens := billing.UsageTokens{
			PromptTokens:           u.PromptTokens,
			CompletionTokens:       u.CompletionTokens,
			CacheHitTokens:         cacheHitTokens,
			CacheMissTokens:        cacheMissTokens,
			CacheWriteTokens:       u.CacheWriteTokens,
			CacheWriteBilledTokens: u.CacheWriteBilledTokens,
			Estimated:              u.Estimated,
		}
		t.usageTelemetry.CostLedger.Add(*q, tokens, time.Now().UTC())
		display := billing.NormalizeCurrency(t.runtimeCostDisplayCurrency)
		if display == "" {
			display = billing.NormalizeCurrency(t.usageTelemetry.SessionCurrency)
		}
		if display == "" && q.Selected != nil {
			display = billing.NormalizeCurrency(q.Selected.Currency)
		}
		if display == "" {
			display = billing.NormalizeCurrency(q.Original.Currency)
		}
		total := t.usageTelemetry.CostLedger.Total(display)
		if t.runtimeCostDisplayCurrency != "" {
			t.runtimeCostQuote = &total
		} else {
			t.usageTelemetry.SessionCostQuote = &total
			t.usageTelemetry.SessionCostComplete = total.Complete
		}
		if total.Selected != nil {
			if t.runtimeCostDisplayCurrency == "" {
				t.usageTelemetry.SessionCost = total.Selected.Float64()
				t.usageTelemetry.SessionCurrency = total.LegacyCurrencyCode()
				t.usageTelemetry.SessionCostUsd = t.usageTelemetry.SessionCost
			}
			src.SessionCost += q.LegacyCostFloat()
			src.SessionCostUsd = src.SessionCost
			src.SessionCurrency = total.LegacyCurrencySymbol()
		} else {

			if t.runtimeCostDisplayCurrency == "" {
				t.usageTelemetry.SessionCostComplete = false
				t.usageTelemetry.SessionCost = 0
				t.usageTelemetry.SessionCurrency = ""
				t.usageTelemetry.SessionCostUsd = 0
			}
			if q.Selected == nil {
				src.SessionCurrency = billing.CurrencySymbol(q.Original.Currency)
			}
		}
	}
	t.usageTelemetry.Sources[source] = src
	t.telemMu.Unlock()
}

func (a *App) repriceTabUsageForCurrentCurrency(tab *WorkspaceTab) {
	if a == nil || tab == nil {
		return
	}
	a.mu.RLock()
	root := tab.WorkspaceRoot
	a.mu.RUnlock()
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return
	}

	display := cfg.ExplicitDisplayCurrency()
	if !tab.selectDisplayCurrency(display) {
		return
	}
	if path := tab.currentSessionPath(); path != "" {
		_ = saveTelemetry(path+".telemetry.json", tab.telemetrySnapshot())
	}
}

func (t *WorkspaceTab) telemetrySnapshot() tabTelemetrySnapshot {
	t.telemMu.Lock()
	defer t.telemMu.Unlock()
	records := make([]readFileRecord, len(t.readTelemetry))
	copy(records, t.readTelemetry)
	usage := t.usageTelemetry
	if started := usage.activeTurnStartedAt; started > 0 {
		now := time.Now().UnixMilli()
		if now >= started {
			usage.ElapsedMs += now - started
		}
	}
	if len(t.usageTelemetry.Sources) > 0 {
		usage.Sources = make(map[string]usageSourceStats, len(t.usageTelemetry.Sources))
		maps.Copy(usage.Sources, t.usageTelemetry.Sources)
	}
	usage.activeTurnStartedAt = 0
	usage.sourceSessionCache = nil
	return tabTelemetrySnapshot{Version: 3, ReadFiles: records, Usage: usage}
}

// displayTelemetrySnapshot overlays the live wallet hint onto a copy used by
// UI reads. The persisted snapshot remains the occurrence-time/original view.
func (t *WorkspaceTab) displayTelemetrySnapshot() tabTelemetrySnapshot {
	snapshot := t.telemetrySnapshot()
	t.telemMu.Lock()
	quote := t.runtimeCostQuote
	t.telemMu.Unlock()
	if quote == nil {
		return snapshot
	}
	snapshot.Usage.SessionCostQuote = quote
	snapshot.Usage.SessionCostComplete = quote.Complete
	if quote.Selected != nil {
		snapshot.Usage.SessionCost = quote.Selected.Float64()
		snapshot.Usage.SessionCurrency = quote.LegacyCurrencyCode()
		snapshot.Usage.SessionCostUsd = snapshot.Usage.SessionCost
	} else {
		snapshot.Usage.SessionCostComplete = false
		snapshot.Usage.SessionCost = 0
		snapshot.Usage.SessionCurrency = ""
		snapshot.Usage.SessionCostUsd = 0
	}
	return snapshot
}

func (t *WorkspaceTab) resetTelemetry(sessionPath string) {
	t.telemMu.Lock()
	t.readTelemetry = nil
	t.usageTelemetry = sessionUsageStats{}
	t.runtimeCostDisplayCurrency = ""
	t.runtimeCostQuote = nil
	t.runtimeCostGeneration++
	t.telemetrySessionKey = sessionRuntimeKey(sessionPath)
	t.telemMu.Unlock()
}

// syncTelemetryToSession keys the in-memory telemetry to the runtime's current
// session. When the runtime rotated to a different session underneath the tab
// (typed /new routes through Controller.Submit and never reaches App.NewSession),
// the previous session's totals must not bleed into the new one: swap in the
// new session's persisted sidecar, or start from zero when none exists. The
// sidecar is rewritten on every recorded event, so a reload never loses more
// than the sub-second in-memory delta of an in-flight record.
func (t *WorkspaceTab) syncTelemetryToSession(sessionPath string) {
	key := sessionRuntimeKey(sessionPath)
	if key == "" {
		return
	}
	t.telemMu.Lock()
	same := t.telemetrySessionKey == key
	t.telemMu.Unlock()
	if same {
		return
	}

	snapshot := loadTelemetry(sessionPath + ".telemetry.json")
	t.telemMu.Lock()
	if t.telemetrySessionKey != key {
		t.readTelemetry = snapshot.ReadFiles
		t.usageTelemetry = snapshot.Usage
		t.runtimeCostDisplayCurrency = ""
		t.runtimeCostQuote = nil
		t.runtimeCostGeneration++
		t.telemetrySessionKey = key
	}
	t.telemMu.Unlock()
}

func (t *WorkspaceTab) resetDisplayTurn() {
	state := t.displayBufferState()
	state.mu.Lock()
	if len(state.planner.messages) == 0 {
		state.planner.tools = nil
	}
	if len(state.executor.messages) == 0 {
		state.executor.tools = nil
	}
	state.mu.Unlock()
}

func (t *WorkspaceTab) recordDisplayEvent(e event.Event) {
	state := t.displayBufferState()
	state.mu.Lock()
	defer state.mu.Unlock()
	buffer := &state.executor
	if strings.TrimSpace(e.Source) == event.UsageSourcePlanner {
		buffer = &state.planner
	}
	recordHistoryDisplayEvent(buffer, e)
}

func (t *WorkspaceTab) displayBufferState() *tabDisplayState {
	t.displayStateMu.Lock()
	defer t.displayStateMu.Unlock()
	if t.displayState == nil {
		t.displayState = &tabDisplayState{}
	}
	return t.displayState
}

func (t *WorkspaceTab) adoptDisplayState(state *tabDisplayState) {
	if t == nil || state == nil {
		return
	}
	t.displayStateMu.Lock()
	t.displayState = state
	t.displayStateMu.Unlock()
}

func recordHistoryDisplayEvent(buffer *displayTurnBuffer, e event.Event) {
	switch e.Kind {
	case event.Phase:
		if strings.TrimSpace(e.Text) != "" {
			buffer.messages = append(buffer.messages, &bufferedHistoryMessage{message: HistoryMessage{Role: "phase", Content: e.Text}})
		}
	case event.Reasoning:
		if e.Text != "" {
			hm := ensureDisplayAssistant(buffer)
			hm.reasoning.append(e.Text)
		}
	case event.Text:
		if e.Text != "" {
			hm := ensureDisplayAssistant(buffer)
			hm.content.append(e.Text)
		}
	case event.Message:
		if e.Text != "" || e.Reasoning != "" || len(e.MemoryCitations) > 0 {
			hm := ensureDisplayAssistant(buffer)
			if e.Text != "" {
				hm.content.replace(e.Text)
			}
			if e.Reasoning != "" {
				hm.reasoning.replace(e.Reasoning)
			}
			if len(e.MemoryCitations) > 0 {
				hm.message.MemoryCitations = append([]provider.MemoryCitation(nil), e.MemoryCitations...)
			}
		}
	case event.ToolDispatch:
		if e.Tool.Partial || strings.TrimSpace(e.Tool.Name) == "" {
			return
		}
		hm := ensureDisplayAssistantForTool(buffer)
		resolvedReadOnly := e.Tool.ReadOnly
		call := HistoryToolCall{
			ID:               e.Tool.ID,
			Name:             e.Tool.Name,
			Arguments:        e.Tool.Args,
			ResolvedName:     e.Tool.ResolvedName,
			CapabilityID:     e.Tool.CapabilityID,
			ResolvedReadOnly: &resolvedReadOnly,
			Subject:          historyToolSubject(e.Tool.Name, e.Tool.Args),
			Summary:          historyToolSummary(e.Tool.Name, e.Tool.Args, ""),
			Diff:             e.Tool.Diff,
			Added:            e.Tool.Added,
			Removed:          e.Tool.Removed,
		}
		replaced := false
		if call.ID != "" {
			for i := range hm.message.ToolCalls {
				if hm.message.ToolCalls[i].ID == call.ID {
					hm.message.ToolCalls[i] = call
					replaced = true
					break
				}
			}
			if buffer.tools == nil {
				buffer.tools = map[string]string{}
			}
			buffer.tools[call.ID] = call.Name
		}
		if !replaced {
			hm.message.ToolCalls = append(hm.message.ToolCalls, call)
		}
	case event.ToolResult:
		callID := strings.TrimSpace(e.Tool.ID)
		content := firstNonEmpty(e.Tool.Output, e.Tool.Err)
		display, errPreview := plannerToolResultDisplay(content, e.Tool.Err != "")
		if callID != "" {
			updateBufferedHistoryToolCallSummary(buffer.messages, callID, content)
		}
		toolName := e.Tool.Name
		if toolName == "" && buffer.tools != nil {
			toolName = buffer.tools[callID]
		}
		buffer.messages = append(buffer.messages, &bufferedHistoryMessage{message: HistoryMessage{
			Role:            "tool",
			ToolCallID:      callID,
			ToolName:        toolName,
			Content:         display,
			ToolResultError: errPreview,
		}})
	case event.Notice:
		if strings.TrimSpace(e.Text) != "" {
			level := "info"
			if e.Level == event.LevelWarn {
				level = "warn"
			}
			buffer.messages = append(buffer.messages, &bufferedHistoryMessage{message: HistoryMessage{
				Role:            "notice",
				Level:           level,
				Content:         e.Text,
				Detail:          e.Detail,
				Code:            e.Code,
				DecisionReceipt: cloneDecisionReceipt(e.DecisionReceipt),
			}})
		}
	}
}

func ensureDisplayAssistant(buffer *displayTurnBuffer) *bufferedHistoryMessage {
	if n := len(buffer.messages); n > 0 && buffer.messages[n-1].message.Role == "assistant" {
		return buffer.messages[n-1]
	}
	message := &bufferedHistoryMessage{message: HistoryMessage{Role: "assistant"}}
	buffer.messages = append(buffer.messages, message)
	return message
}

func ensureDisplayAssistantForTool(buffer *displayTurnBuffer) *bufferedHistoryMessage {
	if n := len(buffer.messages); n > 0 && buffer.messages[n-1].message.Role == "assistant" && !buffer.messages[n-1].content.hasNonWhitespace() {
		return buffer.messages[n-1]
	}
	message := &bufferedHistoryMessage{message: HistoryMessage{Role: "assistant"}}
	buffer.messages = append(buffer.messages, message)
	return message
}

func updateBufferedHistoryToolCallSummary(messages []*bufferedHistoryMessage, callID, output string) {
	if callID == "" {
		return
	}
	for _, v := range slices.Backward(messages) {
		for j := range v.message.ToolCalls {
			call := &v.message.ToolCalls[j]
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

func plannerToolResultDisplay(content string, failed bool) (display, errPreview string) {
	if strings.TrimSpace(content) == "" {
		return "", ""
	}
	if failed || historyToolResultFailed(content) {
		display = clipHistoryToolPreview(strings.TrimSpace(content))
		return display, display
	}
	return "", ""
}

func (t *WorkspaceTab) takeDisplayTurn(cancelled bool) []HistoryMessage {
	state := t.displayBufferState()
	state.mu.Lock()
	defer state.mu.Unlock()
	out := state.planner.materialize()
	if cancelled {
		out = append(out, state.executor.materialize()...)
		if len(out) > 0 {
			out = append(out, HistoryMessage{
				Role:    "notice",
				Level:   "info",
				Code:    event.NoticeCodeCancelledTurn,
				Content: "This turn was interrupted. Partial output is kept for reference; only completed tool pairs and a bounded recovery summary enter the next model turn. Inspect the workspace before continuing or reverting changes.",
			})
		}
	}
	state.planner.reset()
	state.executor.reset()
	return out
}

func enqueuePendingDisplayWrite(state *tabDisplayState, write *pendingDisplayWrite) {
	if state == nil || write == nil || write.persist == nil {
		return
	}
	state.mu.Lock()
	state.pendingWrites = append(state.pendingWrites, write)
	if state.persistRunning {
		state.mu.Unlock()
		return
	}
	state.persistRunning = true
	state.mu.Unlock()
	go retryPendingDisplayWrites(state)
}

func persistOrEnqueueDisplayWrite(state *tabDisplayState, write *pendingDisplayWrite) {
	if state == nil || write == nil || write.persist == nil {
		return
	}
	state.mu.Lock()
	hasPending := len(state.pendingWrites) > 0
	state.mu.Unlock()
	if hasPending {
		enqueuePendingDisplayWrite(state, write)
		return
	}
	if err := write.persist(write.dir, write.sessionPath, write.userContent, write.messages); err != nil {
		slog.Warn("desktop: persist display-only turn history; queued for retry", "err", err)
		enqueuePendingDisplayWrite(state, write)
	}
}

func retryPendingDisplayWrites(state *tabDisplayState) {
	failures := 0
	for {
		state.mu.Lock()
		if len(state.pendingWrites) == 0 {
			state.persistRunning = false
			state.mu.Unlock()
			return
		}
		write := state.pendingWrites[0]
		state.mu.Unlock()

		if failures > 0 {
			time.Sleep(time.Duration(failures*failures) * 50 * time.Millisecond)
		}
		if err := write.persist(write.dir, write.sessionPath, write.userContent, write.messages); err != nil {
			failures++
			if failures < displayPersistRetryLimit {
				continue
			}
			state.mu.Lock()
			state.persistRunning = false
			state.mu.Unlock()
			slog.Warn("desktop: display-only turn history remains pending after retries", "err", err)
			return
		}

		state.mu.Lock()
		if len(state.pendingWrites) > 0 && state.pendingWrites[0] == write {
			state.pendingWrites[0] = nil
			state.pendingWrites = state.pendingWrites[1:]
		}
		state.mu.Unlock()
		failures = 0
	}
}

func (s *tabEventSink) recordTurnStarted() {
	tab, sp := s.telemetryTab()
	if tab == nil {
		return
	}
	if sp != "" {
		tab.syncTelemetryToSession(sp)
	}
	tab.recordTurnStarted(time.Now().UnixMilli())
	if sp != "" {
		_ = saveTelemetry(sp+".telemetry.json", tab.telemetrySnapshot())
	}
}

func (s *tabEventSink) recordTurnDone() {
	tab, sp := s.telemetryTab()
	if tab == nil {
		return
	}
	if sp != "" {
		tab.syncTelemetryToSession(sp)
	}
	tab.recordTurnDone(time.Now().UnixMilli())
	if sp != "" {
		_ = saveTelemetry(sp+".telemetry.json", tab.telemetrySnapshot())
	}
}

func (s *tabEventSink) resetDisplayTurn() {
	tab, _ := s.eventTabAndController()
	if tab != nil {
		tab.resetDisplayTurn()
	}
}
