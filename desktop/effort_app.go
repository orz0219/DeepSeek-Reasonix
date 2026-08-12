package main

import (
	"fmt"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/provider"
)

func (a *App) Effort() EffortInfo {
	return a.EffortForTab("")
}

func (a *App) EffortForTab(tabID string) EffortInfo {
	entry, err := a.currentProviderEntryForTab(tabID)
	if err != nil {
		return EffortInfo{Current: "auto", Levels: []string{}}
	}
	cap := config.EffortCapabilityForEntry(entry)
	if !cap.Supported {
		return EffortInfo{Supported: false, Current: "auto", Default: cap.Default, Levels: []string{}}
	}
	levels := cap.Levels
	if levels == nil {
		levels = []string{}
	}
	return EffortInfo{Supported: true, Current: config.EffortDisplay(entry), Default: cap.Default, Levels: levels}
}

func (a *App) SetEffort(level string) error {
	return a.SetEffortForTab("", level)
}

func (a *App) SetEffortForTab(tabID, level string) error {
	tab := a.tabByID(tabID)
	if tab == nil {
		if strings.TrimSpace(tabID) == "" {
			entry, err := a.currentProviderEntryForTab("")
			if err != nil {
				return err
			}
			effort, err := config.NormalizeEffort(entry, level)
			if err != nil {
				return err
			}
			return a.applyProviderEffortConfig(entry, effort)
		}
		return fmt.Errorf("tab %q not found", tabID)
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	prevPath := a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}

	if a.controllerForTab(tab) == nil && prevPath != "" {
		a.attachExistingSessionRuntime(tab, prevPath, a.ctx)
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), "effort"); err != nil {
		return err
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	prevPath = a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}
	if a.controllerForTab(tab) == nil && prevPath != "" && a.attachExistingSessionRuntime(tab, prevPath, a.ctx) {
		prevPath = a.reconciledSessionPathForTab(tab)
		if prevPath == "" {
			prevPath = a.currentSessionPathFor(tab)
		}
		if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), "effort"); err != nil {
			return err
		}
	}
	snap := a.tabRuntimeSnapshot(tab)
	runtime := snap.normalizedRuntime()
	entry, err := a.currentProviderEntryForTab(tabID)
	if err != nil {
		return err
	}
	modelRef := entry.Name + "/" + entry.Model
	effort, err := config.NormalizeEffort(entry, level)
	if err != nil {
		return err
	}
	var carried []provider.Message
	oldCtrl := a.controllerForTab(tab)
	if oldCtrl != nil {
		if prevPath == "" {
			prevPath = oldCtrl.SessionPath()
		}
		if err := a.ensureTabSessionLeaseForRebuild(tab, prevPath, "effort"); err != nil {
			return err
		}
		if err := a.snapshotTabForAction(tab, "changing effort"); err != nil {
			return err
		}
		prevPath = sessionPathAfterSnapshot(oldCtrl, prevPath)
		carried = oldCtrl.History()
	}
	sharedHost := a.lookupSharedHost(snap.sharedHostKey)
	newCtrl, err := boot.Build(a.bootContext(), boot.Options{
		Model:                    modelRef,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     snap.sink,
		WorkspaceRoot:            snap.workspaceRoot,
		SessionDir:               sessionDirForSnapshot(snap),
		EffortOverride:           &effort,
		AgentPreset:              boot.NormalizeAgentPreset(runtime.tokenMode),
		TokenMode:                runtime.tokenMode,
		SharedHost:               sharedHost,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),

		SessionTemp: sessionTempFromController(oldCtrl),
	})
	if err != nil {
		return err
	}
	a.bindControllerDisplayRecorder(newCtrl)
	configureControllerRuntime(newCtrl, oldCtrl, runtime)
	path := agent.ContinueSessionPath(prevPath, newCtrl.SessionDir(), newCtrl.Label())
	if err := a.ensureTabSessionLeaseForRebuild(tab, path, "effort"); err != nil {
		newCtrl.Close()
		return err
	}
	restoredRuntime, err := resumeControllerRuntimeWithMessages(newCtrl, carried, path, runtime)
	if err != nil {
		newCtrl.Close()
		return err
	}
	a.mu.Lock()
	if err := a.authorizeTabReplacementLocked(tab, newCtrl, "switching effort", "effort-switch"); err != nil {
		a.mu.Unlock()
		newCtrl.Close()
		tab.releaseSessionLease()
		return err
	}
	tab.Ctrl = newCtrl
	tab.model = modelRef
	tab.effort = &effort
	tab.Label = newCtrl.Label()
	applyNormalizedRuntimeToTabLocked(tab, restoredRuntime)
	clearTabStartupError(tab)
	tab.Ready = true
	a.supersedeTabBuildLocked(tab)
	a.saveTabsLocked()
	a.mu.Unlock()
	if oldCtrl != nil {
		oldCtrl.Close()
	}

	a.clearDeferredRebuild(tab.ID)
	a.persistTabSessionPath(tab, path)
	a.notifyTabRuntimeRebuilt(tab)
	return nil
}

