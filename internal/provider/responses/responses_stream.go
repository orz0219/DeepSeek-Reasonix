package responses

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/provider"
)

type streamedCall struct {
	id, name, arguments string
	argChars            int
	completed           bool
}

func (c *client) readStream(ctx context.Context, resp *http.Response, out chan<- provider.Chunk, requestMessages []provider.Message) {
	defer resp.Body.Close()
	defer close(out)

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	idle := c.idleTimeout
	if idle <= 0 {
		idle = defaultStreamIdleTimeout
	}
	watchDone := make(chan struct{})
	activity := make(chan struct{}, 1)
	var stalled atomic.Bool
	go func() {
		timer := time.NewTimer(idle)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = resp.Body.Close()
				return
			case <-watchDone:
				return
			case <-activity:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(idle)
			case <-timer.C:
				stalled.Store(true)
				_ = resp.Body.Close()
				return
			}
		}
	}()
	defer close(watchDone)

	calls := make(map[string]*streamedCall)
	callOrder := make([]string, 0)
	callForItem := func(itemID string) *streamedCall {
		if call := calls[itemID]; call != nil {
			return call
		}
		call := &streamedCall{id: itemID}
		calls[itemID] = call
		callOrder = append(callOrder, itemID)
		return call
	}
	textDeltas := make(map[string]bool)
	reasoningDeltas := make(map[string]bool)
	seenSearchItems := make(map[string]struct{})
	var responsesItems []json.RawMessage
	var text, reasoning strings.Builder
	reasoningID := ""
	reasoningStatus := ""
	terminal := false
	failed := false
	completedResponseID := ""

	for scanner.Scan() {
		select {
		case activity <- struct{}{}:
		default:
		}
		line := scanner.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			terminal = true
			break
		}
		var event sseEvent
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		key := fmt.Sprintf("%s:%d", event.ItemID, event.ContentIndex)
		switch event.Type {
		case "response.output_text.delta":
			textDeltas[key] = true
			text.WriteString(event.Delta)
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkText, Text: event.Delta}) {
				return
			}
		case "response.output_text.done":
			if event.Text != "" && !textDeltas[key] {
				text.WriteString(event.Text)
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkText, Text: event.Text}) {
					return
				}
			}
		case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
			reasoningDeltas[key] = true
			reasoning.WriteString(event.Delta)
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, Text: event.Delta}) {
				return
			}
		case "response.reasoning_text.done", "response.reasoning_summary_text.done":
			if event.Text != "" && !reasoningDeltas[key] {
				reasoning.WriteString(event.Text)
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, Text: event.Text}) {
					return
				}
			}
		case "response.output_item.added":
			if event.Item != nil {
				switch event.Item.Type {
				case "function_call":
					call := callForItem(event.Item.ID)
					call.id = event.Item.CallID
					call.name = event.Item.Name
					if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &provider.ToolCall{ID: call.id, Name: call.name}}) {
						return
					}
				case "reasoning":

					if event.Item.ID != "" {

						reasoningID = event.Item.ID
					}
				}
			}
		case "response.function_call_arguments.delta":
			call := callForItem(event.ItemID)
			call.arguments += event.Delta
			call.argChars += len(event.Delta)
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCallArgsDelta, ToolCall: &provider.ToolCall{ID: call.id, Name: call.name}, ArgChars: call.argChars}) {
				return
			}
		case "response.function_call_arguments.done":
			call := callForItem(event.ItemID)
			if event.Arguments != "" {
				call.arguments = event.Arguments
			}
			if !call.completed {
				call.completed = true
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCall, ToolCall: &provider.ToolCall{ID: call.id, Name: call.name, Arguments: call.arguments}}) {
					return
				}
			}
		case "response.output_item.done":
			if event.Item != nil && event.Item.Type == "web_search_call" && c.webSearch {
				if _, ok := decodeReplayableWebSearchItem(event.Item.Raw); ok {
					key := event.Item.ID
					if key == "" {
						key = string(event.Item.Raw)
					}
					if _, seen := seenSearchItems[key]; !seen {
						seenSearchItems[key] = struct{}{}
						raw := append(json.RawMessage(nil), event.Item.Raw...)
						responsesItems = append(responsesItems, raw)
						if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkResponsesItem, ResponsesItem: raw}) {
							return
						}
					}
				}
			}
			if event.Item != nil {
				switch event.Item.Type {
				case "function_call":
					call := callForItem(event.Item.ID)
					if event.Item.CallID != "" {
						call.id = event.Item.CallID
					}
					if event.Item.Name != "" {
						call.name = event.Item.Name
					}
					if event.Item.Arguments != "" {
						call.arguments = event.Item.Arguments
					}
					if !call.completed {
						call.completed = true
						if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCall, ToolCall: &provider.ToolCall{ID: call.id, Name: call.name, Arguments: call.arguments}}) {
							return
						}
					}
				case "reasoning":

					if event.Item.Status != "" {
						reasoningStatus = event.Item.Status
					}
				}
			}
		case "response.completed", "response.incomplete", "response.failed":
			terminal = true
			if event.Response != nil {
				if event.Type == "response.completed" {
					completedResponseID = event.Response.ID
				}
				usage := usageFromResponse(event.Response)
				provider.ApplyRequestAttemptCount(ctx, usage)
				if event.Type == "response.incomplete" {
					switch event.Response.IncompleteDetails.Reason {
					case "max_output_tokens":
						usage.FinishReason = "length"
					case "content_filter":
						usage.FinishReason = "content_filter"
					default:
						usage.FinishReason = "incomplete"
					}
				} else if event.Type == "response.completed" && usage.FinishReason == "" {

					usage.FinishReason = "stop"
				}

				if usage.TotalTokens > 0 || usage.FinishReason != "" {

					if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkUsage, Usage: usage}) {
						return
					}
				}
			}
			if event.Type == "response.failed" {
				failed = true
				err := fmt.Errorf("responses: response failed")
				if event.Response != nil && event.Response.Error != nil {
					if authErr := authErrorFromResponse(c, event.Response.Error); authErr != nil {
						err = authErr
					} else {
						err = fmt.Errorf("responses: %s", event.Response.Error.Message)
					}
				}
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkError, Err: err}) {
					return
				}
			}
		}
		if terminal {
			break
		}
	}

	if ctx.Err() != nil {
		return
	}
	if err := scanner.Err(); err != nil {
		var reason string
		if stalled.Load() {
			err = fmt.Errorf("responses: stream idle timeout after %s", idle)
			reason = provider.StreamInterruptIdleTimeout
		} else {
			reason = provider.ClassifyStreamInterrupt(err)
		}
		_ = sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkError, Err: provider.StreamInterrupt(err, reason)})
		return
	}

	if !terminal {
		_ = sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkError, Err: provider.StreamInterrupt(io.ErrUnexpectedEOF, provider.StreamInterruptPrematureEOF)})
		return
	}
	if completedResponseID != "" {
		assistant := provider.Message{Role: provider.RoleAssistant, Content: text.String(), ReasoningContent: reasoning.String(), ReasoningID: reasoningID, ReasoningStatus: reasoningStatus, ResponsesItems: responsesItems}
		for _, itemID := range callOrder {
			call := calls[itemID]
			if call.completed {
				assistant.ToolCalls = append(assistant.ToolCalls, provider.ToolCall{ID: call.id, Name: call.name, Arguments: call.arguments})
			}
		}
		expected := append(append([]provider.Message(nil), requestMessages...), assistant)
		c.mu.Lock()
		c.lastResponseID = completedResponseID
		c.expectedPrefixDigest = c.conversationDigest(expected)
		c.mu.Unlock()
	} else {
		c.ResetContext()
	}
	if !failed {

		if reasoningID != "" || reasoningStatus != "" {
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, ReasoningID: reasoningID, ReasoningStatus: reasoningStatus}) {
				return
			}
		}
		_ = sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkDone})
	}
}

