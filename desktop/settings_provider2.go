package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"reasonix/internal/config"
)

// UpgradeDeepSeekProviderAccess applies the explicit Settings action for an
// official legacy OpenAI entry. The config package performs a narrow raw-TOML
// edit so unrelated and future fields are not lost to a full config render.
func (a *App) UpgradeDeepSeekProviderAccess(name string) (string, error) {
	const setting = "DeepSeek provider protocol"
	defer a.lockRuntimeMutation("upgrade-deepseek-provider")()
	releaseGates, err := a.lockRuntimeTurnGates(setting, nil)
	if err != nil {
		return "", err
	}
	defer releaseGates()
	visibleTabs, err := a.visibleTabsForGlobalRuntimeMutation("upgrading the DeepSeek provider protocol")
	if err != nil {
		return "", err
	}

	changed, err := config.UpgradeDeepSeekProviderProtocolUserConfig(name)
	if err != nil {
		return "", err
	}
	if !changed {
		return "", fmt.Errorf("DeepSeek provider %q is not eligible for the recommended protocol upgrade", name)
	}
	warning := ""
	var rebuildErrs []error
	for _, tab := range visibleTabs {
		if a.controllerForTab(tab) == nil {

			continue
		}
		if err := a.rebuildSettingTurnLocked(setting, tab, true, false); err != nil {
			if deferred, ok := a.deferredRebuildWarningForTab(setting, err, tab); ok {
				if warning == "" {
					warning = deferred
				}
				continue
			}

			rebuildErrs = append(rebuildErrs, err)
		}
	}
	return warning, errors.Join(rebuildErrs...)
}

// visibleTabsForGlobalRuntimeUpgrade returns every visible runtime that must
// observe a user-global provider protocol change. A detached controller cannot
// use rebuildSettingTurnLocked because it is intentionally absent from a.tabs;
// reject before mutating config so it never remains silently pinned to the old
// protocol. runtime mutation admission and all turn gates are held by callers.
func (a *App) visibleTabsForGlobalRuntimeMutation(detachedAction string) ([]*WorkspaceTab, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tab := range a.detachedSessions {
		if tab != nil && tab.Ctrl != nil {
			return nil, fmt.Errorf("background session is still open; reopen or close it before %s", detachedAction)
		}
	}
	tabs := make([]*WorkspaceTab, 0, len(a.tabs))
	for _, id := range a.orderedTabIDsLocked() {
		if tab := a.tabs[id]; tab != nil {
			tabs = append(tabs, tab)
		}
	}
	return tabs, nil
}

// AddProviderPresetAccess installs one editable custom-provider preset. Unlike
// official built-ins, these entries are saved as normal providers so users can
// tweak endpoints, model lists, and capability overrides after the one-click
// setup path.
func (a *App) AddProviderPresetAccess(id, key string) (string, error) {
	preset, ok := config.CuratedProviderPreset(id)
	if !ok {
		return "", fmt.Errorf("unknown provider preset %q", id)
	}
	if len(preset.Entries) == 0 {
		return "", fmt.Errorf("provider preset %q has no provider entries", id)
	}
	if err := a.ensureActiveTabRebuildAllowed("provider access"); err != nil {
		return "", err
	}

	cfg, _, err := a.loadDesktopUserConfigForView()
	if err != nil {
		return "", err
	}
	if existing := existingProviderNames(cfg, preset.Entries); len(existing) > 0 {
		return "", providerPresetAlreadyAddedError(preset.ID, existing)
	}
	keyEnv := strings.TrimSpace(preset.KeyEnv)
	if keyEnv == "" {
		for _, e := range preset.Entries {
			if keyEnv = strings.TrimSpace(e.APIKeyEnv); keyEnv != "" {
				break
			}
		}
	}
	keyWarning := ""
	if strings.TrimSpace(key) != "" && keyEnv != "" {
		var err error
		keyWarning, err = a.saveProviderCredential(keyEnv, key)
		if err != nil {
			return "", err
		}
	}
	rebuildWarning, err := a.applyConfigChangeWithWarning("provider access", func(c *config.Config) error {
		if existing := existingProviderNames(c, preset.Entries); len(existing) > 0 {
			return providerPresetAlreadyAddedError(preset.ID, existing)
		}
		names := make([]string, 0, len(preset.Entries))
		for _, e := range preset.Entries {
			if err := c.UpsertProvider(e); err != nil {
				return err
			}
			names = append(names, e.Name)
		}
		addProviderAccess(c, names...)
		return nil
	})
	if err != nil {
		return "", err
	}
	return appendSettingsWarning(keyWarning, rebuildWarning), nil
}

