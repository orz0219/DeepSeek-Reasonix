package config

import (
	"sort"
	"strings"

	"reasonix/internal/provider"
)

// ProviderPreset is a curated, editable provider starter template. Presets are
// not secret-bearing: API key values still live only in Reasonix home .env.
type ProviderPreset struct {
	ID          string
	Label       string
	Description string
	KeyEnv      string
	Entries     []ProviderEntry
}

const (
	ProviderPresetVersion        = 1
	longCat20ContextWindow       = 1_048_576
	legacyLongCat20ContextWindow = 131_072
	longCatOpenAIBaseURL         = "https://api.longcat.chat/openai/v1"
	longCatAnthropicBaseURL      = "https://api.longcat.chat/anthropic"
	deepSeekAnthropicBaseURL     = "https://api.deepseek.com/anthropic"
)

// CuratedProviderPresets returns one-click provider templates for common
// OpenAI-compatible and Anthropic-compatible coding-plan services. These are
// intentionally editable after installation; they reduce setup friction without
// turning fast-moving third-party catalogs into hard runtime dependencies.
func CuratedProviderPresets() []ProviderPreset {
	presets := make([]ProviderPreset, 0, len(curatedProviderPresets))
	for _, preset := range curatedProviderPresets {
		// Keep the old direct lookup available for installed configurations, but
		// do not offer the redundant Anthropic preset in new-provider surfaces.
		if preset.ID == "deepseek-anthropic" {
			continue
		}
		presets = append(presets, cloneProviderPreset(preset))
	}
	sort.SliceStable(presets, func(i, j int) bool {
		return providerPresetDisplayRank(presets[i].ID) < providerPresetDisplayRank(presets[j].ID)
	})
	return presets
}

// CuratedProviderPreset returns a single provider preset by id.
func CuratedProviderPreset(id string) (ProviderPreset, bool) {
	id = strings.ToLower(strings.TrimSpace(id))
	for _, p := range curatedProviderPresets {
		if p.ID == id {
			return cloneProviderPreset(p), true
		}
	}
	return ProviderPreset{}, false
}

func providerPresetDisplayRank(id string) int {
	switch {
	case id == "deepseek-responses":
		return -2
	case id == "glm-cn" || id == "zai-global" || strings.HasPrefix(id, "glm-coding-plan-") || strings.HasPrefix(id, "zai-coding-plan-"):
		return 0
	case strings.HasPrefix(id, "longcat-"):
		return 1
	case id == "token-rhythm":
		return 1
	case strings.HasPrefix(id, "kimi-"):
		return 2
	case strings.HasPrefix(id, "minimax-"):
		return 3
	default:
		return 4
	}
}

