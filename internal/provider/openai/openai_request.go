package openai

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"reasonix/internal/provider"
)

func (c *client) buildRequest(req provider.Request) chatRequest {

	src := provider.SanitizeToolPairing(req.Messages)
	msgs := make([]chatMessage, 0, len(src))
	// Images returned by tool calls can't ride in the tool message itself — the
	// OpenAI API accepts only text content parts under role "tool" — so they are
	// carried by a synthetic user message injected after the turn's full run of
	// tool results, before the next non-tool message (splitting a tool-result
	// run would break the API's tool-call pairing validation).
	var pendingToolImages []string
	flushToolImages := func() {
		if len(pendingToolImages) == 0 {
			return
		}
		msgs = append(msgs, chatMessage{
			Role:    "user",
			Content: imageContentParts("Images returned by the preceding tool call(s):", pendingToolImages, c.visionDetail),
		})
		pendingToolImages = nil
	}
	for _, m := range src {
		if m.Role != provider.RoleTool {
			flushToolImages()
		}
		cm := chatMessage{
			Role:       string(m.Role),
			ToolCallID: m.ToolCallID,
		}
		if m.Role == provider.RoleTool {

			name := m.Name
			cm.Name = &name
		}

		if m.Role == provider.RoleAssistant {
			switch {
			case c.kimiK3 && (m.ReasoningContent != "" || len(m.ToolCalls) > 0):

				cm.ReasoningContent = &m.ReasoningContent
			case c.deepseek && len(m.ToolCalls) > 0:
				if c.RequiresToolCallReasoning() || m.ReasoningContent != "" {
					cm.ReasoningContent = &m.ReasoningContent
				}
			case c.zhipu && m.ReasoningContent != "":

				cm.ReasoningContent = &m.ReasoningContent
			}
		}
		for _, tc := range m.ToolCalls {
			wire := chatToolCall{ID: tc.ID, Type: "function"}
			wire.Function.Name = tc.Name
			wire.Function.Arguments = tc.Arguments
			if tc.ThoughtSignature != "" && usesGeminiThoughtSignatures(c.baseURL, c.model) {

				wire.ExtraContent = &chatToolCallExtraContent{}
				wire.ExtraContent.Google.ThoughtSignature = tc.ThoughtSignature
			}
			cm.ToolCalls = append(cm.ToolCalls, wire)
		}
		switch {
		case c.vision && m.Role == provider.RoleUser && len(m.Images) > 0:
			cm.Content = imageContentParts(m.Content, m.Images, c.visionDetail)
		case m.Role != provider.RoleAssistant || len(cm.ToolCalls) == 0 || m.Content != "":
			cm.Content = m.Content
		}
		msgs = append(msgs, cm)
		if c.vision && m.Role == provider.RoleTool {
			pendingToolImages = append(pendingToolImages, m.Images...)
		}
	}
	flushToolImages()

	var tools []chatTool
	for _, t := range req.Tools {
		parameters := t.Parameters
		if len(parameters) == 0 {
			parameters = provider.CanonicalizeSchema(nil)
		}
		if c.mimo {
			parameters = provider.NormalizeLegacyTupleItemsForDraft202012(parameters)
		}
		tools = append(tools, chatTool{
			Type:     "function",
			Function: chatFunction{Name: t.Name, Description: t.Description, Parameters: parameters},
		})
	}

	maxOutputTokens := req.MaxTokens
	if maxOutputTokens == 0 {
		if c.autoMaxOutput && c.deepseek {

			effort := c.requestEffort(req)
			reasoningOn := c.thinkingType != "disabled" &&
				effort != "disabled" && effort != "off" && effort != "none"
			maxOutputTokens = provider.AutoOutputBudget(reasoningOn, effort)
		} else {
			maxOutputTokens = c.maxOutputTokens
		}
	}
	if maxOutputTokens < 0 {
		maxOutputTokens = 0
	}
	out := chatRequest{
		Model:           c.model,
		Messages:        msgs,
		Tools:           tools,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
		Temperature:     req.Temperature,
		MaxTokens:       maxOutputTokens,
		ReasoningEffort: kimiK3ReasoningEffort(c.kimiK3, c.requestEffort(req)),
		ExtraBody:       c.extraBody,
	}
	switch {
	case c.kimiK3:

		out.Temperature = nil
		out.MaxTokens = 0
		out.MaxCompletionTokens = maxOutputTokens
		out.ExtraBody = omitExtraBodyFields(out.ExtraBody,
			"temperature", "top_p", "n", "presence_penalty", "frequency_penalty", "max_completion_tokens")
	case IsOpenAI(c.baseURL):

		out.MaxTokens = 0
		out.MaxCompletionTokens = maxOutputTokens
	case c.deepseek:

		if c.thinkingType == "disabled" {
			out.Thinking = &thinkingMode{Type: "disabled"}
		} else {
			out.Thinking = &thinkingMode{Type: "enabled"}
		}
	case c.minimax:

		t := c.effort
		if t == "" {
			t = "adaptive"
		}
		out.Thinking = &thinkingMode{Type: t}
		out.ReasoningEffort = ""
	case c.zhipu:

		t := c.effort
		if t == "" {
			t = "enabled"
		}
		if c.thinkingType != "" {
			t = c.thinkingType
		}
		out.Thinking = &thinkingMode{Type: t}
		out.ReasoningEffort = ""
	case c.longcat:

		t := c.effort
		if t == "" {
			t = c.thinkingType
		}
		if t == "" {
			t = "enabled"
		}
		out.Thinking = &thinkingMode{Type: t}
		out.ReasoningEffort = ""
	case c.thinkingType != "":

		out.Thinking = &thinkingMode{Type: c.thinkingType}
	}
	return out
}

