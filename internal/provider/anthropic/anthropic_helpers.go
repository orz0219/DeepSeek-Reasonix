package anthropic

import (
	"net/http"
	"strings"
)

func (c *client) deepSeekThinkingEnabled() bool {
	return c != nil && c.deepseek && c.thinking != "disabled" && c.effort != "disabled"
}

// deepSeekAnthropicUsesProEffortMapping mirrors DeepSeek's model routing for the
// Anthropic endpoint. Opus aliases route to V4 Pro; Sonnet/Haiku aliases and
// unsupported model names route to V4 Flash.
func deepSeekAnthropicUsesProEffortMapping(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	return model == "deepseek-v4-pro" || strings.HasPrefix(model, "claude-opus")
}

func normalizeDeepSeekAnthropicEffort(model, effort string) string {
	switch effort {
	case "low":
		if deepSeekAnthropicUsesProEffortMapping(model) {
			return "high"
		}
		return "low"
	case "medium":
		return "high"
	case "xhigh":
		if deepSeekAnthropicUsesProEffortMapping(model) {
			return "max"
		}
		return "high"
	case "high", "max":
		return effort
	default:
		return ""
	}
}

func cleanCustomHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for name, value := range in {
		name = strings.TrimSpace(name)
		if name == "" || reservedCustomHeader(name) {
			continue
		}
		out[name] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func reservedCustomHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "content-type", "accept", "x-api-key", "authorization", "anthropic-version":
		return true
	default:
		return false
	}
}

func applyCustomHeaders(h http.Header, headers map[string]string) {
	for name, value := range cleanCustomHeaders(headers) {
		h.Set(name, value)
	}
}
