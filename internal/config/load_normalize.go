package config

import (
	"net/url"
	"strings"
)

// normalizeLegacyProviderModels repairs provider entries written by older
// desktop builds that carried the official provider name/endpoint but omitted the
// model field. The repair is intentionally narrow: valid user-provided model
// lists are left untouched, while known official aliases get the model implied by
// their preset name so model pickers and provider validation have an option.
func normalizeLegacyProviderModels(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if providerHasAnyModel(*p) {
			continue
		}
		if model := legacyOfficialProviderModel(p.Name); model != "" {
			p.Model = model
		}
	}
}

const (
	legacyStepFunOpenAIBaseURL      = "https://api.stepfun.ai/step_plan/v1"
	officialStepFunOpenAIBaseURL    = "https://api.stepfun.com/step_plan/v1"
	legacyStepFunAnthropicBaseURL   = "https://api.stepfun.ai/step_plan"
	officialStepFunAnthropicBaseURL = "https://api.stepfun.com/step_plan"
)

func normalizeLegacyStepFunBaseURLs(c *Config) bool {

	return false
}

func normalizedBaseURLForMigration(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

func normalizeLegacyLongCatContextWindows(c *Config) bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.ContextWindow != legacyLongCat20ContextWindow {
			continue
		}
		var kind, baseURL string
		switch strings.TrimSpace(p.PresetID) {
		case "longcat-openai":
			kind, baseURL = "openai", longCatOpenAIBaseURL
		case "longcat-anthropic":
			kind, baseURL = "anthropic", longCatAnthropicBaseURL
		default:
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(p.Kind), kind) ||
			normalizedBaseURLForMigration(p.BaseURL) != baseURL ||
			!stringSlicesEqual(p.Models, longCat20Models) ||
			p.Model != "" ||
			p.Default != longCat20Models[0] {
			continue
		}
		p.ContextWindow = longCat20ContextWindow
		changed = true
	}
	return changed
}

// normalizeLegacyQwenContextWindows upgrades only installed official Qwen
// presets that still carry the old zero context window and untouched model
// catalog. Custom endpoints, catalogs, provider-wide windows, and existing
// per-model override values remain user-owned.
func normalizeLegacyQwenContextWindows(c *Config) bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Providers {
		p := &c.Providers[i]
		if p.ContextWindow != 0 {
			continue
		}
		presetID := qwenPresetIDForMigration(*p)
		if presetID == "" {
			continue
		}
		preset, ok := CuratedProviderPreset(presetID)
		if !ok || len(preset.Entries) != 1 {
			continue
		}
		canonical := preset.Entries[0]
		if !strings.EqualFold(strings.TrimSpace(p.Kind), strings.TrimSpace(canonical.Kind)) ||
			normalizedBaseURLForMigration(p.BaseURL) != normalizedBaseURLForMigration(canonical.BaseURL) ||
			!stringSlicesEqual(p.Models, canonical.Models) ||
			strings.TrimSpace(p.Model) != "" {
			continue
		}
		p.ContextWindow = canonical.ContextWindow
		mergeMissingQwenContextOverrides(p, canonical.ModelOverrides)
		changed = true
	}
	return changed
}

func qwenPresetIDForMigration(p ProviderEntry) string {
	presetID := strings.TrimSpace(p.PresetID)
	if presetID == "" {
		presetID = strings.TrimSpace(p.Name)
	}
	switch presetID {
	case "qwen-cn",
		"qwen-global",
		"qwen-coding-plan-cn",
		"qwen-coding-plan-cn-anthropic",
		"qwen-coding-plan-global",
		"qwen-coding-plan-global-anthropic":
		return presetID
	default:
		return ""
	}
}

func mergeMissingQwenContextOverrides(p *ProviderEntry, defaults map[string]ProviderModelOverride) {
	if p == nil || len(defaults) == 0 {
		return
	}
	if p.ModelOverrides == nil {
		p.ModelOverrides = make(map[string]ProviderModelOverride, len(defaults))
	}
	for defaultKey, defaultOverride := range defaults {
		overrideKey := defaultKey
		for key := range p.ModelOverrides {
			if strings.EqualFold(strings.TrimSpace(key), defaultKey) {
				overrideKey = key
				break
			}
		}
		override := p.ModelOverrides[overrideKey]
		if override.ContextWindow == 0 {
			override.ContextWindow = defaultOverride.ContextWindow
			p.ModelOverrides[overrideKey] = override
		}
	}
}

