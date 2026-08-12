package main

import (
	"fmt"
	"slices"
	"strings"

	"reasonix/internal/config"
)

func officialProviderTemplate(kind, pricingLanguage string) ([]config.ProviderEntry, string, error) {
	_ = pricingLanguage
	webSearchEnabled := true
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "deepseek", "deepseek-official":

		return []config.ProviderEntry{{
			Name:            "deepseek",
			Kind:            "anthropic",
			BaseURL:         "https://api.deepseek.com/anthropic",
			Models:          []string{"deepseek-v4-flash", "deepseek-v4-pro"},
			Default:         "deepseek-v4-flash",
			APIKeyEnv:       "DEEPSEEK_API_KEY",
			BalanceURL:      "https://api.deepseek.com/user/balance",
			Thinking:        "enabled",
			WebSearch:       &webSearchEnabled,
			ContextWindow:   1_000_000,
			BillingCurrency: "USD",
			BillingMode:     "payg",
			Prices:          config.DeepSeekV4PricesForCurrency("USD"),
			ModelOverrides: map[string]config.ProviderModelOverride{
				"deepseek-v4-flash": {SupportedEfforts: []string{"disabled", "low", "high", "max"}, DefaultEffort: "high"},
				"deepseek-v4-pro":   {SupportedEfforts: []string{"disabled", "high", "max"}, DefaultEffort: "high"},
			},
		}}, "DEEPSEEK_API_KEY", nil
	default:
		return nil, "", fmt.Errorf("unknown official provider template %q", kind)
	}
}

func chatProviderModels(models []string) []string {
	out := make([]string, 0, len(models))
	seen := map[string]bool{}
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] || !config.IsLikelyChatModel(model) {
			continue
		}
		seen[model] = true
		out = append(out, model)
	}
	return out
}

func providerVisionModels(models, visionModels []string) []string {
	enabled := map[string]bool{}
	for _, model := range models {
		enabled[model] = true
	}
	out := make([]string, 0, len(visionModels))
	for _, model := range chatProviderModels(visionModels) {
		if enabled[model] {
			out = append(out, model)
		}
	}
	return out
}

func providerDefaultForModels(currentDefault string, models []string) string {
	currentDefault = strings.TrimSpace(currentDefault)
	if currentDefault != "" {
		if slices.Contains(models, currentDefault) {
			return currentDefault
		}
	}
	if len(models) > 0 {
		return models[0]
	}
	return ""
}

func saveProviderConfig(c *config.Config, p ProviderView) error {
	if c == nil {
		return fmt.Errorf("config is nil")
	}
	e := config.ProviderEntry{Name: p.Name}
	existing := false
	for i := range c.Providers {
		if c.Providers[i].Name == p.Name {
			e = c.Providers[i]
			existing = true
			break
		}
	}
	original := e
	e.Name = p.Name
	e.Kind = p.Kind
	e.BaseURL = p.BaseURL
	e.ChatURL = strings.TrimSpace(p.ChatURL)
	e.RequestURL = strings.TrimSpace(p.RequestURL)
	if strings.EqualFold(strings.TrimSpace(e.Kind), "openai") && e.RequestURL != "" {
		e.ChatURL = e.RequestURL
	}
	e.ModelsURL = strings.TrimSpace(p.ModelsURL)
	e.APIKeyEnv = p.APIKeyEnv
	e.Headers = p.Headers
	e.ExtraBody = p.ExtraBody
	e.AuthHeader = p.AuthHeader
	e.BalanceURL = strings.TrimSpace(p.BalanceURL)
	e.ContextWindow = p.ContextWindow
	e.ReasoningProtocol = p.ReasoningProtocol
	e.Thinking = providerThinkingForSettings(p.Thinking)

	if config.IsOfficialDeepSeekWebSearchEndpoint(&e) {
		enabled := p.WebSearch
		e.WebSearch = &enabled
	} else if !config.SupportsServerWebSearch(&e) || !existing || config.IsOfficialDeepSeekWebSearchEndpoint(&original) {
		e.WebSearch = nil
	}
	e.SupportedEfforts = p.SupportedEfforts
	e.DefaultEffort = p.DefaultEffort
	e.Model = ""
	e.Models = nil
	e.Default = ""
	e.VisionModels = nil
	models := chatProviderModels(p.Models)
	if len(models) > 0 {
		e.Model = models[0]
		e.Models = models
		e.ModelOverrides = providerModelOverridesForSave(p.ModelOverrides, models)
		if p.VisionModelsSet || len(p.VisionModels) > 0 {
			e.Vision = false
			e.VisionModels = providerVisionModels(models, p.VisionModels)
		}
		if len(models) > 1 {
			e.Default = providerDefaultForModels(p.Default, models)
		}
	} else {
		e.Vision = false
		e.VisionModels = nil
		e.ModelOverrides = nil
	}
	if err := c.UpsertProvider(e); err != nil {
		return err
	}
	addProviderAccess(c, p.Name)
	return nil
}

