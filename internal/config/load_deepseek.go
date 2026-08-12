package config

import (
	"reflect"
	"slices"
	"strings"

	"reasonix/internal/provider"
)

func canCanonicalizeLegacyDeepSeekProviders(c *Config) bool {
	if c == nil {
		return true
	}
	legacy := officialLegacyDeepSeekProviders(c)
	if canonical, ok := c.Provider("deepseek"); ok {
		if officialProviderKind(canonical) != "deepseek" {
			return false
		}
		for _, old := range legacy {
			if !legacyDeepSeekProviderWideFieldsEqual(canonical, old) ||
				!legacyDeepSeekModelFieldsCompatibleIgnoringDefault(canonical, old) {
				return false
			}
		}
	}
	for i := 1; i < len(legacy); i++ {
		if !legacyDeepSeekProviderWideFieldsEqual(legacy[0], legacy[i]) ||
			!legacyDeepSeekModelFieldsCompatible(legacy[0], legacy[i]) {
			return false
		}
	}
	return true
}

func legacyDeepSeekModelFieldsCompatibleIgnoringDefault(a, b *ProviderEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	left := cloneProviderEntry(*a)
	right := cloneProviderEntry(*b)
	left.Default = ""
	right.Default = ""
	return legacyDeepSeekModelFieldsCompatible(&left, &right)
}

func legacyDeepSeekProviderWideFieldsEqual(a, b *ProviderEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	left := legacyDeepSeekProviderWideProjection(a)
	right := legacyDeepSeekProviderWideProjection(b)
	return reflect.DeepEqual(left, right)
}

func legacyDeepSeekProviderWideProjection(entry *ProviderEntry) ProviderEntry {
	out := cloneProviderEntry(*entry)
	out.Name = ""
	out.Kind = strings.ToLower(strings.TrimSpace(out.Kind))
	out.BaseURL = normalizedBaseURLForMigration(out.BaseURL)
	out.ChatURL = strings.TrimSpace(out.ChatURL)
	out.RequestURL = strings.TrimSpace(out.RequestURL)
	out.ModelsURL = strings.TrimSpace(out.ModelsURL)
	out.APIKeyEnv = strings.TrimSpace(out.APIKeyEnv)
	out.BalanceURL = normalizedDeepSeekBalanceURL(out.BalanceURL)
	out.ResponsesMode = strings.TrimSpace(out.ResponsesMode)
	out.Thinking = strings.TrimSpace(out.Thinking)
	out.Effort = strings.TrimSpace(out.Effort)
	out.VisionDetail = strings.TrimSpace(out.VisionDetail)

	out.Model = ""
	out.Models = nil
	out.Default = ""
	out.ContextWindow = 0
	out.MaxOutputTokens = 0
	out.Price = nil
	out.Prices = nil
	out.ReasoningProtocol = ""
	out.SupportedEfforts = nil
	out.DefaultEffort = ""
	out.Vision = false
	out.VisionModels = nil
	out.ModelOverrides = nil
	out.visionOverride = nil
	out.resolvedAPIKey = ""
	out.resolvedSource = CredentialSource{}
	return out
}

func normalizedDeepSeekBalanceURL(raw string) string {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	if raw == "" {
		return deepSeekOfficialBalanceURL
	}
	return raw
}

type legacyDeepSeekModelFields struct {
	contextWindowSet     bool
	contextWindow        int
	maxOutputTokensSet   bool
	maxOutputTokens      int
	priceSet             bool
	price                *provider.Pricing
	reasoningProtocolSet bool
	reasoningProtocol    string
	supportedEffortsSet  bool
	supportedEfforts     []string
	defaultEffortSet     bool
	defaultEffort        string
	visionSet            bool
	vision               bool
}

func legacyDeepSeekModelFieldsCompatible(a, b *ProviderEntry) bool {
	if a == nil || b == nil {
		return a == b
	}
	if left, right := strings.TrimSpace(a.Default), strings.TrimSpace(b.Default); left != "" && right != "" && left != right {
		return false
	}
	models := map[string]string{}
	add := func(model string) {
		model = strings.TrimSpace(model)
		if model != "" {
			models[strings.ToLower(model)] = model
		}
	}
	for _, entry := range []*ProviderEntry{a, b} {
		for _, model := range entry.ModelList() {
			add(model)
		}
		for _, model := range entry.VisionModels {
			add(model)
		}
		for model := range entry.Prices {
			add(model)
		}
		for model := range entry.ModelOverrides {
			add(model)
		}
	}
	for _, model := range models {
		left, leftSet := legacyDeepSeekModelFieldProjection(a, model)
		right, rightSet := legacyDeepSeekModelFieldProjection(b, model)
		if leftSet && rightSet && !legacyDeepSeekModelFieldProjectionsCompatible(left, right) {
			return false
		}
	}
	return true
}

