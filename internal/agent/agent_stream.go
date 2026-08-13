package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"reasonix/internal/event"
	"reasonix/internal/provider"
)

// streamWithFrozen runs one completion, emitting reasoning and text deltas
// as typed events and collecting complete tool calls. A Message event closes
// the text stream so a sink can re-render the streamed raw text as styled
// markdown. When frozen is non-nil the request is not rebuilt from session —
// retries must replay the same provider-visible body.
func (a *Agent) streamWithFrozen(ctx context.Context, turn int, sink event.Sink, frozen *samplingRequest, attemptID string) streamedTurn {
	ctx = provider.WithRetryNotify(ctx, func(info provider.RetryInfo) {
		sink.Emit(event.Event{Kind: event.Retrying, RetryAttempt: info.Attempt, RetryMax: info.Max, RetryScope: event.RetryScopeHeaders})
	})

	ctx = provider.WithRequestAttemptCounter(ctx)

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var req provider.Request
	var err error
	if frozen != nil {
		req = freezeProviderRequest(frozen.req)
	} else {
		prepared, perr := a.prepareSamplingRequest(ctx)
		if perr != nil {
			return streamedTurn{err: perr}
		}
		req = prepared.req
	}

	defer trackPublishedHostStream(ctx, cancel)()
	ch, err := a.streamProviderRequest(ctx, req)
	if err != nil {
		return streamedTurn{usage: provider.UsageWithRequestAttemptCount(ctx, nil), err: err}
	}

	transformReasoning := a.svc.hooks != nil && a.svc.hooks.HasPostLLMCall()

	var text, reasoning strings.Builder
	var signature string                    // provider-issued proof for the reasoning (Anthropic thinking)
	var reasoningID, reasoningStatus string // Responses reasoning item id/status (meta chunk)
	var calls []provider.ToolCall
	var responsesItems []json.RawMessage
	var partialCalls []provider.ToolCall
	var usage *provider.Usage
	var partialToolStarted bool
	var maxArgChars int
	var lastArgProgress time.Time

	collect := func(stored string, err error) streamedTurn {
		return streamedTurn{
			text: text.String(), reasoning: stored, signature: signature,
			reasoningID: reasoningID, reasoningStatus: reasoningStatus,
			calls: calls, responsesItems: responsesItems, usage: usage,
			partialToolStarted: partialToolStarted, partialCalls: partialCalls,
			maxArgChars: maxArgChars, err: err,
		}
	}
	finishReasoning := func() (stored, display string) {
		original := reasoning.String()
		display = original
		if transformReasoning && original != "" {
			display = a.svc.hooks.PostLLMCall(ctx, original, turn)
			if display != "" {
				sink.Emit(event.Event{Kind: event.Reasoning, Text: display})
			}
		}
		stored = display
		providerBound := signature != "" || reasoningID != "" || reasoningStatus != ""
		if providerBound || provider.RequiresReasoningRoundTrip(a.svc.prov) || (len(calls) > 0 && provider.RequiresToolCallReasoning(a.svc.prov)) {
			stored = original
		}
		return stored, display
	}
	for {
		var chunk provider.Chunk
		select {
		case <-ctx.Done():
			stored, _ := finishReasoning()
			usage = bestEffortStreamUsage(usage, text.Len(), reasoning.Len(), "interrupted")
			usage = provider.UsageWithRequestAttemptCount(ctx, usage)
			return collect(stored, ctx.Err())
		case c, ok := <-ch:
			if !ok {
				if err := ctx.Err(); err != nil {
					stored, _ := finishReasoning()
					usage = bestEffortStreamUsage(usage, text.Len(), reasoning.Len(), "interrupted")
					usage = provider.UsageWithRequestAttemptCount(ctx, usage)
					return collect(stored, err)
				}
				stored, display := finishReasoning()

				providerSignature := signature
				finalText, finalReasoning, signature, calls, usage, err := a.interceptProviderResponse(
					ctx, text.String(), stored, signature, calls, usage)
				if err != nil {
					return streamedTurn{partialToolStarted: partialToolStarted, partialCalls: partialCalls, maxArgChars: maxArgChars, err: err}
				}

				if finalReasoning != stored || signature != providerSignature {
					reasoningID, reasoningStatus = "", ""
				}
				if finalReasoning != stored {

					display = finalReasoning
				}
				if finalText != "" || display != "" {
					sink.Emit(event.Event{
						Kind:      event.Message,
						Text:      DisplayAssistantText(finalText),
						Reasoning: display,
					})
				}
				usage = provider.UsageWithRequestAttemptCount(ctx, usage)

				return streamedTurn{
					text: finalText, reasoning: finalReasoning, signature: signature,
					reasoningID: reasoningID, reasoningStatus: reasoningStatus,
					calls: calls, responsesItems: responsesItems, usage: usage,
					partialCalls: partialCalls, maxArgChars: maxArgChars,
				}
			}
			chunk = c
		}
		switch chunk.Type {
		case provider.ChunkReasoning:
			reasoning.WriteString(chunk.Text)
			if chunk.Signature != "" {
				signature = chunk.Signature
			}

			if chunk.ReasoningID != "" {
				reasoningID = chunk.ReasoningID
			}
			if chunk.ReasoningStatus != "" {
				reasoningStatus = chunk.ReasoningStatus
			}
			if chunk.Text != "" && !transformReasoning {
				sink.Emit(event.Event{Kind: event.Reasoning, Text: chunk.Text})
			}
			if a.reasoningByteLimit > 0 && reasoning.Len() > a.reasoningByteLimit {
				stored, _ := finishReasoning()
				usage = bestEffortStreamUsage(usage, text.Len(), reasoning.Len(), finishReasonClientReasoningLimit)
				usage = provider.UsageWithRequestAttemptCount(ctx, usage)
				a.storeLatestRequestUsage(usage)
				return collect(stored, errReasoningByteLimitExceeded)
			}
		case provider.ChunkText:
			text.WriteString(chunk.Text)
			sink.Emit(event.Event{Kind: event.Text, Text: chunk.Text})
		case provider.ChunkToolCallStart:
			partialToolStarted = true

			if tc := chunk.ToolCall; tc != nil {
				partialCalls = upsertPartialToolCall(partialCalls, *tc)
				sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: event.Tool{
					ID: tc.ID, Name: tc.Name, ReadOnly: a.toolReadOnly(tc.Name), Partial: true, AttemptID: attemptID,
				}})
			}
		case provider.ChunkToolCallArgsDelta:
			partialToolStarted = true

			if chunk.ArgChars > maxArgChars {
				maxArgChars = chunk.ArgChars
			}
			if tc := chunk.ToolCall; tc != nil && time.Since(lastArgProgress) >= 250*time.Millisecond {
				partialCalls = upsertPartialToolCall(partialCalls, *tc)
				lastArgProgress = time.Now()
				sink.Emit(event.Event{Kind: event.ToolDispatch, Tool: event.Tool{
					ID: tc.ID, Name: tc.Name, ReadOnly: a.toolReadOnly(tc.Name), Partial: true, ArgChars: chunk.ArgChars, AttemptID: attemptID,
				}})
			}
		case provider.ChunkToolCall:
			partialToolStarted = true
			if chunk.ToolCall != nil {
				calls = append(calls, *chunk.ToolCall)
				partialCalls = upsertPartialToolCall(partialCalls, *chunk.ToolCall)
				if n := len(chunk.ToolCall.Arguments); n > maxArgChars {
					maxArgChars = n
				}
			}
		case provider.ChunkResponsesItem:
			if len(chunk.ResponsesItem) > 0 {
				responsesItems = append(responsesItems, append(json.RawMessage(nil), chunk.ResponsesItem...))
			}
		case provider.ChunkUsage:
			usage, a.turn.lastReasoning = chunk.Usage, chunk.Usage.ReasoningTokens
			a.storeLatestRequestUsage(chunk.Usage)
			a.sess.cacheHit.Add(int64(chunk.Usage.CacheHitTokens))
			a.sess.cacheMiss.Add(int64(chunk.Usage.CacheMissTokens))
		case provider.ChunkError:
			if provider.IsStreamInterrupted(chunk.Err) {
				stored, _ := finishReasoning()
				usage = bestEffortStreamUsage(usage, text.Len(), reasoning.Len(), "interrupted")
				usage = provider.UsageWithRequestAttemptCount(ctx, usage)
				st := collect(stored, chunk.Err)
				st.interrupted = true
				return st
			}
			stored, _ := finishReasoning()
			if errors.Is(chunk.Err, context.Canceled) || errors.Is(chunk.Err, context.DeadlineExceeded) {
				usage = bestEffortStreamUsage(usage, text.Len(), reasoning.Len(), "interrupted")
			}
			usage = provider.UsageWithRequestAttemptCount(ctx, usage)
			return collect(stored, chunk.Err)
		}
	}
}