// SaveProvider adds or updates a provider. Enabled models are persisted through
// `models` even when only one model is selected, while `model` remains populated
// in-memory for validation/back-compat. The shared key/endpoint live on the entry.
func (a *App) SaveProvider(p ProviderView) error {
	return a.applyConfigChange(func(c *config.Config) error {
		return saveProviderConfig(c, p)
	})
}

// SetProviderWebSearch updates every provider represented by one Settings
// access card in a single config transaction. Legacy DeepSeek aliases can
// remain separate when their custom transport fields differ, so changing only
// the first profile would leave the grouped control in a contradictory state.
func (a *App) SetProviderWebSearch(names []string, enabled bool) error {
	return a.applyGlobalProviderConfigChange(
		"DeepSeek server-side web search",
		"set-provider-web-search",
		"changing DeepSeek server-side web search",
		func(c *config.Config) error {
			seen := make(map[string]bool, len(names))
			providers := make([]*config.ProviderEntry, 0, len(names))
			for _, rawName := range names {
				name := strings.TrimSpace(rawName)
				if name == "" || seen[name] {
					continue
				}
				seen[name] = true
				entry, ok := c.Provider(name)
				if !ok {
					return fmt.Errorf("provider %q not found", name)
				}
				if !config.IsOfficialDeepSeekWebSearchEndpoint(entry) {
					return fmt.Errorf("provider %q does not support configurable server-side web search", name)
				}
				providers = append(providers, entry)
			}
			if len(providers) == 0 {
				return fmt.Errorf("provider list is empty")
			}
			for _, entry := range providers {
				value := enabled
				entry.WebSearch = &value
			}
			return nil
		})
}