func legacyDeepSeekModelFieldProjection(entry *ProviderEntry, model string) (legacyDeepSeekModelFields, bool) {
	var out legacyDeepSeekModelFields
	if entry == nil {
		return out, false
	}
	listed := entry.HasModel(model)
	if listed {
		out.contextWindowSet = true
		out.contextWindow = entry.ContextWindow
		if out.contextWindow <= 0 {
			out.contextWindow = 1_000_000
		}
		out.maxOutputTokensSet = true
		out.maxOutputTokens = entry.MaxOutputTokens
		out.reasoningProtocolSet = true
		out.reasoningProtocol = strings.TrimSpace(entry.ReasoningProtocol)
		out.supportedEffortsSet = true
		out.supportedEfforts = append([]string(nil), entry.SupportedEfforts...)
		out.defaultEffortSet = true
		out.defaultEffort = strings.TrimSpace(entry.DefaultEffort)
		out.visionSet = true
		out.vision = entry.Vision || entry.HasVisionModel(model)
		if price := entry.PriceForModel(model); price != nil {
			out.priceSet = true
			out.price = price
		}
	}
	if price, ok := pricingForModelKey(entry.Prices, model); ok {
		out.priceSet = true
		out.price = clonePricing(price)
	}
	if override, ok := entry.modelOverrideForModel(model); ok {
		if override.ContextWindow > 0 {
			out.contextWindowSet = true
			out.contextWindow = override.ContextWindow
		}
		if override.MaxOutputTokens != 0 {
			out.maxOutputTokensSet = true
			out.maxOutputTokens = override.MaxOutputTokens
		}
		if strings.TrimSpace(override.ReasoningProtocol) != "" {
			out.reasoningProtocolSet = true
			out.reasoningProtocol = strings.TrimSpace(override.ReasoningProtocol)
		}
		if override.SupportedEfforts != nil {
			out.supportedEffortsSet = true
			out.supportedEfforts = append([]string(nil), override.SupportedEfforts...)
			out.defaultEffortSet = true
			out.defaultEffort = strings.TrimSpace(override.DefaultEffort)
		}
		if override.Vision != nil {
			out.visionSet = true
			out.vision = *override.Vision
		}
	}
	return out, listed || out.contextWindowSet || out.maxOutputTokensSet || out.priceSet ||
		out.reasoningProtocolSet || out.supportedEffortsSet || out.defaultEffortSet || out.visionSet
}

func pricingForModelKey(prices map[string]*provider.Pricing, model string) (*provider.Pricing, bool) {
	for key, price := range prices {
		if strings.EqualFold(strings.TrimSpace(key), strings.TrimSpace(model)) {
			return price, true
		}
	}
	return nil, false
}

func legacyDeepSeekModelFieldProjectionsCompatible(a, b legacyDeepSeekModelFields) bool {
	return (!a.contextWindowSet || !b.contextWindowSet || a.contextWindow == b.contextWindow) &&
		(!a.maxOutputTokensSet || !b.maxOutputTokensSet || a.maxOutputTokens == b.maxOutputTokens) &&
		(!a.priceSet || !b.priceSet || reflect.DeepEqual(a.price, b.price)) &&
		(!a.reasoningProtocolSet || !b.reasoningProtocolSet || a.reasoningProtocol == b.reasoningProtocol) &&
		(!a.supportedEffortsSet || !b.supportedEffortsSet || slices.Equal(a.supportedEfforts, b.supportedEfforts)) &&
		(!a.defaultEffortSet || !b.defaultEffortSet || a.defaultEffort == b.defaultEffort) &&
		(!a.visionSet || !b.visionSet || a.vision == b.vision)
}

func preferredLegacyDeepSeekDefault(entries []*ProviderEntry, models []string, fallback string) string {
	for _, entry := range entries {
		if entry == nil {
			continue
		}
		candidate := strings.TrimSpace(entry.Default)
		if candidate != "" && slices.Contains(models, candidate) {
			return candidate
		}
	}
	return firstKnownModel(fallback, models, "deepseek-v4-flash")
}