// ResetProviderPresetAccess intentionally overwrites same-name provider entries
// with the curated preset template. It only mutates config; provider secrets stay
// in Reasonix home .env under whichever api_key_env the resulting preset uses.
func (a *App) ResetProviderPresetAccess(id string) error {
	preset, ok := config.CuratedProviderPreset(id)
	if !ok {
		return fmt.Errorf("unknown provider preset %q", id)
	}
	if len(preset.Entries) == 0 {
		return fmt.Errorf("provider preset %q has no provider entries", id)
	}
	if err := a.ensureActiveTabRebuildAllowed("provider access"); err != nil {
		return err
	}

	cfg, _, err := a.loadDesktopUserConfigForView()
	if err != nil {
		return err
	}
	if existing := existingProviderNames(cfg, preset.Entries); len(existing) == 0 {
		return providerPresetNoExistingProviderError(preset.ID)
	}
	return a.applyConfigChange(func(c *config.Config) error {
		if existing := existingProviderNames(c, preset.Entries); len(existing) == 0 {
			return providerPresetNoExistingProviderError(preset.ID)
		}
		names := make([]string, 0, len(preset.Entries))
		for _, e := range preset.Entries {
			if err := c.UpsertProvider(e); err != nil {
				return err
			}
			names = append(names, e.Name)
		}
		addProviderAccess(c, names...)
		return nil
	})
}

func existingProviderNames(c *config.Config, entries []config.ProviderEntry) []string {
	if c == nil || len(entries) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := strings.TrimSpace(entry.Name)
		if name == "" {
			continue
		}
		if _, ok := c.Provider(name); ok {
			names = append(names, name)
		}
	}
	return names
}

func providerPresetAlreadyAddedError(id string, names []string) error {
	return fmt.Errorf("provider preset %q cannot be added because provider name(s) already exist: %s; edit, rename, or remove the existing provider before adding it again", id, strings.Join(names, ", "))
}

func providerPresetNoExistingProviderError(id string) error {
	return fmt.Errorf("provider preset %q cannot be reset because no same-name provider exists; add the preset instead", id)
}

// FetchProviderModels probes the provider's OpenAI-compatible model-list
// endpoint and returns the available model IDs. This is a settings-only helper:
// it never touches chat request serialization or provider-visible prompt data.
func (a *App) FetchProviderModels(p ProviderView) ([]string, error) {
	e := config.ProviderEntry{
		Name:       p.Name,
		Kind:       p.Kind,
		BaseURL:    p.BaseURL,
		ModelsURL:  strings.TrimSpace(p.ModelsURL),
		APIKeyEnv:  p.APIKeyEnv,
		Headers:    p.Headers,
		AuthHeader: p.AuthHeader,
	}
	e.ResolveAPIKeyForRoot(a.activeWorkspaceRoot())
	ctx, cancel := context.WithTimeout(a.reqCtx(), 15*time.Second)
	defer cancel()
	models, err := e.FetchModels(ctx)
	if err != nil {
		return []string{}, err
	}
	return nonNil(chatProviderModels(models)), nil
}

// FetchAllProviderModels fetches model lists for all providers in a single
// batch. Models are fetched concurrently (up to 4 parallel requests) and
// returned as a map keyed by provider name. Errors for individual providers
// are recorded as nil entries; callers should handle missing keys.
func (a *App) FetchAllProviderModels(providers []ProviderView) map[string][]string {
	results := make(map[string][]string, len(providers))
	var mu sync.Mutex
	g, ctx := errgroup.WithContext(a.reqCtx())
	g.SetLimit(4)
	root := a.activeWorkspaceRoot()
	for i := range providers {
		p := providers[i]
		g.Go(func() error {
			e := config.ProviderEntry{
				Name:       p.Name,
				Kind:       p.Kind,
				BaseURL:    p.BaseURL,
				ModelsURL:  strings.TrimSpace(p.ModelsURL),
				APIKeyEnv:  p.APIKeyEnv,
				Headers:    p.Headers,
				AuthHeader: p.AuthHeader,
			}
			e.ResolveAPIKeyForRoot(root)
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			models, err := e.FetchModels(ctx)
			if err != nil {

				return nil
			}
			mu.Lock()
			defer mu.Unlock()
			results[p.Name] = nonNil(chatProviderModels(models))
			return nil
		})
	}
	_ = g.Wait()
	return results
}