func providerModelOverridesForCatalog(overrides map[string]config.ProviderModelOverride, models []string) map[string]config.ProviderModelOverride {
	if len(overrides) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(models))
	for _, model := range models {
		allowed[model] = true
	}
	filtered := make(map[string]config.ProviderModelOverride, len(overrides))
	for model, override := range overrides {
		if allowed[model] {
			filtered[model] = override
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

func applyProviderModelCatalogUpdate(c *config.Config, update ProviderModelCatalogUpdate, credentialsRevision string) (bool, error) {
	if c == nil {
		return false, fmt.Errorf("config is nil")
	}
	current, ok := c.Provider(strings.TrimSpace(update.Name))
	if !ok || strings.TrimSpace(update.ExpectedFingerprint) == "" ||
		providerModelCatalogFingerprintForCredentials(*current, credentialsRevision) != strings.TrimSpace(update.ExpectedFingerprint) {
		return false, nil
	}
	models := chatProviderModels(update.Models)
	if len(models) == 0 {
		return false, fmt.Errorf("provider %q model catalog is empty", update.Name)
	}

	next := *current
	visionConfigured := next.Vision || next.VisionModels != nil
	next.Model = models[0]
	next.Models = models
	next.Default = ""
	if len(models) > 1 {
		next.Default = providerDefaultForModels(update.Default, models)
	}
	next.Vision = false
	if visionConfigured {
		next.VisionModels = providerVisionModels(models, update.VisionModels)
	} else {
		next.VisionModels = nil
	}
	next.ModelOverrides = providerModelOverridesForCatalog(next.ModelOverrides, models)
	if config.ProviderEntriesConfigEqual(*current, next) {
		return false, nil
	}
	if err := c.UpsertProvider(next); err != nil {
		return false, err
	}
	return true, nil
}

// SaveProviderModelCatalogs applies only model-catalog fields. Each update is
// compared against the provider snapshot that launched discovery while the
// config edit lock is held, so an older async completion cannot overwrite newer
// provider edits. Stale updates are skipped rather than treated as failures.
func (a *App) SaveProviderModelCatalogs(updates []ProviderModelCatalogUpdate) ([]string, error) {
	if len(updates) == 0 {
		return []string{}, nil
	}
	if err := a.ensureActiveTabRebuildAllowed("provider model catalogs"); err != nil {
		return []string{}, err
	}
	applied := make([]string, 0, len(updates))
	if err := func() error {
		unlock := config.LockUserConfigEdits()
		defer unlock()
		cfg, path, err := a.loadDesktopUserConfigForEdit()
		if err != nil {
			return err
		}
		observedCredentialsRevision := providerCredentialsRevision()
		if a.providerCatalogBeforeCredentialLockHook != nil {
			a.providerCatalogBeforeCredentialLockHook(observedCredentialsRevision)
		}
		unlockCredentials, err := config.LockUserCredentialEdits()
		if err != nil {
			return err
		}
		defer unlockCredentials()

		credentialsRevision := providerCredentialsRevision()
		for _, update := range updates {
			changed, err := applyProviderModelCatalogUpdate(cfg, update, credentialsRevision)
			if err != nil {
				return err
			}
			if changed {
				applied = append(applied, strings.TrimSpace(update.Name))
			}
		}
		if len(applied) == 0 {
			return nil
		}
		return cfg.SaveTo(path)
	}(); err != nil {
		return []string{}, err
	}
	if len(applied) == 0 {
		return applied, nil
	}
	if err := a.rebuildSetting("provider model catalogs"); err != nil {
		if _, ok := a.deferredRebuildWarning("provider model catalogs", err); ok {
			return applied, nil
		}
		return []string{}, err
	}
	return applied, nil
}

// SaveProviderWithKey saves a custom provider and its credential as one settings
// transaction, then rebuilds once after both are visible to the runtime.
func (a *App) SaveProviderWithKey(p ProviderView, key string) (string, error) {
	apiKeyEnv := strings.TrimSpace(p.APIKeyEnv)
	if apiKeyEnv == "" {
		return "", fmt.Errorf("this provider has no api_key_env set")
	}
	if err := a.ensureActiveTabRebuildAllowed("provider"); err != nil {
		return "", err
	}
	warning, err := a.saveProviderCredential(apiKeyEnv, key)
	if err != nil {
		return "", err
	}
	if err := func() error {
		unlock := config.LockUserConfigEdits()
		defer unlock()
		cfg, path, err := a.loadDesktopUserConfigForEdit()
		if err != nil {
			return err
		}
		if err := saveProviderConfig(cfg, p); err != nil {
			return err
		}
		return cfg.SaveTo(path)
	}(); err != nil {
		return "", err
	}
	if err := a.rebuildSetting("provider"); err != nil {
		if rebuildWarning, ok := a.deferredRebuildWarning("provider", err); ok {
			return appendSettingsWarning(warning, rebuildWarning), nil
		}
		return "", err
	}
	return warning, nil
}