var (
	legacyKimiAPIModels = []string{"kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5"}
	kimiAPIModels       = []string{"kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5"}
	kimiAPIVisionModels = []string{"kimi-k3", "kimi-k2.7-code", "kimi-k2.7-code-highspeed", "kimi-k2.6", "kimi-k2.5"}
	kimiCodingModels    = []string{"kimi-for-coding"}

	longCat20Models   = []string{"LongCat-2.0"}
	deepSeekV4Models  = []string{"deepseek-v4-flash", "deepseek-v4-pro"}
	tokenRhythmModels = []string{
		"deepseek-v4-flash", "deepseek-v4-pro", "glm-5", "glm-5.1",
		"minimax-m2.7", "kimi-k2.5", "kimi-k2.6", "minimax-m2.5",
		"mimo-v2.5-pro", "qwen3.7-max", "kimi-k2.7-code", "glm-5.2",
		"qwen3.8-max", "deepseek-v4-flash-0731",
	}
	tokenRhythmVisionModels = []string{"kimi-k2.5", "kimi-k2.6", "kimi-k2.7-code"}

	mimoV25Models       = []string{"mimo-v2.5-pro", "mimo-v2.5"}
	mimoV25VisionModels = []string{"mimo-v2.5"}

	minimaxMSeriesModels       = []string{"MiniMax-M3", "MiniMax-M2.7", "MiniMax-M2.7-highspeed"}
	minimaxMSeriesVisionModels = []string{"MiniMax-M3"}
	deepSeekResponsesModels    = []string{"deepseek-v4-flash"}

	glmAPIModels       = []string{"glm-5.2", "glm-5.1", "glm-5", "glm-5-turbo", "glm-5v-turbo", "glm-4.7", "glm-4.7-flash", "glm-4.7-flashx", "glm-4.6", "glm-4.5", "glm-4.5-air", "glm-4.5-flash"}
	glmAPIVisionModels = []string{"glm-5v-turbo"}
	glmCodingModels    = []string{"glm-5.2", "glm-5.1", "glm-5", "glm-4.7"}
	glmAnthropicModels = []string{"glm-5.2[1m]", "glm-5.2", "glm-5.1", "glm-5", "glm-4.7", "glm-4.5-air"}

	qwenAPIModels        = []string{"qwen3.7-plus", "qwen3.7-max", "qwen3.6-plus", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-next", "qwen3-coder-plus", "MiniMax-M2.5", "glm-5", "glm-4.7", "kimi-k2.5"}
	qwenAPIVisionModels  = []string{"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "kimi-k2.5"}
	qwenPlanModels       = []string{"qwen3.7-plus", "qwen3.6-plus", "kimi-k2.5", "glm-5", "MiniMax-M2.5", "qwen3.5-plus", "qwen3-max-2026-01-23", "qwen3-coder-next", "qwen3-coder-plus", "glm-4.7"}
	qwenPlanVisionModels = []string{"qwen3.7-plus", "qwen3.6-plus", "qwen3.5-plus", "kimi-k2.5"}

	stepfunPlanModels = []string{"step-3.7-flash", "step-3.5-flash", "step-3.5-flash-2603"}

	legacyOpenCodeGoModels           = []string{"glm-5.2", "glm-5.1", "kimi-k2.7-code", "kimi-k2.6", "deepseek-v4-pro", "deepseek-v4-flash", "mimo-v2.5-pro", "mimo-v2.5"}
	opencodeGoModels                 = []string{"glm-5.2", "glm-5.1", "kimi-k3", "kimi-k2.7-code", "kimi-k2.6", "deepseek-v4-pro", "deepseek-v4-flash", "mimo-v2.5-pro", "mimo-v2.5"}
	opencodeGoVisionModels           = []string{"kimi-k3"}
	opencodeZenAnthropicModels       = []string{"claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5", "qwen3.6-plus", "qwen3.5-plus", "qwen3.6-plus-free"}
	opencodeZenAnthropicVisionModels = []string{"claude-sonnet-4-6", "claude-opus-4-8", "claude-haiku-4-5"}

	novitaModels      = []string{"zai-org/glm-5.2", "moonshotai/kimi-k2.7-code", "minimax/minimax-m3", "deepseek/deepseek-v4-pro", "deepseek/deepseek-v4-flash", "qwen/qwen3.7-max", "qwen/qwen3.6-plus", "zai-org/glm-5v-turbo"}
	gmiModels         = []string{"zai-org/GLM-5.2-FP8", "deepseek-ai/DeepSeek-V4-Pro", "deepseek-ai/DeepSeek-V4-Flash", "moonshotai/Kimi-K2.7-Code", "anthropic/claude-sonnet-4.6", "openai/gpt-5.5"}
	vercelModels      = []string{"anthropic/claude-sonnet-4.6", "anthropic/claude-opus-4.8", "openai/gpt-5.4", "openai/gpt-5.4-pro", "moonshotai/kimi-k2.7-code", "zai/glm-5.2", "deepseek/deepseek-v4-pro"}
	huggingFaceModels = []string{"zai-org/GLM-5.2", "deepseek-ai/DeepSeek-V3.2", "Qwen/Qwen3.5-72B-Instruct"}
	nvidiaModels      = []string{"nvidia/nemotron-3-nano-30b-a3b", "nvidia/nemotron-3-super-120b-a12b", "nvidia/nemotron-3-ultra-550b-a55b", "deepseek-ai/deepseek-v4-pro", "qwen/qwen3.5-397b-a17b"}
	ollamaCloudModels = []string{"glm-5.2", "kimi-k2.7-code", "deepseek-v4-pro", "deepseek-v4-flash", "minimax-m3", "nemotron-3-nano:30b", "qwen3-coder-next"}
)

func qwenModelContextOverrides() map[string]ProviderModelOverride {
	return map[string]ProviderModelOverride{
		"qwen3-max-2026-01-23": {ContextWindow: 262_144},
		"qwen3-coder-next":     {ContextWindow: 262_144},
		"MiniMax-M2.5":         {ContextWindow: 196_608},
		"glm-5":                {ContextWindow: 202_752},
		"glm-4.7":              {ContextWindow: 202_752},
		"kimi-k2.5":            {ContextWindow: 262_144},
	}
}

func tokenRhythmModelOverrides() map[string]ProviderModelOverride {
	return map[string]ProviderModelOverride{
		"deepseek-v4-flash": {
			ReasoningProtocol: ReasoningProtocolDeepSeek,
			SupportedEfforts:  []string{"disabled", "low", "high", "max"},
			DefaultEffort:     "high",
		},
		"deepseek-v4-pro": {
			ReasoningProtocol: ReasoningProtocolDeepSeek,
			SupportedEfforts:  []string{"disabled", "high", "max"},
			DefaultEffort:     "high",
		},
		"deepseek-v4-flash-0731": {
			ReasoningProtocol: ReasoningProtocolDeepSeek,
			SupportedEfforts:  []string{"disabled", "low", "high", "max"},
			DefaultEffort:     "high",
		},
		"glm-5": {
			ReasoningProtocol: ReasoningProtocolGLM,
			SupportedEfforts:  []string{"enabled", "disabled"},
			DefaultEffort:     "enabled",
		},
		"glm-5.1": {
			ReasoningProtocol: ReasoningProtocolGLM,
			SupportedEfforts:  []string{"enabled", "disabled"},
			DefaultEffort:     "enabled",
			ContextWindow:     200_000,
		},
		"glm-5.2": {
			ReasoningProtocol: ReasoningProtocolGLM,
			SupportedEfforts:  []string{"enabled", "disabled"},
			DefaultEffort:     "enabled",
		},
		"minimax-m2.7":   {ContextWindow: 200_000},
		"kimi-k2.5":      {ContextWindow: 256_000},
		"kimi-k2.6":      {ContextWindow: 256_000},
		"minimax-m2.5":   {ContextWindow: 200_000},
		"mimo-v2.5-pro":  {ContextWindow: 256_000},
		"kimi-k2.7-code": {ContextWindow: 256_000},
	}
}

func kimiK3DirectOverride() ProviderModelOverride {
	return ProviderModelOverride{
		ReasoningProtocol: ReasoningProtocolOpenAI,
		SupportedEfforts:  []string{"low", "high", "max"},
		DefaultEffort:     "max",
		ContextWindow:     1_048_576,
	}
}

func boolPointer(value bool) *bool {
	return &value
}

func cloneProviderPreset(p ProviderPreset) ProviderPreset {
	p.Entries = cloneProviderEntries(p.Entries)
	for i := range p.Entries {
		p.Entries[i].PresetID = p.ID
		p.Entries[i].PresetVersion = ProviderPresetVersion
	}
	return p
}

func cloneProviderEntries(in []ProviderEntry) []ProviderEntry {
	out := make([]ProviderEntry, 0, len(in))
	for _, e := range in {
		out = append(out, cloneProviderEntry(e))
	}
	return out
}

func cloneProviderEntry(e ProviderEntry) ProviderEntry {
	if e.WebSearch != nil {
		value := *e.WebSearch
		e.WebSearch = &value
	}
	if e.ResponsesStateful != nil {
		value := *e.ResponsesStateful
		e.ResponsesStateful = &value
	}
	if e.visionOverride != nil {
		value := *e.visionOverride
		e.visionOverride = &value
	}
	e.Models = append([]string(nil), e.Models...)
	e.VisionModels = append([]string(nil), e.VisionModels...)
	e.SupportedEfforts = append([]string(nil), e.SupportedEfforts...)
	e.Headers = cloneStringMap(e.Headers)
	e.ExtraBody = cloneAnyMap(e.ExtraBody)
	e.Price = clonePricing(e.Price)
	e.Prices = clonePricingMap(e.Prices)
	e.ModelOverrides = cloneModelOverrideMap(e.ModelOverrides)
	return e
}

func clonePricingMap(in map[string]*provider.Pricing) map[string]*provider.Pricing {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]*provider.Pricing, len(in))
	for k, v := range in {
		out[k] = clonePricing(v)
	}
	return out
}

func cloneAnyMap(in map[string]any) map[string]any {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneAnyValue(v)
	}
	return out
}

func cloneAnyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return cloneAnyMap(x)
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = cloneAnyValue(x[i])
		}
		return out
	default:
		return v
	}
}

func cloneModelOverrideMap(in map[string]ProviderModelOverride) map[string]ProviderModelOverride {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]ProviderModelOverride, len(in))
	for k, v := range in {
		v.SupportedEfforts = append([]string(nil), v.SupportedEfforts...)
		if v.Vision != nil {
			vision := *v.Vision
			v.Vision = &vision
		}
		out[k] = v
	}
	return out
}