// normalizeLegacyKimiK3Catalog upgrades only untouched Kimi direct-API model
// catalogs on the official regional endpoints. Custom model lists, endpoints,
// defaults, credentials, and provider-wide settings remain user-owned.
func normalizeLegacyKimiK3Catalog(c *Config) bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Providers {
		p := &c.Providers[i]
		presetID := strings.TrimSpace(p.PresetID)
		name := strings.TrimSpace(p.Name)
		var baseURL string
		switch {
		case presetID == "kimi-cn" || (presetID == "" && name == "kimi-cn"):
			baseURL = "https://api.moonshot.cn/v1"
		case presetID == "kimi-global" || (presetID == "" && name == "kimi-global"):
			baseURL = "https://api.moonshot.ai/v1"
		default:
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(p.Kind), "openai") ||
			normalizedBaseURLForMigration(p.BaseURL) != baseURL ||
			!stringSlicesEqual(p.Models, legacyKimiAPIModels) ||
			strings.TrimSpace(p.Model) != "" {
			continue
		}
		p.Models = append([]string(nil), kimiAPIModels...)
		p.VisionModels = migrateKimiK3VisionModels(p.VisionModels, legacyKimiAPIModels)
		mergeMissingKimiK3Override(p, kimiK3DirectOverride())
		changed = true
	}
	return changed
}

// migrateKimiK3VisionModels preserves explicit provider-level vision choices.
// A nil list or an exact copy of the old preset list indicates that the user
// has not customized vision support and should receive Kimi K3's capability.
func migrateKimiK3VisionModels(current, legacy []string) []string {
	if current != nil && (legacy == nil || !stringSlicesEqual(current, legacy)) {
		return current
	}
	return mergeModelLists([]string{"kimi-k3"}, current)
}

func mergeMissingKimiK3Override(p *ProviderEntry, defaults ProviderModelOverride) {
	if p.ModelOverrides == nil {
		p.ModelOverrides = map[string]ProviderModelOverride{}
	}
	overrideKey := "kimi-k3"
	for key := range p.ModelOverrides {
		if strings.EqualFold(strings.TrimSpace(key), overrideKey) {
			overrideKey = key
			break
		}
	}
	kimiK3 := p.ModelOverrides[overrideKey]
	if strings.TrimSpace(kimiK3.ReasoningProtocol) == "" {
		kimiK3.ReasoningProtocol = defaults.ReasoningProtocol
	}
	if kimiK3.SupportedEfforts == nil {
		kimiK3.SupportedEfforts = append([]string(nil), defaults.SupportedEfforts...)
	}
	if strings.TrimSpace(kimiK3.DefaultEffort) == "" && containsString(normalizedEffortLevels(kimiK3.SupportedEfforts), defaults.DefaultEffort) {
		kimiK3.DefaultEffort = defaults.DefaultEffort
	}
	if kimiK3.ContextWindow <= 0 {
		kimiK3.ContextWindow = defaults.ContextWindow
	}
	p.ModelOverrides[overrideKey] = kimiK3
}

// normalizeLegacyOpenCodeGoKimiK3Catalog upgrades only the untouched model
// catalog from the original editable OpenCode Go preset. A user-curated model
// list or custom endpoint is left alone, while other provider edits (headers,
// key env, provider-wide context) survive the additive K3 capability update.
func normalizeLegacyOpenCodeGoKimiK3Catalog(c *Config) bool {
	if c == nil {
		return false
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		presetID := strings.TrimSpace(p.PresetID)
		if (presetID != "opencode-go" && (presetID != "" || strings.TrimSpace(p.Name) != "opencode-go")) ||
			!strings.EqualFold(strings.TrimSpace(p.Kind), "openai") ||
			normalizedBaseURLForMigration(p.BaseURL) != "https://opencode.ai/zen/go/v1" ||
			!stringSlicesEqual(p.Models, legacyOpenCodeGoModels) ||
			strings.TrimSpace(p.Model) != "" {
			continue
		}
		p.Models = append([]string(nil), opencodeGoModels...)
		p.VisionModels = migrateKimiK3VisionModels(p.VisionModels, nil)
		mergeMissingKimiK3Override(p, ProviderModelOverride{
			ReasoningProtocol: ReasoningProtocolOpenAI,
			SupportedEfforts:  []string{"high", "max"},
			DefaultEffort:     "max",
			ContextWindow:     1_048_576,
		})
		return true
	}
	return false
}

func normalizeLegacyMimoProviderCatalogs(c *Config) bool {
	if c == nil {
		return false
	}
	changed := false
	for i := range c.Providers {
		p := &c.Providers[i]
		if legacyMimoProviderName(p.Name) == "" || len(p.Models) > 0 {
			continue
		}
		switch officialProviderHost(p.BaseURL) {
		case "api.xiaomimimo.com":
			if applyLegacyMimoCatalog(p, legacyMimoAPIModels(), []string{"mimo-v2.5", "mimo-v2-omni"}, "mimo-v2.5-pro") {
				changed = true
			}
		case "token-plan-cn.xiaomimimo.com":
			if applyLegacyMimoCatalog(p, legacyMimoTokenPlanModels(), []string{"mimo-v2.5"}, "mimo-v2.5-pro") {
				changed = true
			}
		}
	}
	return changed
}

