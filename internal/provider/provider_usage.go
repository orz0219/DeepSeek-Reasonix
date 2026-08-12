package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// ChunkType identifies the kind of a streamed increment.
type ChunkType int

// Usage reports token accounting for a completion. Cache hit/miss come from
// either DeepSeek's top-level prompt_cache_{hit,miss}_tokens or the OpenAI/MiMo
// standard prompt_tokens_details.cached_tokens — the openai provider normalises
// both shapes into these fields. ReasoningTokens is the thinking-mode subset of
// CompletionTokens reported by thinking-capable models. FinishReason carries
// the model's last reported choices[0].finish_reason so the agent can surface
// abnormal terminations ("length", "content_filter", "repetition_truncation").
// Estimated marks counts reconstructed locally because the provider's terminal
// usage record did not arrive; exact provider usage leaves it false.
type Usage struct {
	PromptTokens           int
	CompletionTokens       int
	TotalTokens            int
	CacheHitTokens         int     // prompt tokens served from cache
	CacheMissTokens        int     // prompt tokens not cached, including CacheWriteTokens
	CacheWriteTokens       int     // subset of CacheMissTokens used to create provider cache entries
	CacheWriteBilledTokens float64 // cache-write charge expressed in ordinary input-token equivalents
	ReasoningTokens        int     // subset of CompletionTokens spent on chain-of-thought
	FinishReason           string  // "stop", "tool_calls", "length", "content_filter", "repetition_truncation", …
	Estimated              bool
	// RequestCount is the number of provider requests represented by this
	// aggregate. Zero means one request for backward compatibility. Recovery
	// paths that merge multiple attempts set the exact count.
	RequestCount int
	// Context* fields describe the latest single-request shape for context
	// gauges and rebind telemetry. When zero, consumers fall back to the
	// billable Prompt/Completion/… fields. Multi-attempt sampling recovery
	// sets PromptTokens (etc.) to the billable aggregate and fills Context*
	// from the final attempt only.
	ContextPromptTokens     int
	ContextCompletionTokens int
	ContextReasoningTokens  int
	ContextCacheHitTokens   int
	ContextCacheMissTokens  int
}

// ContextFillTokens returns the latest prompt occupancy used by context gauges.
func (u *Usage) ContextFillTokens() int {
	return u.LatestPromptTokens()
}

// LatestPromptTokens returns the latest-attempt prompt size for context-aware
// runtime decisions. Falls back to PromptTokens for single-attempt legacy usage.
func (u *Usage) LatestPromptTokens() int {
	if u == nil {
		return 0
	}
	if u.ContextPromptTokens > 0 {
		return u.ContextPromptTokens
	}
	return u.PromptTokens
}

// Pricing is a provider's per-1M-token rates, used to estimate spend. Currency
// is a display symbol or ISO-like code (default "¥"). toml tags let config decode it.
type Pricing struct {
	CacheHit float64 `toml:"cache_hit"` // per 1M cached prompt tokens
	Input    float64 `toml:"input"`     // per 1M uncached prompt tokens
	Output   float64 `toml:"output"`    // per 1M completion tokens
	Currency string  `toml:"currency"`
}

// Cost estimates the spend for a usage record. Compatibility adapter only —
// new host code must consume billing.CostQuote instead of aggregating floats.
func (p *Pricing) Cost(u *Usage) float64 {
	if p == nil || u == nil {
		return 0
	}

	hit := u.CacheHitTokens
	miss := u.CacheMissTokens
	if hit+miss == 0 && u.PromptTokens > 0 {
		miss = u.PromptTokens
	} else if miss == 0 && hit > 0 && u.PromptTokens > hit {
		miss = u.PromptTokens - hit
	}

	write := min(max(u.CacheWriteTokens, 0), miss)
	billedWrite := 0.0
	if write > 0 {
		billedWrite = u.CacheWriteBilledTokens
		if billedWrite <= 0 {
			billedWrite = float64(write)
		}
	}
	inputTokenUnits := float64(miss-write) + billedWrite
	return (float64(hit)*p.CacheHit +
		inputTokenUnits*p.Input +
		float64(u.CompletionTokens)*p.Output) / 1e6
}