func sendChunk(ctx context.Context, out chan<- provider.Chunk, chunk provider.Chunk) bool {
	select {
	case out <- chunk:
		return true
	default:
	}
	notifySendChunkEnterBlocking()
	select {
	case out <- chunk:
		return true
	case <-ctx.Done():
		return false
	}
}

func usageFromResponse(response *sseResponse) *provider.Usage {
	usage := &provider.Usage{}
	if response == nil || response.Usage == nil {
		return usage
	}
	u := response.Usage
	cached, reasoning := 0, 0
	if u.InputTokensDetails != nil {
		cached = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil {
		reasoning = u.OutputTokensDetails.ReasoningTokens
	}
	miss := max(u.InputTokens-cached, 0)
	total := u.TotalTokens
	if total == 0 {
		total = u.InputTokens + u.OutputTokens
	}
	return &provider.Usage{PromptTokens: u.InputTokens, CompletionTokens: u.OutputTokens, TotalTokens: total, CacheHitTokens: cached, CacheMissTokens: miss, ReasoningTokens: reasoning}
}

func authErrorFromResponse(c *client, responseError *sseError) error {
	if responseError == nil {
		return nil
	}
	value := strings.ToLower(responseError.Code + " " + responseError.Message)
	if !strings.Contains(value, "auth") && !strings.Contains(value, "api key") && !strings.Contains(value, "unauthorized") && !strings.Contains(value, "forbidden") && !strings.Contains(value, "permission") {
		return nil
	}
	status := http.StatusUnauthorized
	if strings.Contains(value, "forbidden") || strings.Contains(value, "permission") {
		status = http.StatusForbidden
	}
	return &provider.AuthError{Provider: c.name, KeyEnv: c.keyEnv, KeySource: c.keySource, Status: status, HasKey: c.apiKey != "", Body: responseError.Message}
}
