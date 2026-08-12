package openai

import (
	"encoding/json"
	"maps"
	"strings"
)

type chatRequest struct {
	Model               string         `json:"model"`
	Messages            []chatMessage  `json:"messages"`
	Tools               []chatTool     `json:"tools,omitempty"`
	Stream              bool           `json:"stream"`
	StreamOptions       *streamOptions `json:"stream_options,omitempty"`
	Temperature         *float64       `json:"temperature,omitempty"`
	MaxTokens           int            `json:"max_tokens,omitempty"`
	MaxCompletionTokens int            `json:"max_completion_tokens,omitempty"`
	ReasoningEffort     string         `json:"reasoning_effort,omitempty"`
	Thinking            *thinkingMode  `json:"thinking,omitempty"`
	ExtraBody           map[string]any `json:"-"`
}

func omitExtraBodyFields(in map[string]any, names ...string) map[string]any {
	if len(in) == 0 {
		return nil
	}
	omit := make(map[string]struct{}, len(names))
	for _, name := range names {
		omit[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	out := make(map[string]any, len(in))
	for name, value := range in {
		if _, blocked := omit[strings.ToLower(strings.TrimSpace(name))]; !blocked {
			out[name] = value
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func (r chatRequest) MarshalJSON() ([]byte, error) {
	type wire chatRequest
	baseReq := wire(r)
	baseReq.ExtraBody = nil
	raw, err := json.Marshal(baseReq)
	if err != nil {
		return nil, err
	}
	if len(r.ExtraBody) == 0 {
		return raw, nil
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, err
	}
	maps.Copy(body, cleanExtraBody(r.ExtraBody))
	return json.Marshal(body)
}

type thinkingMode struct {
	Type string `json:"type"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role string `json:"role"`
	// content is always present (never omitted): DeepSeek's strict deserializer
	// rejects a message missing the field. A pure tool_calls assistant turn
	// serializes as null (nil here); a string for every other text message
	// (empty included — null is rejected by some backends for a tool message);
	// and a []chatContentPart array for a vision user turn carrying images.
	Content any `json:"content"`
	// Prefix is wire-only and is set exclusively on an automatically recovered
	// DeepSeek assistant tail. omitempty keeps every ordinary request byte-stable.
	Prefix bool `json:"prefix,omitempty"`
	// A pointer so the field can serialize as an empty string: DeepSeek thinking
	// mode requires the reasoning_content key to be PRESENT on assistant
	// tool_calls turns (an empty value passes; a missing key 400s), while every
	// other message must keep omitting it.
	ReasoningContent *string        `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
	// Name is the role=tool message's function name. A pointer so ordinary
	// messages omit the key (byte-stable prefix), while tool messages always
	// serialize it — even empty: strict OpenAI-compatible backends (MiMo, per
	// its error table) reject a tool message whose `name` key is absent
	// ("name is not set"), and OpenAI's spec requires the field on role=tool.
	Name *string `json:"name,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatImageURL struct {
	URL    string `json:"url"`
	Detail string `json:"detail,omitempty"`
}

func imageContentParts(text string, images []string, detail string) []chatContentPart {
	parts := make([]chatContentPart, 0, len(images)+1)
	if text != "" {
		parts = append(parts, chatContentPart{Type: "text", Text: text})
	}
	for _, url := range images {
		parts = append(parts, chatContentPart{Type: "image_url", ImageURL: &chatImageURL{URL: url, Detail: detail}})
	}
	return parts
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type chatToolCall struct {
	Index        int                       `json:"index,omitempty"`
	ID           string                    `json:"id,omitempty"`
	Type         string                    `json:"type,omitempty"`
	ExtraContent *chatToolCallExtraContent `json:"extra_content,omitempty"`
	Function     struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		// Decode compatibility for the early Gemini OpenAI shape. New requests
		// use extra_content.google.thought_signature.
		ThoughtSignature string `json:"thought_signature,omitempty"`
	} `json:"function"`
}

type chatToolCallExtraContent struct {
	Google struct {
		ThoughtSignature string `json:"thought_signature,omitempty"`
	} `json:"google"`
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string         `json:"content"`
			ReasoningContent string         `json:"reasoning_content"`
			Reasoning        string         `json:"reasoning"`
			ToolCalls        []chatToolCall `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *wireUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// wireUsage covers DeepSeek's top-level cache fields, OpenAI/MiMo's nested
// details, and Anthropic-style fallbacks returned by compatible gateways.
type wireUsage struct {
	PromptTokens             int `json:"prompt_tokens"`
	CompletionTokens         int `json:"completion_tokens"`
	TotalTokens              int `json:"total_tokens"`
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	PromptCacheHitTokens     int `json:"prompt_cache_hit_tokens"`
	PromptCacheMissTokens    int `json:"prompt_cache_miss_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	PromptTokensDetails      *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}
