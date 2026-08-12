package main

import (
	"errors"
	"fmt"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/provider"
)

// rebuild builds a replacement controller from the (just-changed) config and
// swaps it in only after the target session lease is available. The old
// controller stays usable if the rebuild fails.
func (a *App) rebuild() error {
	return a.rebuildSetting("settings")
}

func (a *App) rebuildSetting(setting string) error {
	if a.ctx == nil {
		return nil
	}

	a.runtimeRebuildMu.Lock()
	err := a.rebuildSettingLocked(setting)
	a.runtimeRebuildMu.Unlock()
	return err
}

// rebuildSettingLocked is rebuildSetting's body; callers must already hold
// runtimeRebuildMu. The deferred-rebuild retry loop calls this directly because
// it takes the lock across its lease probe.
func (a *App) rebuildSettingLocked(setting string) error {
	if a.ctx == nil {
		return nil
	}
	tab := a.activeTab()
	if tab == nil {
		return fmt.Errorf("no active tab")
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	return a.rebuildSettingTurnLocked(setting, tab, false, false)
}

// rebuildSettingTurnLocked is rebuildSettingLocked's body; callers must hold
// runtimeRebuildMu and the passed tab's turnStartMu. admissionHeld is true for
// MCP lifecycle callers that also hold runtimeAdmissionMu's write side.
// reload selects the stage-3b runtime-reload build path (boot.Rebuild migrates
// the session) instead of the legacy boot.Build + manual migration; everything
// else — active-work guards, workspace prep, lease moves, swap, close-after-
// swap, fence — is shared.
func (a *App) rebuildSettingTurnLocked(setting string, tab *WorkspaceTab, admissionHeld bool, reload bool) error {
	return a.rebuildSettingTurnLockedWithModel(setting, tab, "", admissionHeld, reload)
}

// rebuildSettingTurnLockedWithModel optionally builds the replacement for a
// target model without changing tab.model before the swap. Provider removal
// uses this to remain failure-atomic: a failed fallback build leaves both the
// old controller and its visible model identity untouched.
func (a *App) rebuildSettingTurnLockedWithModel(setting string, tab *WorkspaceTab, modelOverride string, admissionHeld bool, reload bool) error {
	if a.ctx == nil {
		return nil
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), setting); err != nil {
		return err
	}
	if !admissionHeld {
		if err := a.ensureTabControllerWorkspace(tab); err != nil {
			return err
		}
	}
	prevPath := a.reconciledSessionPathForTab(tab)
	if prevPath == "" {
		prevPath = a.currentSessionPathFor(tab)
	}
	if a.controllerForTab(tab) == nil && prevPath != "" && a.attachExistingSessionRuntime(tab, prevPath, a.ctx) {
		prevPath = a.reconciledSessionPathForTab(tab)
		if prevPath == "" {
			prevPath = a.currentSessionPathFor(tab)
		}
	}
	if err := rebuildControllerActiveWorkErrorFor(a.controllerForTab(tab), setting); err != nil {
		return err
	}

	var carried []provider.Message
	oldCtrl := a.controllerForTab(tab)
	if oldCtrl != nil {
		if prevPath == "" {
			prevPath = oldCtrl.SessionPath()
		}
		if err := a.ensureTabSessionLeaseForRebuild(tab, prevPath, setting); err != nil {
			return err
		}
		if err := a.snapshotTabForAction(tab, "rebuilding settings"); err != nil {
			return err
		}
		prevPath = sessionPathAfterSnapshot(oldCtrl, prevPath)
		carried = oldCtrl.History()
	}
	snap := a.tabRuntimeSnapshot(tab)
	runtime := snap.normalizedRuntime()
	model := snap.model
	if override := strings.TrimSpace(modelOverride); override != "" {
		model = override
	}
	if cfg, err := config.LoadForRoot(snap.workspaceRoot); err == nil {
		if resolved, fallback, ok := cfg.ResolveModelWithFallback(model); ok {
			if fallback && strings.TrimSpace(model) != "" {
				a.noticeForTab(tab.ID, fmt.Sprintf("model %q is no longer available; switched to %s", model, resolved))
			}
			model = resolved
		}
	}
	ctrl, restoredRuntime, path, err := a.buildSettingReplacementController(tab, snap, runtime, model, prevPath, setting, oldCtrl, carried, reload)
	if err != nil {
		if oldCtrl == nil {
			leaseHeld := false
			a.mu.Lock()
			leaseHeld = setTabStartupError(tab, err)
			tab.Ready = false
			if leaseHeld {
				a.setSessionRuntimePhaseLocked(tab, sessionRuntimeLeaseBlocked, err)
			} else {
				a.setSessionRuntimePhaseLocked(tab, sessionRuntimeFailed, err)
			}
			a.mu.Unlock()
			if leaseHeld {
				a.scheduleDeferredStartupBuild(tab.ID)
			}
			a.emitReady(a.ctx)
		}
		return err
	}
	a.mu.Lock()
	if err := a.authorizeTabReplacementLocked(tab, ctrl, "rebuilding settings", "rebuilt"); err != nil {
		a.mu.Unlock()
		ctrl.Close()
		tab.releaseSessionLease()
		return err
	}
	tab.Ctrl = ctrl
	tab.model = model
	tab.Label = ctrl.Label()
	applyNormalizedRuntimeToTabLocked(tab, restoredRuntime)
	clearTabStartupError(tab)
	tab.Ready = true

	a.supersedeTabBuildLocked(tab)
	a.saveTabsLocked()
	a.mu.Unlock()

	if oldCtrl != nil && oldCtrl != ctrl {
		oldCtrl.Close()
	}
	a.persistTabSessionPath(tab, path)
	a.clearDeferredRebuild(tab.ID)
	a.notifyTabRuntimeRebuilt(tab)
	a.emitReady(a.ctx)
	return nil
}