func (a *App) SetTokenMode(mode string) error {

	return a.SetAgentPreset(boot.NormalizeAgentPreset(mode))
}

func (a *App) SetTokenModeForTab(tabID, mode string) error {

	return a.SetAgentPresetForTab(tabID, boot.NormalizeAgentPreset(mode))
}

// SetAgentPreset switches the active tab's execution setting (执行设定).
func (a *App) SetAgentPreset(preset string) error {
	return a.SetAgentPresetForTab("", preset)
}

// SetAgentPresetForTab switches a tab's role setting without rebuilding the
// controller. Active turns, background jobs, and pending approvals/asks refuse.
func (a *App) SetAgentPresetForTab(tabID, preset string) error {
	preset = boot.NormalizeAgentPreset(preset)
	legacyMode := boot.TokenModeFromAgentPreset(preset)
	tab := a.tabByID(tabID)
	if tab == nil {
		if strings.TrimSpace(tabID) == "" {
			return nil
		}
		return fmt.Errorf("tab %q not found", tabID)
	}
	a.mu.RLock()
	currentPreset := boot.NormalizeAgentPreset(tab.tokenMode)
	a.mu.RUnlock()
	if preset == currentPreset {
		return nil
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	prevPath := a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}
	if a.controllerForTab(tab) == nil && prevPath != "" {
		a.attachExistingSessionRuntime(tab, prevPath, a.ctx)
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), "执行设定"); err != nil {
		return err
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	ctrl := a.controllerForTab(tab)
	if ctrl == nil {

		a.mu.Lock()
		tab.tokenMode = legacyMode
		a.mu.Unlock()
		a.persistTabTokenMode(tab, legacyMode)
		return nil
	}

	ctrl.SetAgentPreset(preset)
	a.mu.Lock()
	tab.tokenMode = legacyMode
	a.mu.Unlock()
	a.persistTabTokenMode(tab, legacyMode)
	a.notifyTabRuntimeRebuilt(tab)
	return nil
}

// persistTabTokenMode dual-writes tokenMode (legacy) for the role setting.
// Failures keep the in-memory preset (already applied).
func (a *App) persistTabTokenMode(tab *WorkspaceTab, mode string) {
	if a == nil || tab == nil {
		return
	}
	_ = mode
	a.mu.Lock()
	a.saveTabsLocked()
	a.mu.Unlock()
	_ = a.saveTabSessionMetaForCurrentSession(tab)
}

func (a *App) applyProviderEffortConfig(entry *config.ProviderEntry, effort string) error {
	return a.applyConfigChange(func(cfg *config.Config) error {
		if _, ok := cfg.Provider(entry.Name); !ok {
			if err := cfg.UpsertProvider(*entry); err != nil {
				return err
			}
		}
		if entry.Kind == "anthropic" && effort != "" && entry.Thinking == "" {
			if err := cfg.SetProviderThinking(entry.Name, "adaptive"); err != nil {
				return err
			}
		}
		for _, name := range providerEffortTargetNames(cfg, entry) {
			if err := cfg.SetProviderEffort(name, effort); err != nil {
				return err
			}
		}
		return nil
	})
}

func providerEffortTargetNames(cfg *config.Config, entry *config.ProviderEntry) []string {
	if cfg == nil || entry == nil {
		return nil
	}
	out := []string{entry.Name}
	seen := map[string]bool{entry.Name: true}
	kind := officialProviderKindFromEntry(*entry)
	if kind == "" {
		return out
	}
	var family []string
	switch kind {
	case "deepseek":
		family = []string{"deepseek", "deepseek-flash", "deepseek-pro"}
	}
	for _, name := range family {
		if seen[name] {
			continue
		}
		p, ok := cfg.Provider(name)
		if !ok || officialProviderKindFromEntry(*p) != kind {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