func bestEffortStreamUsage(current *provider.Usage, textBytes, reasoningBytes int, finishReason string) *provider.Usage {
	if current == nil && textBytes == 0 && reasoningBytes == 0 {
		return nil
	}
	var usage provider.Usage
	if current != nil {
		usage = *current
	}
	if finishReason != "" {
		usage.FinishReason = finishReason
	}
	reasoningTokens := estimateTokensFromBytes(reasoningBytes)
	textTokens := estimateTokensFromBytes(textBytes)
	completionTokens := reasoningTokens + textTokens
	if usage.ReasoningTokens < reasoningTokens {
		usage.ReasoningTokens = reasoningTokens
		usage.Estimated = true
	}
	if usage.CompletionTokens < completionTokens {
		usage.CompletionTokens = completionTokens
		usage.Estimated = true
	}
	if minTotal := usage.PromptTokens + usage.CompletionTokens; usage.TotalTokens < minTotal {
		usage.TotalTokens = minTotal
		usage.Estimated = true
	}
	return &usage
}

func estimateTokensFromBytes(n int) int {
	if n <= 0 {
		return 0
	}
	tokens := n / 4
	if n%4 != 0 {
		tokens++
	}
	if tokens <= 0 {
		return 1
	}
	return tokens
}

func upsertPartialToolCall(calls []provider.ToolCall, call provider.ToolCall) []provider.ToolCall {
	for i := range calls {
		if call.ID != "" && calls[i].ID == call.ID {
			calls[i] = call
			return calls
		}
	}
	return append(calls, call)
}