// buildSettingReplacementController builds the replacement controller for
// rebuildSettingTurnLocked and migrates the session onto it, returning the
// controller, the runtime posture actually restored, and the session path it
// bound. reload=false is the legacy settings path (boot.Build plus the
// desktop's manual migration); reload=true is the stage-3b runtime reload,
// routing build and migration through boot.Rebuild so history, approval mode
// and grants, plan/goal state, and lifecycle move inside the boot layer. The
// caller owns the swap, closing the old controller after the swap, and the
// post-swap persistence.
func (a *App) buildSettingReplacementController(tab *WorkspaceTab, snap tabRuntimeSnapshot, runtime normalizedTabRuntime, model, prevPath, setting string, oldCtrl control.SessionAPI, carried []provider.Message, reload bool) (control.SessionAPI, normalizedTabRuntime, string, error) {
	opts := boot.Options{
		Model: model, RequireKey: false,
		RuntimeReload:            boot.RuntimeReload{ForceFullRebuild: reload},
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     snap.sink,
		WorkspaceRoot:            snap.workspaceRoot,
		SessionDir:               sessionDirForSnapshot(snap),
		EffortOverride:           cloneStringPtr(snap.effort),
		AgentPreset:              boot.NormalizeAgentPreset(runtime.tokenMode),
		TokenMode:                runtime.tokenMode,
		SharedHost:               a.lookupSharedHost(snap.sharedHostKey),
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),
	}
	if reload && oldCtrl != nil {
		old, ok := oldCtrl.(*control.Controller)
		if !ok {
			return nil, normalizedTabRuntime{}, "", fmt.Errorf("reload runtime: controller is %T, want *control.Controller", oldCtrl)
		}
		res, err := rebuildTabRuntime(a, tab, old, opts)
		if err != nil {
			return nil, normalizedTabRuntime{}, "", err
		}
		ctrl := res.Controller
		a.bindControllerDisplayRecorder(ctrl)

		ctrl.EnableInteractiveApproval()
		applyTabModeToController(ctrl, runtime.tabMode())

		path := agent.ContinueSessionPath(prevPath, ctrl.SessionDir(), ctrl.Label())
		if err := a.ensureTabSessionLeaseForRebuild(tab, path, setting); err != nil {
			ctrl.Close()
			return nil, normalizedTabRuntime{}, "", err
		}
		restoredRuntime, err := normalizeRestoredControllerRuntime(ctrl, runtime)
		if err != nil {
			ctrl.Close()
			return nil, normalizedTabRuntime{}, "", err
		}
		return ctrl, restoredRuntime, path, nil
	}

	if old, ok := oldCtrl.(*control.Controller); ok && old != nil && opts.SessionTemp == nil {
		opts.SessionTemp = old.SessionTemp()
	}
	ctrl, err := boot.Build(a.bootContext(), opts)
	if err != nil {
		return nil, normalizedTabRuntime{}, "", err
	}
	a.bindControllerDisplayRecorder(ctrl)
	configureControllerRuntime(ctrl, oldCtrl, runtime)
	path := agent.ContinueSessionPath(prevPath, ctrl.SessionDir(), ctrl.Label())
	if err := a.ensureTabSessionLeaseForRebuild(tab, path, setting); err != nil {
		ctrl.Close()
		return nil, normalizedTabRuntime{}, "", err
	}
	restoredRuntime, err := resumeControllerRuntimeWithMessages(ctrl, carried, path, runtime)
	if err != nil {
		ctrl.Close()
		return nil, normalizedTabRuntime{}, "", err
	}
	return ctrl, restoredRuntime, path, nil
}

// runtimeReloadSettingLabel is the settings-style label used in busy/lease
// error text and notices for an explicit runtime reload.
const runtimeReloadSettingLabel = "runtime reload"

// ReloadRuntime rebuilds the tab's agent runtime in place — tools, skills,
// commands, hooks, providers, and MCP servers are re-discovered from the
// current config — while the session carries over (transcript, approval
// grants, goal/recovery state, shared plugin Host) via boot.Rebuild. Active
// work or a held lease queues exactly one reload on the deferred-rebuild
// loop, which runs it once the tab is idle; a failure keeps the old
// controller fully usable.
func (a *App) ReloadRuntime(tabID string) error {
	if a.ctx == nil {
		return nil
	}
	tab := a.tabByID(tabID)
	if tab == nil || tab.ID != tabID {
		return fmt.Errorf("unknown tab %q", tabID)
	}

	a.runtimeRebuildMu.Lock()
	err := a.reloadRuntimeTurnLocked(tab)
	a.runtimeRebuildMu.Unlock()
	if err == nil {
		return nil
	}
	var busy *rebuildBusyError
	if errors.As(err, &busy) || errors.Is(err, agent.ErrSessionLeaseHeld) {

		a.scheduleDeferredRebuild(tab.ID, deferredRuntimeReloadLabel)
		a.noticeForTab(tab.ID, "runtime reload queued: will run when the current work finishes")
		return nil
	}
	return err
}

// reloadRuntimeTurnLocked runs the in-place runtime reload for tab; callers
// hold runtimeRebuildMu (the deferred-rebuild retry loop also drives it).
func (a *App) reloadRuntimeTurnLocked(tab *WorkspaceTab) error {
	if a.ctx == nil {
		return nil
	}
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	return a.rebuildSettingTurnLocked(runtimeReloadSettingLabel, tab, false, true)
}
