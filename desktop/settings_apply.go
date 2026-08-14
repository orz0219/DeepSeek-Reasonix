package main

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/config"
)

// applyConfigChange mutates the user-global config and rebuilds the controller so
// the change takes effect this session. Desktop settings such as providers and
// keys are account-level, not per-project: writing them to the global config
// rather than the cwd's reasonix.toml is what lets them survive a workspace switch.
func (a *App) applyConfigChange(mutate func(*config.Config) error) error {
	_, err := a.applyConfigChangeWithWarning("settings", mutate)
	return err
}

// applySkillConfigChange edits the config file that owns the selected [skills]
// field. Project skill settings shadow the global setting at runtime, so
// writing only the user config would make the UI appear to save while the
// active project continued using its old value.
func (a *App) applySkillConfigChange(field, setting string, mutate func(*config.Config) error) error {
	return a.applySkillConfigChangeForFields([]string{field}, setting, mutate)
}

func (a *App) applySkillConfigChangeForFields(fields []string, setting string, mutate func(*config.Config) error) error {
	workspaceRoot := a.activeWorkspaceRoot()
	projectPath := config.SourcePathForRoot(workspaceRoot)
	projectOwned := strings.TrimSpace(projectPath) != "" && !config.IsUserConfigPath(projectPath)
	if projectOwned {
		projectOwned = slices.ContainsFunc(fields, func(field string) bool {
			return config.ConfigFileDefinesSkillKey(projectPath, field)
		})
	}
	if !projectOwned {
		return a.applyConfigChange(mutate)
	}
	if err := a.ensureActiveTabRebuildAllowed(setting); err != nil {
		return err
	}
	if err := func() error {
		unlock, err := config.LockConfigFileEdits(projectPath)
		if err != nil {
			return err
		}
		defer unlock()
		cfg, err := config.LoadForEditWithoutCredentialsReadOnlyStrict(projectPath)
		if err != nil {
			return err
		}
		if err := mutate(cfg); err != nil {
			return err
		}
		for _, field := range fields {
			if err := cfg.KeepProjectSkillKey(field); err != nil {
				return err
			}
		}
		return cfg.SaveTo(projectPath)
	}(); err != nil {
		return err
	}
	if err := a.rebuildSetting(setting); err != nil {
		if _, ok := a.deferredRebuildWarning(setting, err); ok {
			return nil
		}
		return err
	}
	return nil
}

func (a *App) applyConfigChangeWithWarning(setting string, mutate func(*config.Config) error) (string, error) {
	if err := a.ensureActiveTabRebuildAllowed(setting); err != nil {
		return "", err
	}
	if err := func() error {

		unlock := config.LockUserConfigEdits()
		defer unlock()
		cfg, path, err := a.loadDesktopUserConfigForEdit()
		if err != nil {
			return err
		}
		if err := mutate(cfg); err != nil {
			return err
		}
		return cfg.SaveTo(path)
	}(); err != nil {
		return "", err
	}
	if err := a.rebuildSetting(setting); err != nil {
		if warning, ok := a.deferredRebuildWarning(setting, err); ok {
			return warning, nil
		}
		return "", err
	}
	return "", nil
}

// applyGlobalProviderConfigChange persists a provider-wide setting and refreshes
// every visible runtime while runtime admission and all turn gates are frozen.
// Detached runtimes cannot participate in the failure-atomic build-and-swap, so
// reject before writing instead of leaving one session on stale provider state.
func (a *App) applyGlobalProviderConfigChange(setting, operation, detachedAction string, mutate func(*config.Config) error) error {
	defer a.lockRuntimeMutation(operation)()
	releaseGates, err := a.lockRuntimeTurnGates(setting, nil)
	if err != nil {
		return err
	}
	defer releaseGates()
	tabs, err := a.visibleTabsForGlobalRuntimeMutation(detachedAction)
	if err != nil {
		return err
	}
	if err := func() error {
		unlock := config.LockUserConfigEdits()
		defer unlock()
		cfg, path, err := a.loadDesktopUserConfigForEdit()
		if err != nil {
			return err
		}
		if err := mutate(cfg); err != nil {
			return err
		}
		return cfg.SaveTo(path)
	}(); err != nil {
		return err
	}

	var rebuildErrs []error
	for _, tab := range tabs {
		if a.controllerForTab(tab) == nil {

			continue
		}
		if err := a.rebuildSettingTurnLocked(setting, tab, true, false); err != nil {
			if _, ok := a.deferredRebuildWarningForTab(setting, err, tab); ok {
				continue
			}
			rebuildErrs = append(rebuildErrs, err)
		}
	}
	return errors.Join(rebuildErrs...)
}