// Symbol returns the currency display symbol, defaulting to "¥".
func (p *Pricing) Symbol() string {
	if p == nil || p.Currency == "" {
		return "¥"
	}
	return currencySymbol(p.Currency)
}

func currencySymbol(currency string) string {
	value := strings.TrimSpace(currency)
	if value == "" {
		return "¥"
	}
	switch strings.ToLower(value) {
	case "cny", "rmb", "yuan", "renminbi", "cnh":
		return "¥"
	case "usd", "dollar", "dollars", "us dollar", "us dollars", "us$":
		return "$"
	case "eur", "euro", "euros":
		return "€"
	case "gbp", "pound", "pounds", "sterling":
		return "£"
	case "jpy", "yen":
		return "¥"
	}
	switch value {
	case "￥", "¥":
		return "¥"
	case "$", "€", "£":
		return value
	}

	for _, r := range value {
		if unicode.Is(unicode.Sc, r) {
			return value
		}
	}
	if isThreeLetterCurrencyCode(value) {
		return strings.ToUpper(value) + " "
	}
	return "¥"
}

func isThreeLetterCurrencyCode(value string) bool {
	if len(value) != 3 {
		return false
	}
	for _, r := range value {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}

// Chunk is a single streamed event. Read the field matching Type.
type Chunk struct {
	Type      ChunkType
	Text      string // ChunkText, ChunkReasoning
	Signature string // ChunkReasoning: opaque proof for the reasoning (Anthropic thinking signature), when issued
	// ReasoningID/ReasoningStatus ride the final ChunkReasoning of a turn
	// (empty Text): the provider-issued reasoning item id/status captured
	// from the SSE stream, so the Agent can persist them into the session
	// and the next turn's input reasoning item round-trips them (review
	// #7234 — OpenAI Responses schema marks Reasoning.id required).
	ReasoningID     string          // ChunkReasoning: provider-issued reasoning item id
	ReasoningStatus string          // ChunkReasoning: final reasoning item status ("completed")
	ToolCall        *ToolCall       // ChunkToolCallStart (ID+Name only), ChunkToolCallArgsDelta (ID+Name), ChunkToolCall (complete)
	ArgChars        int             // ChunkToolCallArgsDelta: cumulative argument characters received for this call
	ResponsesItem   json.RawMessage // ChunkResponsesItem: opaque validated Responses API output item
	Usage           *Usage          // ChunkUsage
	Err             error           // ChunkError
}

// StreamInterruptedError marks that the current sampling attempt never reached
// a clean provider terminal event and is therefore uncommitted. The Agent may
// replay the exact same provider request. Providers must not perform body-phase
// request replay themselves — that lives at the Agent layer so retry budgets,
// UI rollback, and tool execution stay single-owner. context.Canceled, auth,
// 4xx/schema errors, and unparseable complete protocol payloads must not use
// this type.
type StreamInterruptedError struct {
	Err    error
	Reason string // one of the StreamInterrupt* constants; may be empty for older callers
}

func (e *StreamInterruptedError) Error() string {
	if e == nil || e.Err == nil {
		return "stream interrupted"
	}
	return e.Err.Error()
}

func (e *StreamInterruptedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// StreamInterrupt wraps err as a StreamInterruptedError with a fixed reason.
func StreamInterrupt(err error, reason string) error {
	if err == nil {
		return nil
	}
	return &StreamInterruptedError{Err: err, Reason: reason}
}

// StreamInterruptReason returns the fixed reason when err is a stream
// interruption, or empty otherwise.
func StreamInterruptReason(err error) string {
	var interrupted *StreamInterruptedError
	if !errors.As(err, &interrupted) || interrupted == nil {
		return ""
	}
	if interrupted.Reason != "" {
		return interrupted.Reason
	}
	return ClassifyStreamInterrupt(interrupted.Err)
}
