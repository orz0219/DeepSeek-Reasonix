package openai

import (
	"net/http"
	"strings"
)

func normalizeReasoningProtocol(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "deepseek", "glm", "kimi-k3", "openai", "none":
		return strings.ToLower(strings.TrimSpace(raw))
	default:
		return ""
	}
}

func normalizeChatURL(baseURL, chatURL string) string {
	if legacy := strings.TrimRight(strings.TrimSpace(chatURL), "/"); legacy != "" {
		return legacy
	}
	return strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/chat/completions"
}

func cleanCustomHeaders(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for rawName, rawValue := range in {
		name := strings.TrimSpace(rawName)
		value := strings.TrimSpace(rawValue)
		if name == "" || value == "" || reservedCustomHeader(name) {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func applyCustomHeaders(h http.Header, headers map[string]string) {
	for name, value := range cleanCustomHeaders(headers) {
		h.Set(name, value)
	}
}

func applyAPIKeyHeader(h http.Header, baseURL, apiKey string) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return
	}
	if IsMiMo(baseURL) {
		h.Set("api-key", apiKey)
		return
	}
	h.Set("Authorization", "Bearer "+apiKey)
}

func cleanExtraBody(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for rawName, value := range in {
		name := strings.TrimSpace(rawName)
		if name == "" || reservedExtraBodyField(name) {
			continue
		}
		out[name] = value
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func reservedExtraBodyField(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "model", "messages", "tools", "stream", "stream_options", "temperature", "max_tokens", "max_completion_tokens", "max_output_tokens", "reasoning_effort", "thinking":
		return true
	default:
		return false
	}
}

func reservedCustomHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "authorization", "content-type", "accept", "host":
		return true
	default:
		return false
	}
}