func (a *App) applyConfigOnly(mutate func(*config.Config) error) error {
	unlock := config.LockUserConfigEdits()
	defer unlock()
	cfg, path, err := a.loadDesktopUserConfigForEdit()
	if err != nil {
		return err
	}
	if err := mutate(cfg); err != nil {
		return err
	}
	return cfg.SaveTo(path)
}

func (a *App) ensureActiveTabRebuildAllowed(setting string) error {
	tab := a.activeTab()
	if tab == nil {
		if a.ctx == nil {
			return nil
		}
		return fmt.Errorf("no active tab")
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), setting); err != nil {
		return err
	}
	return nil
}

func (a *App) ensureLiveControllersRuntimeMutationAllowed(setting string) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tab := range a.tabs {
		if tab == nil {
			continue
		}
		if err := rebuildControllerActiveWorkErrorFor(tab.Ctrl, setting); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deferredRebuildWarning(setting string, err error) (string, bool) {
	return a.deferredRebuildWarningForTab(setting, err, a.activeTab())
}

func (a *App) deferredRebuildWarningForTab(setting string, err error, tab *WorkspaceTab) (string, bool) {
	if err == nil || !errors.Is(err, agent.ErrSessionLeaseHeld) {
		return "", false
	}
	setting = strings.TrimSpace(setting)
	if setting == "" {
		setting = "settings"
	}
	userErr := userFacingSessionLeaseError(setting, err)
	warning := fmt.Sprintf("%s saved, but the current session could not refresh yet: %s", setting, userErr.Error())
	slog.Warn("desktop: deferred settings rebuild", "setting", setting, "err", err)

	if tab != nil {
		a.warnForTab(tab.ID, warning)
		a.scheduleDeferredRebuild(tab.ID, setting)
	}
	return warning, true
}

func appendSettingsWarning(existing, warning string) string {
	existing = strings.TrimSpace(existing)
	warning = strings.TrimSpace(warning)
	if existing == "" {
		return warning
	}
	if warning == "" {
		return existing
	}
	return existing + "\n" + warning
}

// loadDesktopUserConfigForEdit loads the user config for a write path. Pending
// legacy migrations are assembled in memory and reach disk through the locked
// user-config save, never by rewriting a project file as a side effect.
//
// Contract: the caller must already hold config.LockUserConfigEdits() across
// its whole load→mutate→SaveTo cycle, so the migration write-back cannot race
// other in-process config editors. This helper must never acquire that lock
// itself: applyConfigChange/applyConfigOnly (and every other caller) invoke it
// with the lock held, so an inner acquire would self-deadlock. Read-only
// callers must use loadDesktopUserConfigForView (or its WithCredentials
// variant), which never writes to disk.
func (a *App) loadDesktopUserConfigForEdit() (*config.Config, string, error) {
	return a.loadDesktopUserConfigForEditForRoot(a.activeWorkspaceRoot())
}

func (a *App) loadDesktopUserConfigForEditForRoot(root string) (*config.Config, string, error) {
	userPath := config.UserConfigPath()
	if userPath == "" {
		return nil, "", fmt.Errorf("cannot resolve user config directory")
	}
	if _, err := os.Stat(userPath); err == nil {
		cfg, err := config.LoadForEditReadOnlyStrict(userPath)
		if err != nil {
			return nil, "", err
		}
		if err := normalizeLegacyDesktopProviderAccessForSettings(cfg, userPath); err != nil {
			return nil, "", err
		}
		return cfg, userPath, nil
	}
	cfg, err := config.LoadForEditReadOnlyStrict(userPath)
	if err != nil {
		return nil, "", err
	}
	legacyPath := config.SourcePathForRoot(root)
	if legacyPath == "" || sameConfigPath(legacyPath, userPath) {
		if err := normalizeLegacyDesktopProviderAccessForSettings(cfg, userPath); err != nil {
			return nil, "", err
		}
		return cfg, userPath, nil
	}
	legacyCfg, err := config.LoadForEditReadOnlyStrict(legacyPath)
	if err != nil {
		return nil, "", err
	}
	normalizeLegacyDesktopProviderAccessInMemory(legacyCfg, legacyPath)
	legacyCfg.ConfigVersion = config.Default().ConfigVersion
	return legacyCfg, userPath, nil
}

// loadDesktopUserConfigForView loads the user config for read-only callers.
// Contract: it never writes to disk, so it is safe without
// config.LockUserConfigEdits(). Legacy migrations (provider-access normalize)
// are applied to the returned copy in memory only;
// the on-disk file migrates the first time a locked write path runs
// loadDesktopUserConfigForEdit. Credentials (Reasonix global .env) are not
// loaded; callers that hand the config to a runtime resolving secrets from the
// process env must use loadDesktopUserConfigForViewWithCredentials.
func (a *App) loadDesktopUserConfigForView() (*config.Config, string, error) {
	return a.loadDesktopUserConfigForViewForRoot(a.activeWorkspaceRoot())
}

func (a *App) loadDesktopUserConfigForViewForRoot(root string) (*config.Config, string, error) {
	return a.loadDesktopUserConfigReadOnlyForRoot(root, config.LoadForEditWithoutCredentialsReadOnlyStrict)
}

// loadDesktopUserConfigReadOnlyForRoot is the shared pure-read loader behind
// the View variants: same shape as loadDesktopUserConfigForEdit, but every
// legacy migration stays in memory (zero SaveTo) and resolves from root.
func (a *App) loadDesktopUserConfigReadOnlyForRoot(root string, load func(string) (*config.Config, error)) (*config.Config, string, error) {
	userPath := config.UserConfigPath()
	if userPath == "" {
		return nil, "", fmt.Errorf("cannot resolve user config directory")
	}
	if _, err := os.Stat(userPath); err == nil {
		cfg, err := load(userPath)
		if err != nil {
			return nil, "", err
		}
		normalizeLegacyDesktopProviderAccessInMemory(cfg, userPath)
		return cfg, userPath, nil
	}
	cfg, err := load(userPath)
	if err != nil {
		return nil, "", err
	}
	legacyPath := config.SourcePathForRoot(root)
	if legacyPath == "" || sameConfigPath(legacyPath, userPath) {
		normalizeLegacyDesktopProviderAccessInMemory(cfg, userPath)
		return cfg, userPath, nil
	}

	legacyCfg, err := load(legacyPath)
	if err != nil {
		return nil, "", err
	}
	normalizeLegacyDesktopProviderAccessInMemory(legacyCfg, legacyPath)
	legacyCfg.ConfigVersion = config.Default().ConfigVersion
	return legacyCfg, userPath, nil
}

// normalizeLegacyDesktopProviderAccessForSettings is the write-path variant:
// it normalizes in memory and persists the migrated form to path. Callers must
// hold config.LockUserConfigEdits() (see loadDesktopUserConfigForEdit). Read
// paths use normalizeLegacyDesktopProviderAccessInMemory instead.
func normalizeLegacyDesktopProviderAccessForSettings(cfg *config.Config, path string) error {
	if !normalizeLegacyDesktopProviderAccessInMemory(cfg, path) {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return cfg.SaveTo(path)
}

// normalizeLegacyDesktopProviderAccessInMemory seeds cfg.Desktop.ProviderAccess
// from configs written before Settings tracked explicit provider access. It
// never touches disk; it reports whether cfg now carries a normalized list
// that the file at path does not declare (i.e. whether a write path should
// persist it).
func normalizeLegacyDesktopProviderAccessInMemory(cfg *config.Config, path string) bool {
	if cfg == nil || len(cfg.Desktop.ProviderAccess) > 0 || configDeclaresProviderAccess(path) {
		return false
	}
	config.NormalizeLegacyDesktopProviderAccess(cfg)
	return len(cfg.Desktop.ProviderAccess) > 0 && strings.TrimSpace(path) != ""
}

func configDeclaresProviderAccess(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	body, err := readFileUTF8(path)
	if err != nil {
		return false
	}
	for line := range strings.SplitSeq(string(body), "\n") {
		if before, _, ok := strings.Cut(line, "#"); ok {
			line = before
		}
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "provider_access"); ok {
			rest := strings.TrimSpace(after)
			return strings.HasPrefix(rest, "=")
		}
	}
	return false
}

func (a *App) activeWorkspaceRoot() string {
	tab := a.activeTab()
	if tab != nil {
		a.reconcileTabWithPinnedSessionMeta(tab)
		if strings.TrimSpace(tab.WorkspaceRoot) != "" {
			return tab.WorkspaceRoot
		}
	}
	return "."
}

func (a *App) saveProviderCredential(apiKeyEnv, value string) (string, error) {
	apiKeyEnv = strings.TrimSpace(apiKeyEnv)
	value = strings.TrimSpace(value)
	if err := upsertDotEnv(apiKeyEnv, value); err != nil {
		return "", err
	}
	return providerCredentialSourceNotice(apiKeyEnv, value), nil
}

func providerCredentialSourceNotice(apiKeyEnv, value string) string {
	return ""
}

func sameConfigPath(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	aAbs, aErr := filepath.Abs(a)
	bAbs, bErr := filepath.Abs(b)
	if aErr == nil && bErr == nil {
		return filepath.Clean(aAbs) == filepath.Clean(bAbs)
	}
	return filepath.Clean(a) == filepath.Clean(b)
}
