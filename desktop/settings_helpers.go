package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"net/url"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"reasonix/internal/config"
)

// shadowingConfigPath returns the config file that outranks writePath for the
// workspace at root, or "" when writePath is the one in effect. A project
// reasonix.toml beats the user config, so settings written here would otherwise
// look ignored (#4333).
func shadowingConfigPath(writePath, root string) string {
	effective := config.SourcePathForRoot(root)
	if effective == "" || samePath(effective, writePath) {
		return ""
	}
	if abs, err := filepath.Abs(effective); err == nil {
		return abs
	}
	return effective
}

func samePath(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return a == b
	}
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(absA), filepath.Clean(absB))
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func nonNilStringMap(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

func nonNilAnyMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	return m
}

func providerCredentialsRevision() string {
	return config.CredentialStoreRevision()
}

var providerStateFingerprintKey = func() []byte {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(fmt.Sprintf("initialize provider state fingerprint key: %v", err))
	}
	return key
}()

func providerModelCatalogFingerprintForCredentials(p config.ProviderEntry, credentialsRevision string) string {

	h := hmac.New(sha256.New, providerStateFingerprintKey)
	write := func(value string) {
		_, _ = fmt.Fprintf(h, "%d:", len(value))
		_, _ = h.Write([]byte(value))
	}
	write("provider-model-catalog-v1")
	write("name")
	write(p.Name)
	write("kind")
	write(p.Kind)
	write("base_url")
	write(p.BaseURL)
	write("models_url")
	write(p.ModelsURL)
	write("api_key_env")
	write(p.APIKeyEnv)
	write("credentials_revision")
	write(credentialsRevision)
	write("auth_header")
	write(fmt.Sprintf("%t", p.AuthHeader))
	keys := make([]string, 0, len(p.Headers))
	for key := range p.Headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	write("headers")
	write(fmt.Sprintf("%d", len(keys)))
	for _, key := range keys {
		write(key)
		write(p.Headers[key])
	}
	write("model")
	write(p.Model)
	write("models")
	write(fmt.Sprintf("%d", len(p.Models)))
	for _, model := range p.Models {
		write(model)
	}
	write("default")
	write(p.Default)
	write("vision")
	write(fmt.Sprintf("%t", p.Vision))
	write("vision_models")
	write(fmt.Sprintf("%d", len(p.VisionModels)))
	for _, model := range p.VisionModels {
		write(model)
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func providerModelOverridesForView(overrides map[string]config.ProviderModelOverride, models []string) []ProviderModelOverrideView {
	if len(overrides) == 0 {
		return []ProviderModelOverrideView{}
	}
	modelSet := map[string]bool{}
	for _, model := range models {
		modelSet[model] = true
	}
	keys := make([]string, 0, len(overrides))
	for model := range overrides {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if len(modelSet) > 0 && !modelSet[model] {
			continue
		}
		keys = append(keys, model)
	}
	sort.Strings(keys)
	out := make([]ProviderModelOverrideView, 0, len(keys))
	for _, model := range keys {
		ov := overrides[model]
		out = append(out, ProviderModelOverrideView{
			Model:             model,
			ReasoningProtocol: ov.ReasoningProtocol,
			SupportedEfforts:  nonNil(ov.SupportedEfforts),
			DefaultEffort:     ov.DefaultEffort,
			Vision:            ov.Vision,
			ContextWindow:     ov.ContextWindow,
			MaxOutputTokens:   ov.MaxOutputTokens,
		})
	}
	return out
}

func providerModelOverridesForSave(overrides []ProviderModelOverrideView, models []string) map[string]config.ProviderModelOverride {
	if len(overrides) == 0 {
		return nil
	}
	modelSet := map[string]bool{}
	for _, model := range models {
		modelSet[model] = true
	}
	out := map[string]config.ProviderModelOverride{}
	for _, item := range overrides {
		model := strings.TrimSpace(item.Model)
		if model == "" || (len(modelSet) > 0 && !modelSet[model]) {
			continue
		}
		ov := config.ProviderModelOverride{
			ReasoningProtocol: strings.TrimSpace(item.ReasoningProtocol),
			SupportedEfforts:  nonNil(item.SupportedEfforts),
			DefaultEffort:     strings.TrimSpace(item.DefaultEffort),
			Vision:            item.Vision,
			ContextWindow:     max(item.ContextWindow, 0),
			MaxOutputTokens:   item.MaxOutputTokens,
		}
		if strings.TrimSpace(ov.ReasoningProtocol) == "" && len(ov.SupportedEfforts) == 0 && strings.TrimSpace(ov.DefaultEffort) == "" && ov.Vision == nil && ov.ContextWindow == 0 && ov.MaxOutputTokens == 0 {
			continue
		}
		out[model] = ov
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func desktopModelRefsProvider(c *config.Config, ref, name string) bool {
	if config.ModelRefsProvider(ref, name) {
		return true
	}
	if e, ok := c.ResolveModel(ref); ok {
		return e.Name == name
	}
	return false
}

func officialProviderHost(baseURL string) string {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func officialProviderKindFromEntry(p config.ProviderEntry) string {
	host := officialProviderHost(p.BaseURL)
	switch config.CanonicalDesktopOfficialProviderName(p.Name) {
	case "deepseek":
		if host == "api.deepseek.com" {
			return "deepseek"
		}
	}
	return ""
}

func isOfficialBuiltInProvider(p config.ProviderEntry) bool {
	return officialProviderKindFromEntry(p) != ""
}

func providerAccessSet(names []string) map[string]bool {
	out := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" {
			out[name] = true
		}
	}
	return out
}

func providerEnabled(c *config.Config, name string) bool {
	if c == nil {
		return true
	}
	return !providerAccessSet(c.Desktop.ProviderDisabled)[strings.TrimSpace(name)]
}

func addProviderAccess(c *config.Config, names ...string) {
	seen := providerAccessSet(c.Desktop.ProviderAccess)
	removeProviderDisabled(c, names...)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		c.Desktop.ProviderAccess = append(c.Desktop.ProviderAccess, name)
		seen[name] = true
	}
}

func removeProviderAccess(c *config.Config, names ...string) {
	remove := providerAccessSet(names)
	if len(remove) == 0 {
		return
	}
	out := c.Desktop.ProviderAccess[:0]
	for _, name := range c.Desktop.ProviderAccess {
		if !remove[name] {
			out = append(out, name)
		}
	}
	c.Desktop.ProviderAccess = out
	removeProviderDisabled(c, names...)
}

func addProviderDisabled(c *config.Config, names ...string) {
	seen := providerAccessSet(c.Desktop.ProviderDisabled)
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		c.Desktop.ProviderDisabled = append(c.Desktop.ProviderDisabled, name)
		seen[name] = true
	}
}

func removeProviderDisabled(c *config.Config, names ...string) {
	remove := providerAccessSet(names)
	if len(remove) == 0 || len(c.Desktop.ProviderDisabled) == 0 {
		return
	}
	out := c.Desktop.ProviderDisabled[:0]
	for _, name := range c.Desktop.ProviderDisabled {
		if !remove[name] {
			out = append(out, name)
		}
	}
	c.Desktop.ProviderDisabled = out
}

func providerViewFromEntryForRootWithResolverAndCredentials(p config.ProviderEntry, builtIn, added bool, root string, resolver *config.CredentialResolver, credentialsRevision string) ProviderView {
	models := p.ChatModelList()
	visionModels := p.VisionModels
	visionModelsSet := p.Vision || p.VisionModels != nil
	if p.Vision {
		visionModels = models
	}
	if resolver == nil {
		resolver = config.NewCredentialResolverForRoot(root)
	}
	key := resolver.ResolveGlobalFirst(p.APIKeyEnv)
	requiresKey := p.RequiresAPIKey()
	visionCapability := "configurable"
	if !config.CanConfigureVision(&p) {
		visionCapability = "unsupported"
	}
	return ProviderView{
		Name: p.Name, BuiltIn: builtIn, Added: added, Enabled: providerEnabled(nil, p.Name), Kind: p.Kind, BaseURL: p.BaseURL, ChatURL: p.ChatURL, RequestURL: p.RequestURL,
		Models: nonNil(models), VisionModels: nonNil(providerVisionModels(models, visionModels)), VisionModelsSet: visionModelsSet, VisionCapability: visionCapability, ModelsURL: p.ModelsURL, Default: p.DefaultModel(),
		APIKeyEnv:                   p.APIKeyEnv,
		Headers:                     nonNilStringMap(p.Headers),
		ExtraBody:                   nonNilAnyMap(p.ExtraBody),
		AuthHeader:                  p.AuthHeader,
		KeySet:                      key.Set,
		RequiresKey:                 requiresKey,
		Configured:                  !requiresKey || key.Set,
		KeySource:                   key.Source.Label,
		KeySourcePath:               key.Source.Path,
		BalanceURL:                  p.BalanceURL,
		ContextWindow:               p.ContextWindow,
		ReasoningProtocol:           p.ReasoningProtocol,
		Thinking:                    providerThinkingForSettings(p.Thinking),
		WebSearch:                   config.EffectiveWebSearch(&p),
		ServerWebSearchCapability:   config.HasServerWebSearchCapability(&p),
		SupportedEfforts:            nonNil(p.SupportedEfforts),
		DefaultEffort:               p.DefaultEffort,
		ModelOverrides:              providerModelOverridesForView(p.ModelOverrides, models),
		RecommendedUpgradeAvailable: config.CanUpgradeDeepSeekProviderProtocol(&p),
		ModelCatalogFingerprint:     providerModelCatalogFingerprintForCredentials(p, credentialsRevision),
	}
}

func providerThinkingForSettings(thinking string) string {
	normalized := strings.ToLower(strings.TrimSpace(thinking))
	switch normalized {
	case "enabled", "disabled", "adaptive":
		return normalized
	default:
		return ""
	}
}

func officialProviderViews(added map[string]bool, pricingLanguage string) []ProviderView {
	return officialProviderViewsForRoot(added, pricingLanguage, ".")
}

func officialProviderViewsForRoot(added map[string]bool, pricingLanguage, root string) []ProviderView {
	return officialProviderViewsForRootWithResolver(added, pricingLanguage, root, nil)
}

func officialProviderViewsForRootWithResolver(added map[string]bool, pricingLanguage, root string, resolver *config.CredentialResolver) []ProviderView {
	var out []ProviderView
	if resolver == nil {
		resolver = config.NewCredentialResolverForRoot(root)
	}
	credentialsRevision := providerCredentialsRevision()
	for _, kind := range []string{"deepseek"} {
		entries, _, err := officialProviderTemplate(kind, pricingLanguage)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			out = append(out, providerViewFromEntryForRootWithResolverAndCredentials(entry, true, added[entry.Name], root, resolver, credentialsRevision))
		}
	}
	return out
}

