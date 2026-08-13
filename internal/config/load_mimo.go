package config

import (
	"maps"
	"strings"
)

func normalizeLegacyMimoCustomProviders(c *Config) bool {
	return normalizeLegacyMimoCustomProvidersForRefs(c, legacyMimoConfigRefs(c)...)
}

// NormalizeLegacyMimoCustomProvidersForRefs appends custom OpenAI-compatible
// MiMo providers needed by legacy refs that live outside reasonix.toml, such as
// restored desktop tab state.
func NormalizeLegacyMimoCustomProvidersForRefs(c *Config, refs ...string) bool {
	return normalizeLegacyMimoCustomProvidersForRefs(c, refs...)
}

func normalizeLegacyMimoCustomProvidersForRefs(c *Config, refs ...string) bool {
	if c == nil {
		return false
	}
	needed := map[string]bool{}
	addRef := func(ref string) {
		if name := legacyMimoProviderNameForRef(ref); name != "" {
			needed[name] = true
		}
	}
	for _, ref := range refs {
		addRef(ref)
	}
	changed := normalizeLegacyMimoProviderCatalogs(c)
	for name := range needed {
		if _, ok := c.Provider(name); ok {
			continue
		}
		c.Providers = append(c.Providers, legacyMimoCustomProvider(name))
		changed = true
	}
	if normalizeLegacyMimoProviderCatalogs(c) {
		changed = true
	}
	return changed
}

func legacyMimoConfigRefs(c *Config) []string {
	if c == nil {
		return nil
	}
	refs := []string{
		c.DefaultModel,
		c.Agent.PlannerModel,
		c.Agent.SubagentModel,
		c.Bot.Model,
	}
	for _, ref := range c.Agent.SubagentModels {
		refs = append(refs, ref)
	}
	for _, conn := range c.Bot.Connections {
		refs = append(refs, conn.Model)
	}
	refs = append(refs, c.Desktop.ProviderAccess...)
	return refs
}

func legacyMimoProviderName(ref string) string {
	switch strings.TrimSpace(ref) {
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api", "mimo-token-plan", "mimo-pro", "mimo-flash":
		return strings.TrimSpace(ref)
	default:
		return ""
	}
}

func legacyMimoProviderNameForRef(ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	providerName, _, hasModel := strings.Cut(ref, "/")
	if name := legacyMimoProviderName(providerName); name != "" {
		return name
	}
	if hasModel {
		return ""
	}
	switch ref {
	case "mimo-v2.5-pro":
		return "mimo-pro"
	case "mimo-v2.5":
		return "mimo-flash"
	case "mimo-v2-omni":
		return "mimo-api"
	default:
		return ""
	}
}

func legacyMimoAPIModels() []string {
	return []string{"mimo-v2.5-pro", "mimo-v2.5", "mimo-v2-omni"}
}

func legacyMimoTokenPlanModels() []string {
	return []string{"mimo-v2.5-pro", "mimo-v2.5"}
}

func legacyMimoCustomProvider(name string) ProviderEntry {
	switch strings.TrimSpace(name) {
	case "mimo", "xiaomi-mimo", "xiaomi_mimo", "mimo-api":
		models := legacyMimoAPIModels()
		return ProviderEntry{
			Name:          strings.TrimSpace(name),
			Kind:          "openai",
			BaseURL:       "https://api.xiaomimimo.com/v1",
			Models:        models,
			VisionModels:  []string{"mimo-v2.5", "mimo-v2-omni"},
			Default:       "mimo-v2.5-pro",
			APIKeyEnv:     "MIMO_API_KEY",
			ContextWindow: 1_048_576,
			Prices:        mimoDomesticPrices(models),
			NoProxy:       true,
		}
	case "mimo-token-plan":
		models := legacyMimoTokenPlanModels()
		return ProviderEntry{
			Name:          "mimo-token-plan",
			Kind:          "openai",
			BaseURL:       "https://token-plan-cn.xiaomimimo.com/v1",
			Models:        models,
			VisionModels:  []string{"mimo-v2.5"},
			Default:       "mimo-v2.5-pro",
			APIKeyEnv:     "MIMO_API_KEY",
			ContextWindow: 1_048_576,
			Prices:        mimoDomesticPrices(models),
			NoProxy:       true,
		}
	case "mimo-flash":
		return ProviderEntry{Name: "mimo-flash", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: mimoV25Price(), NoProxy: true}
	default:
		return ProviderEntry{Name: "mimo-pro", Kind: "openai", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Model: "mimo-v2.5-pro", APIKeyEnv: "MIMO_API_KEY", ContextWindow: 1_000_000, Price: mimoV25ProPrice(), NoProxy: true}
	}
}

