package anthropic

import (
	"encoding/json"
	"fmt"
	"strings"

	"reasonix/internal/provider"
)

// webSearchResult is a single result from a web_search_tool_result block.
type webSearchResult struct {
	URL      string `json:"url"`
	Title    string `json:"title"`
	Text     string `json:"text"`
	SiteName string `json:"site_name"`
}

// formatWebSearchResults parses a web_search_tool_result content array
// and formats titles and URLs as human-readable text. DeepSeek returns
// encrypted_content rather than plain text at the transport layer; the
// model still sees the original content.
func formatWebSearchResults(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var results []webSearchResult
	if err := json.Unmarshal(raw, &results); err != nil {
		return ""
	}
	var b strings.Builder
	for _, r := range results {
		if r.Title == "" && r.URL == "" {
			continue
		}
		fmt.Fprintf(&b, "\n- **%s**", r.Title)
		if r.URL != "" {
			fmt.Fprintf(&b, "\n  <%s>", r.URL)
		}
	}
	if b.Len() == 0 {
		return ""
	}
	return "\n" + b.String() + "\n"
}

const cacheWrite5MinuteInputMultiplier = 1.25

func ephemeral() *cacheControl { return &cacheControl{Type: "ephemeral"} }

type cacheControl struct {
	Type string `json:"type"`
}

type anthRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       []textBlock     `json:"system,omitempty"`
	Messages     []anthMessage   `json:"messages"`
	Tools        []anthTool      `json:"tools,omitempty"`
	Temperature  *float64        `json:"temperature,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
	Stream       bool            `json:"stream"`
}

type thinkingConfig struct {
	Type    string `json:"type"`              // "adaptive"
	Display string `json:"display,omitempty"` // "summarized" to stream the reasoning text
}

type outputConfig struct {
	Effort string `json:"effort,omitempty"` // low | high | max
}

type textBlock struct {
	Type         string        `json:"type"`
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type anthMessage struct {
	Role    string         `json:"role"`
	Content []contentBlock `json:"content"`
}

// contentBlock is the union of the block kinds we emit in a request: text,
// tool_use (echoing a prior assistant call), and tool_result. Unused fields are
// omitted so each block serialises to its canonical shape.
type contentBlock struct {
	Type         string          `json:"type"`
	Text         string          `json:"text,omitempty"`        // text
	Thinking     string          `json:"thinking,omitempty"`    // thinking
	Signature    string          `json:"signature,omitempty"`   // thinking
	ID           string          `json:"id,omitempty"`          // tool_use
	Name         string          `json:"name,omitempty"`        // tool_use
	Input        json.RawMessage `json:"input,omitempty"`       // tool_use
	ToolUseID    string          `json:"tool_use_id,omitempty"` // tool_result
	Content      any             `json:"content,omitempty"`     // tool_result: string, or []contentBlock when the result carries images
	Source       *imageSource    `json:"source,omitempty"`      // image
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type imageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// toolResultBlocks builds array content for a tool_result whose message carries
// images: the text first, then one image block per parseable data URL. It
// returns nil when nothing parses, so text-only results keep plain string
// content — byte-identical serialization to previous releases.
func toolResultBlocks(text string, images []string) []contentBlock {
	var imgs []contentBlock
	for _, url := range images {
		if mt, data, ok := provider.ParseImageDataURL(url); ok {
			imgs = append(imgs, contentBlock{Type: "image", Source: &imageSource{Type: "base64", MediaType: mt, Data: data}})
		}
	}
	if imgs == nil {
		return nil
	}
	return append([]contentBlock{{Type: "text", Text: text}}, imgs...)
}

type anthTool struct {
	Type         string          `json:"type,omitempty"` // "web_search" for server-side search; empty for named tools
	Name         string          `json:"name,omitempty"`
	Description  string          `json:"description,omitempty"`
	InputSchema  json.RawMessage `json:"input_schema,omitempty"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// streamEvent is the discriminated SSE event; read the fields matching Type.
type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *wireUsage `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type      string          `json:"type"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		ToolUseID string          `json:"tool_use_id"` // web_search_tool_result
		Content   json.RawMessage `json:"content"`     // web_search_tool_result: array of result objects
	} `json:"content_block"`
	Delta *struct {
		Type             string          `json:"type"`         // text_delta | thinking_delta | signature_delta | input_json_delta | web_search_tool_result_delta
		Text             string          `json:"text"`         // text_delta
		Thinking         string          `json:"thinking"`     // thinking_delta
		Signature        string          `json:"signature"`    // signature_delta
		PartialJSON      string          `json:"partial_json"` // input_json_delta
		StopReason       string          `json:"stop_reason"`  // message_delta
		WebSearchResults json.RawMessage `json:"results"`      // web_search_tool_result_delta
	} `json:"delta"`
	Usage *wireUsage `json:"usage"` // message_delta (cumulative output_tokens)
	Error *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type wireUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}