func providerPresetViewsForRootWithResolver(cfg *config.Config, root string, resolver *config.CredentialResolver) []ProviderPresetView {
	if resolver == nil {
		resolver = config.NewCredentialResolverForRoot(root)
	}
	presets := config.CuratedProviderPresets()
	out := make([]ProviderPresetView, 0, len(presets))
	for _, preset := range presets {
		keyEnv := strings.TrimSpace(preset.KeyEnv)
		names := make([]string, 0, len(preset.Entries))
		models := make([]string, 0)
		modelSeen := map[string]bool{}
		requiresKey := false
		for _, entry := range preset.Entries {
			if keyEnv == "" {
				keyEnv = strings.TrimSpace(entry.APIKeyEnv)
			}
			if entry.RequiresAPIKey() {
				requiresKey = true
			}
			name := strings.TrimSpace(entry.Name)
			if name != "" {
				names = append(names, name)
			}
			for _, model := range chatProviderModels(entry.ChatModelList()) {
				if modelSeen[model] {
					continue
				}
				modelSeen[model] = true
				models = append(models, model)
			}
		}
		key := config.CredentialResolution{}
		if keyEnv != "" {
			key = resolver.ResolveGlobalFirst(keyEnv)
		}
		status, statusNames := classifyProviderPresetStatus(cfg, preset)
		added := status == providerPresetStatusInstalled || status == providerPresetStatusInstalledModified || status == providerPresetStatusNameConflict
		out = append(out, ProviderPresetView{
			ID:                  preset.ID,
			Label:               preset.Label,
			Description:         preset.Description,
			KeyEnv:              keyEnv,
			ProviderNames:       nonNil(names),
			Models:              nonNil(models),
			Added:               added,
			Status:              status,
			StatusProviderNames: nonNil(statusNames),
			KeySet:              key.Set,
			RequiresKey:         requiresKey,
			Configured:          !requiresKey || key.Set,
			KeySource:           key.Source.Label,
			KeySourcePath:       key.Source.Path,
		})
	}
	return out
}