// rebuildActiveSettingRuntimeMutationLocked refreshes the active controller
// while lockRuntimeMutation and all runtime turn gates are held.
func (a *App) rebuildActiveSettingRuntimeMutationLocked(setting string) error {
	tab := a.activeTab()
	if tab == nil {
		if a.ctx == nil {
			return nil
		}
		return fmt.Errorf("no active tab")
	}
	return a.rebuildSettingTurnLocked(setting, tab, true, false)
}

// SetProviderKey writes a secret to Reasonix's global .env under the given
// env-var name (the one a provider's api_key_env points at) and rebuilds so it
// resolves immediately.
func (a *App) SetProviderKey(apiKeyEnv, value string) (string, error) {
	if strings.TrimSpace(apiKeyEnv) == "" {
		return "", fmt.Errorf("this provider has no api_key_env set")
	}
	if err := a.ensureActiveTabRebuildAllowed("provider key"); err != nil {
		return "", err
	}
	warning, err := a.saveProviderCredential(apiKeyEnv, value)
	if err != nil {
		return "", err
	}
	if err := a.ensureProviderAccessForKey(apiKeyEnv); err != nil {
		return "", err
	}
	if err := a.rebuildSetting("provider key"); err != nil {
		if rebuildWarning, ok := a.deferredRebuildWarning("provider key", err); ok {
			return appendSettingsWarning(warning, rebuildWarning), nil
		}
		return "", err
	}
	return warning, nil
}

// SaveProviderKey writes a provider secret without rebuilding the chat runtime.
// It is used by settings probes that need credentials only for a model-list
// request; explicit "save key" actions still call SetProviderKey.
func (a *App) SaveProviderKey(apiKeyEnv, value string) (string, error) {
	if strings.TrimSpace(apiKeyEnv) == "" {
		return "", fmt.Errorf("this provider has no api_key_env set")
	}
	return a.saveProviderCredential(apiKeyEnv, value)
}

func (a *App) ensureProviderAccessForKey(apiKeyEnv string) error {
	apiKeyEnv = strings.TrimSpace(apiKeyEnv)
	if apiKeyEnv == "" {
		return nil
	}

	unlock := config.LockUserConfigEdits()
	defer unlock()
	cfg, path, err := a.loadDesktopUserConfigForEdit()
	if err != nil {
		return err
	}
	access := providerAccessSet(cfg.Desktop.ProviderAccess)
	changed := false
	addAccess := func(name string) {
		if name == "" || access[name] {
			return
		}
		addProviderAccess(cfg, name)
		access[name] = true
		changed = true
	}
	for i := range cfg.Providers {
		p := cfg.Providers[i]
		if strings.TrimSpace(p.APIKeyEnv) != apiKeyEnv {
			continue
		}
		if len(p.ModelList()) == 0 {
			continue
		}
		if isOfficialBuiltInProvider(p) {
			addAccess(config.CanonicalDesktopOfficialProviderName(p.Name))
		} else {
			addAccess(strings.TrimSpace(p.Name))
		}
	}
	if !changed && apiKeyEnv == "DEEPSEEK_API_KEY" {
		entries, _, err := officialProviderTemplate("deepseek", cfg.DeepSeekOfficialPricingLanguage())
		if err != nil {
			return err
		}
		for _, e := range entries {
			if err := cfg.UpsertProvider(e); err != nil {
				return err
			}
			addAccess(e.Name)
		}
	}
	if !changed {
		return nil
	}
	return cfg.SaveTo(path)
}

// ClearProviderKey removes a provider secret from Reasonix's global .env
// and rebuilds so the provider immediately becomes unauthenticated.
func (a *App) ClearProviderKey(apiKeyEnv string) error {
	if strings.TrimSpace(apiKeyEnv) == "" {
		return fmt.Errorf("this provider has no api_key_env set")
	}
	if err := a.ensureActiveTabRebuildAllowed("provider key"); err != nil {
		return err
	}
	if err := removeDotEnv(apiKeyEnv); err != nil {
		return err
	}
	if err := a.rebuildSetting("provider key"); err != nil {
		if _, ok := a.deferredRebuildWarning("provider key", err); ok {
			return nil
		}
		return err
	}
	return nil
}