func (a *Agent) recordInterruptedDisplay(text, reasoning string, calls []provider.ToolCall, pending bool, workDurationMs int64) {
	displayCalls := make([]provider.ToolCall, 0, len(calls))
	interrupted := make([]string, 0, len(calls))
	seen := make(map[string]struct{}, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Name)
		key := call.ID + "\x00" + name
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		displayCalls = append(displayCalls, provider.ToolCall{ID: call.ID, Name: name})
		if name != "" {
			interrupted = append(interrupted, name)
		}
	}
	a.sess.conversation.Add(provider.Message{
		Role:             provider.RoleTool,
		Content:          text,
		ReasoningContent: reasoning,
		ToolCalls:        displayCalls,
		ToolCallID:       provider.LocalOnlyToolID,
		Name:             provider.LocalOnlyToolName,
		WorkDurationMs:   workDurationMs,
		LocalOnly:        true,
		InterruptedTurn: &provider.InterruptedTurnRecovery{
			Pending:                 pending,
			InterruptedTools:        interrupted,
			DroppedPartialText:      strings.TrimSpace(text) != "",
			DroppedPartialReasoning: strings.TrimSpace(reasoning) != "",
		},
	})
}

func (a *Agent) capturePrefixShape(schemas []provider.ToolSchema) PrefixShape {
	return CaptureShape(a.systemPrompt(), schemas, a.sess.conversation.RewriteVersion())
}

func (a *Agent) systemPrompt() string {
	var b strings.Builder
	for _, m := range a.sess.conversation.Messages {
		if m.Role != provider.RoleSystem {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(m.Content)
	}
	return b.String()
}