func classifyProviderPresetStatus(cfg *config.Config, preset config.ProviderPreset) (string, []string) {
	if cfg == nil {
		return providerPresetStatusAvailable, nil
	}
	installed := make([]string, 0)
	modified := make([]string, 0)
	conflicts := make([]string, 0)
	similar := make([]string, 0)
	presetID := strings.TrimSpace(preset.ID)
	for _, entry := range preset.Entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		existing, ok := cfg.Provider(name)
		if !ok {
			continue
		}
		if providerEntryMatchesPreset(*existing, entry, presetID) {
			installed = append(installed, name)
		} else if providerEntryUsesPresetID(*existing, presetID) {
			modified = append(modified, name)
		} else {
			conflicts = append(conflicts, name)
		}
	}
	if len(conflicts) > 0 {
		return providerPresetStatusNameConflict, uniqueNonEmptyStrings(conflicts)
	}
	if len(modified) > 0 {
		return providerPresetStatusInstalledModified, uniqueNonEmptyStrings(modified)
	}
	if len(installed) > 0 {
		return providerPresetStatusInstalled, uniqueNonEmptyStrings(installed)
	}
	for i := range cfg.Providers {
		existing := cfg.Providers[i]
		existingName := strings.TrimSpace(existing.Name)
		if existingName == "" {
			continue
		}
		for _, entry := range preset.Entries {
			if existingName == strings.TrimSpace(entry.Name) {
				continue
			}
			if providerEntrySimilarToPreset(existing, entry, presetID) {
				similar = append(similar, existingName)
				break
			}
		}
	}
	if len(similar) > 0 {
		return providerPresetStatusSimilarExisting, uniqueNonEmptyStrings(similar)
	}
	return providerPresetStatusAvailable, nil
}