func normalizeDesktopOfficialProviderAccess(c *Config) {
	if c == nil || len(c.Desktop.ProviderAccess) == 0 {
		return
	}
	canCanonicalizeDeepSeek := canCanonicalizeLegacyDeepSeekProviders(c)
	_, hasCanonicalDeepSeek := c.Provider("deepseek")
	legacyDeepSeek := officialLegacyDeepSeekProviders(c)
	seen := desktopProviderAccessMap(nil)
	next := make([]string, 0, len(c.Desktop.ProviderAccess))
	for _, name := range c.Desktop.ProviderAccess {
		name = strings.TrimSpace(name)
		if name == "deepseek" && !canCanonicalizeDeepSeek && !hasCanonicalDeepSeek && len(legacyDeepSeek) > 0 {
			for _, legacy := range legacyDeepSeek {
				if !seen[legacy.Name] {
					seen[legacy.Name] = true
					next = append(next, legacy.Name)
				}
			}
			continue
		}
		if CanonicalDesktopOfficialProviderName(name) != "deepseek" || name == "deepseek" || canCanonicalizeDeepSeek {
			name = desktopProviderAccessNameForConfig(c, name)
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		next = append(next, name)
	}
	c.Desktop.ProviderAccess = next
	if seen["deepseek"] {
		ensureDeepSeekOfficialProvider(c)
	}
	normalizeLegacyMimoProviderCatalogs(c)
	retargetAccess := maps.Clone(seen)
	if p, ok := c.Provider("deepseek"); !canCanonicalizeDeepSeek || !ok || officialProviderKind(p) != "deepseek" {
		delete(retargetAccess, "deepseek")
	}
	retargetDesktopOfficialRefs(c, retargetAccess)
}

// NormalizeLegacyDesktopProviderAccess seeds the desktop provider-access list
// for configs written before Settings tracked explicit provider access. Callers
// should only use this when they know the TOML did not declare provider_access;
// an explicit empty list means the user removed all access entries.
func NormalizeLegacyDesktopProviderAccess(c *Config) {
	if c == nil || len(c.Desktop.ProviderAccess) > 0 {
		return
	}
	seen := desktopProviderAccessMap(nil)
	var access []string
	add := func(name string) {
		name = desktopProviderAccessNameForConfig(c, name)
		if name == "" || seen[name] {
			return
		}
		seen[name] = true
		access = append(access, name)
	}
	addRef := func(ref string) {
		if entry, ok := c.ResolveModel(ref); ok {
			if !entry.Configured() {
				return
			}
			add(entry.Name)
		}
	}
	addRef(c.DefaultModel)
	addRef(c.Agent.PlannerModel)
	addRef(c.Agent.SubagentModel)
	for _, ref := range c.Agent.SubagentModels {
		addRef(ref)
	}
	addRef(c.Bot.Model)
	for _, conn := range c.Bot.Connections {
		addRef(conn.Model)
	}
	for i := range c.Providers {
		p := &c.Providers[i]
		if legacyMimoProviderName(p.Name) != "" && len(p.ModelList()) > 0 {
			add(p.Name)
			continue
		}
		if p.Configured() && len(p.ModelList()) > 0 {
			add(p.Name)
		}
	}
	if len(access) == 0 {
		return
	}
	c.Desktop.ProviderAccess = access
	normalizeDesktopOfficialProviderAccess(c)
}

func canonicalDesktopOfficialProviderName(name string) string {
	switch strings.TrimSpace(name) {
	case "deepseek-flash", "deepseek-pro":
		return "deepseek"
	default:
		return strings.TrimSpace(name)
	}
}

func desktopProviderAccessNameForConfig(c *Config, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	canonical := canonicalDesktopOfficialProviderName(name)
	if canonical == name {
		return name
	}
	if c == nil {
		return canonical
	}
	if p, ok := c.Provider(name); ok && !providerEntryMatchesCanonicalOfficialAccess(p, canonical) {
		return name
	}
	return canonical
}

func providerEntryMatchesCanonicalOfficialAccess(p *ProviderEntry, canonical string) bool {
	if p == nil {
		return false
	}
	switch canonical {
	case "deepseek":
		return isCanonicalizableLegacyDeepSeekProvider(p)
	default:
		return false
	}
}

// CanonicalDesktopOfficialProviderName returns the Settings Center provider ID
// for built-in official provider aliases.
func CanonicalDesktopOfficialProviderName(name string) string {
	return canonicalDesktopOfficialProviderName(name)
}

func desktopProviderAccessMap(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func ensureDeepSeekOfficialProvider(c *Config) {
	if p, ok := c.Provider("deepseek"); ok {
		if officialProviderKind(p) == "deepseek" {
			backfillOfficialContextWindow(p, 1_000_000)
		}
		return
	}
	if !canCanonicalizeLegacyDeepSeekProviders(c) {
		return
	}
	entry := ProviderEntry{
		Name:          "deepseek",
		Kind:          "anthropic",
		BaseURL:       deepSeekAnthropicBaseURL,
		Models:        []string{"deepseek-v4-flash", "deepseek-v4-pro"},
		Default:       "deepseek-v4-flash",
		APIKeyEnv:     "DEEPSEEK_API_KEY",
		BalanceURL:    "https://api.deepseek.com/user/balance",
		Thinking:      "enabled",
		WebSearch:     boolPointer(true),
		ContextWindow: 1_000_000,
		Prices:        deepSeekV4PricesForConfig(c),
		ModelOverrides: map[string]ProviderModelOverride{
			"deepseek-v4-flash": {SupportedEfforts: []string{"disabled", "low", "high", "max"}, DefaultEffort: "high"},
			"deepseek-v4-pro":   {SupportedEfforts: []string{"disabled", "high", "max"}, DefaultEffort: "high"},
		},
	}
	legacyProviders := officialLegacyDeepSeekProviders(c)
	if len(legacyProviders) > 0 {
		entry = officialProviderFromLegacy(entry, legacyProviders[0])
		currency := c.DeepSeekOfficialPricingCurrency()
		if c.DesktopCurrency() == "" && legacyProviders[0].persistedOfficialCurrency != "" {
			currency = legacyProviders[0].persistedOfficialCurrency
			entry.persistedOfficialCurrency = currency
		}
		entry.Prices = DeepSeekV4PricesForCurrency(currency)
		for _, old := range legacyProviders {
			entry.Models = mergeModelLists(entry.Models, old.ModelList())
			mergeLegacyDeepSeekModelConfiguration(&entry, old)
		}
		entry.Default = preferredLegacyDeepSeekDefault(legacyProviders, entry.Models, entry.Default)
	}
	backfillOfficialContextWindow(&entry, 1_000_000)
	c.Providers = append(c.Providers, entry)
}

func isOpenAIProviderKind(e *ProviderEntry) bool {
	return e != nil && strings.EqualFold(strings.TrimSpace(e.Kind), "openai")
}

func backfillOfficialContextWindow(e *ProviderEntry, fallback int) {
	if e != nil && e.ContextWindow <= 0 {
		e.ContextWindow = fallback
	}
}

func officialProviderFromLegacy(entry ProviderEntry, old *ProviderEntry) ProviderEntry {
	if old == nil {
		return entry
	}

	legacy := cloneProviderEntry(*old)
	legacy.Name = entry.Name
	legacy.Model = ""
	legacy.Models = append([]string(nil), entry.Models...)
	legacy.Default = entry.Default
	legacy.ContextWindow = entry.ContextWindow
	legacy.MaxOutputTokens = entry.MaxOutputTokens
	legacy.Price = nil
	legacy.Prices = clonePricingMap(entry.Prices)
	legacy.ReasoningProtocol = entry.ReasoningProtocol
	legacy.SupportedEfforts = append([]string(nil), entry.SupportedEfforts...)
	legacy.DefaultEffort = entry.DefaultEffort
	legacy.Vision = entry.Vision
	legacy.VisionModels = append([]string(nil), entry.VisionModels...)
	legacy.ModelOverrides = cloneModelOverrideMap(entry.ModelOverrides)
	return legacy
}

func officialLegacyDeepSeekProviders(c *Config) []*ProviderEntry {
	if c == nil {
		return nil
	}
	out := make([]*ProviderEntry, 0, 2)
	for _, name := range []string{"deepseek-flash", "deepseek-pro"} {
		if p, ok := c.Provider(name); ok && isCanonicalizableLegacyDeepSeekProvider(p) {
			out = append(out, p)
		}
	}
	return out
}

func isCanonicalizableLegacyDeepSeekProvider(p *ProviderEntry) bool {
	if p == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(p.Kind)) {
	case "openai":
		return isOfficialDeepSeekOpenAIEndpoint(p.BaseURL)
	case "anthropic":
		return IsOfficialDeepSeekWebSearchEndpoint(p)
	default:
		return false
	}
}