func mergeLegacyDeepSeekModelConfiguration(entry, old *ProviderEntry) {
	if entry == nil || old == nil {
		return
	}
	if entry.Prices == nil {
		entry.Prices = map[string]*provider.Pricing{}
	}
	if entry.ModelOverrides == nil {
		entry.ModelOverrides = map[string]ProviderModelOverride{}
	}
	entry.VisionModels = mergeModelLists(entry.VisionModels, old.VisionModels)
	for model, price := range old.Prices {
		entry.Prices[model] = clonePricing(price)
	}
	for _, model := range old.ModelList() {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if price := old.PriceForModel(model); price != nil {
			entry.Prices[model] = price
		}
		override := entry.ModelOverrides[model]
		if old.ContextWindow > 0 && old.ContextWindow != entry.ContextWindow {
			override.ContextWindow = old.ContextWindow
		}
		if old.MaxOutputTokens != entry.MaxOutputTokens {
			override.MaxOutputTokens = old.MaxOutputTokens
		}
		if protocol := strings.TrimSpace(old.ReasoningProtocol); protocol != "" {
			override.ReasoningProtocol = protocol
		}
		if len(old.SupportedEfforts) > 0 {
			override.SupportedEfforts = append([]string(nil), old.SupportedEfforts...)
			override.DefaultEffort = old.DefaultEffort
		}
		if old.Vision || old.HasVisionModel(model) {
			vision := true
			override.Vision = &vision
		}
		if explicit, ok := old.modelOverrideForModel(model); ok {
			mergeProviderModelOverride(&override, explicit)
		}
		entry.ModelOverrides[model] = override
	}
	for model, override := range old.ModelOverrides {
		if old.HasModel(model) {
			continue
		}
		current := entry.ModelOverrides[model]
		mergeProviderModelOverride(&current, override)
		entry.ModelOverrides[model] = current
	}
}

func mergeProviderModelOverride(dst *ProviderModelOverride, src ProviderModelOverride) {
	if dst == nil {
		return
	}
	if strings.TrimSpace(src.ReasoningProtocol) != "" {
		dst.ReasoningProtocol = src.ReasoningProtocol
	}
	if len(src.SupportedEfforts) > 0 {
		dst.SupportedEfforts = append([]string(nil), src.SupportedEfforts...)
		dst.DefaultEffort = src.DefaultEffort
	}
	if src.Vision != nil {
		vision := *src.Vision
		dst.Vision = &vision
	}
	if src.ContextWindow > 0 {
		dst.ContextWindow = src.ContextWindow
	}
	if src.MaxOutputTokens != 0 {
		dst.MaxOutputTokens = src.MaxOutputTokens
	}
}

func mergeModelLists(primary, extra []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(primary)+len(extra))
	for _, list := range [][]string{primary, extra} {
		for _, model := range list {
			model = strings.TrimSpace(model)
			if model == "" || seen[model] {
				continue
			}
			seen[model] = true
			out = append(out, model)
		}
	}
	return out
}

func firstKnownModel(current string, models []string, fallback string) string {
	current = strings.TrimSpace(current)
	if slices.Contains(models, current) {
		return current
	}
	if slices.Contains(models, fallback) {
		return fallback
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

func retargetDesktopOfficialRefs(c *Config, access map[string]bool) {
	c.DefaultModel = retargetDesktopOfficialRef(c.DefaultModel, access)
	c.Agent.PlannerModel = retargetDesktopOfficialRef(c.Agent.PlannerModel, access)
	c.Agent.SubagentModel = retargetDesktopOfficialRef(c.Agent.SubagentModel, access)
	for skill, ref := range c.Agent.SubagentModels {
		c.Agent.SubagentModels[skill] = retargetDesktopOfficialRef(ref, access)
	}
}

func retargetDesktopOfficialRef(ref string, access map[string]bool) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	provider, model, hasModel := strings.Cut(ref, "/")
	switch provider {
	case "deepseek-flash":
		if !access["deepseek"] {
			return ref
		}
		if !hasModel || strings.TrimSpace(model) == "" {
			model = "deepseek-v4-flash"
		}
		return "deepseek/" + model
	case "deepseek-pro":
		if !access["deepseek"] {
			return ref
		}
		if !hasModel || strings.TrimSpace(model) == "" {
			model = "deepseek-v4-pro"
		}
		return "deepseek/" + model
	default:
		return ref
	}
}