func providerEntryMatchesPreset(existing, preset config.ProviderEntry, presetID string) bool {
	if strings.TrimSpace(existing.PresetID) != "" {
		if providerEntryUsesPresetID(existing, presetID) {
			return providerEntryCoreMatches(existing, preset)
		}
		return false
	}
	return providerEntryCoreMatches(existing, preset)
}

func providerEntrySimilarToPreset(existing, preset config.ProviderEntry, presetID string) bool {
	if providerEntryUsesPresetID(existing, presetID) {
		return true
	}
	return providerEntryCoreMatches(existing, preset)
}

func providerEntryUsesPresetID(existing config.ProviderEntry, presetID string) bool {
	presetID = strings.TrimSpace(presetID)
	return presetID != "" && strings.TrimSpace(existing.PresetID) == presetID
}

func providerEntryCoreMatches(existing, preset config.ProviderEntry) bool {
	return strings.EqualFold(strings.TrimSpace(existing.Kind), strings.TrimSpace(preset.Kind)) &&
		normalizeProviderURL(existing.BaseURL) == normalizeProviderURL(preset.BaseURL) &&
		strings.TrimSpace(existing.ChatURL) == strings.TrimSpace(preset.ChatURL) &&
		strings.TrimSpace(existing.RequestURL) == strings.TrimSpace(preset.RequestURL) &&
		strings.TrimSpace(existing.APIKeyEnv) == strings.TrimSpace(preset.APIKeyEnv) &&
		existing.AuthHeader == preset.AuthHeader
}

func normalizeProviderURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err == nil && u.Scheme != "" && u.Host != "" {
		u.Scheme = strings.ToLower(u.Scheme)
		u.Host = strings.ToLower(u.Host)
		u.Path = strings.TrimRight(u.Path, "/")
		u.RawPath = ""
		u.RawQuery = ""
		u.Fragment = ""
		return strings.TrimRight(u.String(), "/")
	}
	return strings.TrimRight(raw, "/")
}

func uniqueNonEmptyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func officialProviderAddedSet(cfg *config.Config) map[string]bool {
	out := map[string]bool{}
	if cfg == nil {
		return out
	}
	access := providerAccessSet(cfg.Desktop.ProviderAccess)
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		if !access[p.Name] {
			continue
		}
		if kind := officialProviderKindFromEntry(p); kind != "" {
			out[kind] = true
		}
	}
	return out
}