func (c *client) buildPrefixRequest(req provider.Request, content, reasoning string) chatRequest {
	out := c.buildRequest(req)
	prefix := chatMessage{Role: "assistant", Content: content, Prefix: true}
	if c.deepseek && c.thinkingType != "disabled" {
		prefix.ReasoningContent = &reasoning
	}
	out.Messages = append(out.Messages, prefix)
	return out
}

// readStream parses one SSE response into chunks: text deltas stream live,
// tool-call fragments accumulate by index and emit complete on [DONE], and a
// ChunkToolCallStart fires the moment a call's name is known. It returns whether
// any model output was forwarded (so the caller can decide a replay is safe) and
// the first fatal error — a nil error means the stream reached [DONE].
func (c *client) readStream(ctx context.Context, resp *http.Response, out chan<- provider.Chunk) (emitted bool, _ error) {
	defer resp.Body.Close()

	idleTimeout := c.idleTimeout
	if idleTimeout <= 0 {
		idleTimeout = defaultStreamIdleTimeout
	}
	done := make(chan struct{})
	defer close(done)
	activity := make(chan struct{}, 1)
	var stalled atomic.Bool
	go func() {
		idle := time.NewTimer(idleTimeout)
		defer idle.Stop()
		for {
			select {
			case <-ctx.Done():
				resp.Body.Close()
				return
			case <-idle.C:
				stalled.Store(true)
				resp.Body.Close()
				return
			case <-activity:
				if !idle.Stop() {
					select {
					case <-idle.C:
					default:
					}
				}
				idle.Reset(idleTimeout)
			case <-done:
				return
			}
		}
	}()

	acc := map[int]*provider.ToolCall{}
	started := map[int]bool{}
	argBucket := map[int]int{}
	var order []int
	var lastFinishReason string
	var sawDone bool
	var think thinkSplitter

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		select {
		case activity <- struct{}{}:
		default:
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			sawDone = true
			break
		}
		if data == "" {
			continue
		}

		var sr streamResponse
		if err := json.Unmarshal([]byte(data), &sr); err != nil {
			return emitted, provider.StreamDecodeError(c.name, data, err)
		}
		if sr.Error != nil {
			return emitted, fmt.Errorf("%s: %s", c.name, sr.Error.Message)
		}
		if len(sr.Choices) > 0 && sr.Choices[0].FinishReason != nil && *sr.Choices[0].FinishReason != "" {
			lastFinishReason = *sr.Choices[0].FinishReason
		}
		if sr.Usage != nil {
			u := normaliseUsage(sr.Usage)
			u.FinishReason = lastFinishReason
			provider.ApplyRequestAttemptCount(ctx, u)
			emitted = true
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkUsage, Usage: u}) {
				return emitted, ctx.Err()
			}
		}
		if len(sr.Choices) == 0 {
			continue
		}

		delta := sr.Choices[0].Delta
		reasoningDelta := delta.ReasoningContent
		if reasoningDelta == "" {
			reasoningDelta = delta.Reasoning
		}
		if reasoningDelta != "" {
			emitted = true
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, Text: reasoningDelta}) {
				return emitted, ctx.Err()
			}
		}
		if delta.Content != "" {
			r, txt := think.push(delta.Content)
			if r != "" {
				emitted = true
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, Text: r}) {
					return emitted, ctx.Err()
				}
			}
			if txt != "" {
				emitted = true
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkText, Text: txt}) {
					return emitted, ctx.Err()
				}
			}
		}
		for _, tc := range delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
				cur = &provider.ToolCall{}
				acc[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Function.Name != "" {
				cur.Name = tc.Function.Name
			}
			cur.Arguments += tc.Function.Arguments
			thoughtSignature := ""
			if tc.ExtraContent != nil {
				thoughtSignature = tc.ExtraContent.Google.ThoughtSignature
			}
			if thoughtSignature == "" {

				thoughtSignature = tc.Function.ThoughtSignature
			}
			if thoughtSignature != "" {
				cur.ThoughtSignature = thoughtSignature
			}

			if !started[tc.Index] && cur.Name != "" {
				started[tc.Index] = true
				emitted = true
				if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCallStart, ToolCall: &provider.ToolCall{ID: cur.ID, Name: cur.Name}}) {
					return emitted, ctx.Err()
				}
			}

			if started[tc.Index] {
				if bucket := len(cur.Arguments) / 2048; bucket > argBucket[tc.Index] {
					argBucket[tc.Index] = bucket
					emitted = true
					if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCallArgsDelta, ToolCall: &provider.ToolCall{ID: cur.ID, Name: cur.Name}, ArgChars: len(cur.Arguments)}) {
						return emitted, ctx.Err()
					}
				}
			}
		}
	}

	if err := ctx.Err(); err != nil {
		return emitted, err
	}
	if stalled.Load() {

		return emitted, fmt.Errorf("%s: stream stalled — no data for %s, connection likely dropped: %w", c.name, idleTimeout, io.ErrUnexpectedEOF)
	}
	if err := scanner.Err(); err != nil {
		return emitted, fmt.Errorf("%s: read stream: %w", c.name, err)
	}

	if !sawDone && lastFinishReason == "" {
		return emitted, fmt.Errorf("%s: stream ended before completion: %w", c.name, io.ErrUnexpectedEOF)
	}

	if r, txt := think.flush(); r != "" || txt != "" {
		if r != "" {
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkReasoning, Text: r}) {
				return emitted, ctx.Err()
			}
		}
		if txt != "" {
			if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkText, Text: txt}) {
				return emitted, ctx.Err()
			}
		}
	}

	sort.Ints(order)
	for _, idx := range order {
		tc := acc[idx]
		if tc.ID == "" {

			tc.ID = fmt.Sprintf("call_%d", idx)
		}
		if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkToolCall, ToolCall: tc}) {
			return emitted, ctx.Err()
		}
	}
	if !sendChunk(ctx, out, provider.Chunk{Type: provider.ChunkDone}) {
		return emitted, ctx.Err()
	}
	return emitted, nil
}