func applyLegacyMimoCatalog(p *ProviderEntry, models, visionModels []string, fallbackDefault string) bool {
	if p == nil || len(models) == 0 {
		return false
	}
	beforeModels := append([]string(nil), p.Models...)
	beforeVision := append([]string(nil), p.VisionModels...)
	beforeDefault := p.Default
	beforeModel := p.Model
	beforeWindow := p.ContextWindow
	beforeNoProxy := p.NoProxy
	beforePricesLen := len(p.Prices)

	currentDefault := strings.TrimSpace(p.Default)
	if currentDefault == "" {
		currentDefault = strings.TrimSpace(p.Model)
	}
	p.Models = mergeModelLists(models, p.ModelList())
	p.Model = p.Models[0]
	p.Default = firstKnownModel(currentDefault, p.Models, fallbackDefault)
	p.VisionModels = mergeModelLists(visionModels, p.VisionModels)
	backfillOfficialContextWindow(p, 1_048_576)
	p.NoProxy = true
	if p.Prices == nil {
		p.Prices = mimoDomesticPrices(models)
	} else {
		for model, price := range mimoDomesticPrices(models) {
			if p.Prices[model] == nil {
				p.Prices[model] = price
			}
		}
	}

	return !stringSlicesEqual(beforeModels, p.Models) ||
		!stringSlicesEqual(beforeVision, p.VisionModels) ||
		beforeDefault != p.Default ||
		beforeModel != p.Model ||
		beforeWindow != p.ContextWindow ||
		beforeNoProxy != p.NoProxy ||
		beforePricesLen != len(p.Prices)
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func normalizeOfficialDeepSeekModels(c *Config) {
	if c == nil {
		return
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if officialProviderHost(p.BaseURL) != "api.deepseek.com" {
			continue
		}
		switch strings.TrimSpace(p.Name) {
		case "deepseek":
			required := []string{"deepseek-v4-flash", "deepseek-v4-pro"}
			if strings.EqualFold(strings.TrimSpace(p.Kind), "responses") {
				required = required[:1]
			}
			ensureProviderModels(p, required, "deepseek-v4-flash")
		case "deepseek-flash":
			ensureProviderModels(p, []string{"deepseek-v4-flash"}, "deepseek-v4-flash")
		case "deepseek-pro":
			ensureProviderModels(p, []string{"deepseek-v4-pro"}, "deepseek-v4-pro")
		}
		backfillDeepSeekAnthropicCapabilities(p)
	}
}

func backfillDeepSeekAnthropicCapabilities(p *ProviderEntry) {
	if p == nil || !strings.EqualFold(strings.TrimSpace(p.Kind), "anthropic") ||
		!IsOfficialDeepSeekWebSearchEndpoint(p) {
		return
	}
	if strings.TrimSpace(p.Thinking) == "" {
		p.Thinking = "enabled"
	}
	capabilities := map[string]ProviderModelOverride{
		"deepseek-v4-flash": {SupportedEfforts: []string{"disabled", "low", "high", "max"}, DefaultEffort: "high"},
		"deepseek-v4-pro":   {SupportedEfforts: []string{"disabled", "high", "max"}, DefaultEffort: "high"},
	}
	if model := strings.TrimSpace(p.Model); model != "" && len(p.Models) == 0 {
		defaults, ok := capabilities[model]
		if !ok || len(p.SupportedEfforts) > 0 {
			return
		}
		p.SupportedEfforts = append([]string(nil), defaults.SupportedEfforts...)
		if strings.TrimSpace(p.DefaultEffort) == "" {
			p.DefaultEffort = defaults.DefaultEffort
		}
		return
	}
	if p.ModelOverrides == nil {
		p.ModelOverrides = map[string]ProviderModelOverride{}
	}
	for model, defaults := range capabilities {
		if !p.HasModel(model) {
			continue
		}
		override := p.ModelOverrides[model]
		if len(override.SupportedEfforts) == 0 {
			override.SupportedEfforts = append([]string(nil), defaults.SupportedEfforts...)
			if strings.TrimSpace(override.DefaultEffort) == "" {
				override.DefaultEffort = defaults.DefaultEffort
			}
		}
		p.ModelOverrides[model] = override
	}
}

func officialProviderHost(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func ensureProviderModels(p *ProviderEntry, required []string, fallbackDefault string) {
	if p == nil {
		return
	}

	if len(p.Models) > 0 {
		return
	}
	models := mergeModelLists(required, p.ModelList())
	if len(models) == 0 {
		return
	}
	p.Model = models[0]
	if len(models) > 1 {
		p.Models = models
		p.Default = firstKnownModel(p.Default, models, fallbackDefault)
		return
	}
	p.Models = nil
	p.Default = ""
}

func legacyOfficialProviderModel(name string) string {
	switch strings.TrimSpace(name) {
	case "deepseek-flash":
		return "deepseek-v4-flash"
	case "deepseek-pro":
		return "deepseek-v4-pro"
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api", "mimo-token-plan", "mimo-pro":
		return "mimo-v2.5-pro"
	case "mimo-flash":
		return "mimo-v2.5"
	default:
		return ""
	}
}