// normaliseUsage folds the cache shapes used by OpenAI-compatible providers into
// a single Usage. DeepSeek reports prompt_cache_{hit,miss}_tokens at the top of
// usage; OpenAI and MiMo put cache hits under prompt_tokens_details; some
// compatible gateways return Anthropic-style input/cache counters instead.
// Reasoning tokens land in completion_tokens_details on thinking-mode models.
func normaliseUsage(u *wireUsage) *provider.Usage {
	prompt := u.PromptTokens
	anthropicPrompt := prompt == 0 &&
		(u.InputTokens != 0 || u.CacheCreationInputTokens != 0 || u.CacheReadInputTokens != 0)
	if anthropicPrompt {

		prompt = u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	}
	completion := u.CompletionTokens
	if completion == 0 {
		completion = u.OutputTokens
	}
	total := u.TotalTokens
	if total == 0 && (prompt != 0 || completion != 0) {
		total = prompt + completion
	}

	hit := u.PromptCacheHitTokens
	miss := u.PromptCacheMissTokens
	if hit == 0 && u.PromptTokensDetails != nil {
		hit = u.PromptTokensDetails.CachedTokens
	}
	if hit == 0 {
		hit = u.CacheReadInputTokens
	}
	if miss == 0 {
		switch {
		case anthropicPrompt:

			miss = u.InputTokens + u.CacheCreationInputTokens
		case hit > 0 && prompt > hit:
			miss = prompt - hit
		}
	}
	reasoning := 0
	if u.CompletionTokensDetails != nil {
		reasoning = u.CompletionTokensDetails.ReasoningTokens
	}
	return &provider.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
		CacheHitTokens:   hit,
		CacheMissTokens:  miss,
		ReasoningTokens:  reasoning,
	}
}
