package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/control"
)

// ResumeSession snapshots the current conversation, then loads the session at
// path and continues it on the active tab. The model and working folder are
// unchanged; only the transcript is swapped. Returns the resumed messages for
// the frontend to render.
func (a *App) ResumeSession(path string) ([]HistoryMessage, error) {
	return a.ResumeSessionForTab("", path)
}

func (a *App) ResumeSessionPage(path string, limit int) (HistoryPage, error) {
	return a.ResumeSessionPageForTab("", path, limit)
}

func (a *App) ResumeSessionPageForTab(tabID, path string, limit int) (HistoryPage, error) {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if tab == nil || ctrl == nil {
		return HistoryPage{}, fmt.Errorf("tab is not ready")
	}
	sessionPath, _, err := validateSessionPath(controllerSessionDir(ctrl), path)
	if err != nil {
		return HistoryPage{}, err
	}
	loaded, err := loadResumableSession(sessionPath)
	if err != nil {
		return HistoryPage{}, err
	}
	if sessionRuntimeKey(tab.currentSessionPath()) != sessionRuntimeKey(sessionPath) {
		if err := a.rebindTabToLoadedSessionPath(tab, sessionPath, loaded); err != nil {
			return HistoryPage{}, err
		}
	}
	a.setTabReadOnly(tab.ID, false)
	return a.HistoryPageForTab(tab.ID, 0, limit), nil
}

// ResumeSessionForTab is the tab-scoped form of ResumeSession. A saved session
// path is a runtime identity, so changing to a different path must replace the
// tab's controller binding rather than mutating the current controller in place.
func (a *App) ResumeSessionForTab(tabID, path string) ([]HistoryMessage, error) {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if tab == nil || ctrl == nil {
		return []HistoryMessage{}, fmt.Errorf("tab is not ready")
	}
	if canonical := a.resolveCanonicalSessionPath(path); canonical != "" {
		path = canonical
	}
	sessionPath, _, err := validateSessionPath(controllerSessionDir(ctrl), path)
	if err != nil {
		return nil, err
	}
	loaded, err := loadResumableSession(sessionPath)
	if err != nil {
		return nil, err
	}
	if sessionRuntimeKey(tab.currentSessionPath()) == sessionRuntimeKey(sessionPath) {
		a.setTabReadOnly(tab.ID, false)
		return a.HistoryForTab(tabID), nil
	}

	if err := a.rebindTabToLoadedSessionPath(tab, sessionPath, loaded); err != nil {
		return nil, err
	}
	a.setTabReadOnly(tab.ID, false)
	return a.HistoryForTab(tab.ID), nil
}

func (a *App) OpenChannelSessionForTab(tabID, path string) ([]HistoryMessage, error) {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if tab == nil || ctrl == nil {
		return []HistoryMessage{}, fmt.Errorf("tab is not ready")
	}
	sessionPath, _, err := validateSessionPath(controllerSessionDir(ctrl), path)
	if err != nil {
		return nil, err
	}
	loaded, err := loadResumableSession(sessionPath)
	if err != nil {
		return nil, err
	}
	if sessionRuntimeKey(tab.currentSessionPath()) != sessionRuntimeKey(sessionPath) {
		if err := a.rebindTabToLoadedSessionPath(tab, sessionPath, loaded); err != nil {
			return nil, err
		}
	}
	a.setTabReadOnly(tab.ID, true)
	return a.HistoryForTab(tab.ID), nil
}

func (a *App) OpenChannelSessionPageForTab(tabID, path string, limit int) (HistoryPage, error) {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if tab == nil || ctrl == nil {
		return HistoryPage{}, fmt.Errorf("tab is not ready")
	}
	sessionPath, _, err := validateSessionPath(controllerSessionDir(ctrl), path)
	if err != nil {
		return HistoryPage{}, err
	}
	loaded, err := loadResumableSession(sessionPath)
	if err != nil {
		return HistoryPage{}, err
	}
	if sessionRuntimeKey(tab.currentSessionPath()) != sessionRuntimeKey(sessionPath) {
		if err := a.rebindTabToLoadedSessionPath(tab, sessionPath, loaded); err != nil {
			return HistoryPage{}, err
		}
	}
	a.setTabReadOnly(tab.ID, true)
	return a.HistoryPageForTab(tab.ID, 0, limit), nil
}

func (a *App) setTabReadOnly(tabID string, readOnly bool) {
	var terminalSessions []*terminalSession
	a.mu.Lock()
	tab := a.tabs[tabID]
	if tab == nil || tab.ReadOnly == readOnly {
		a.mu.Unlock()
		return
	}
	if a.terminals != nil {
		if readOnly {

			terminalSessions = a.terminals.detachForTab(tabID)
		} else {

			a.terminals.reopenForTab(tabID)
		}
	}
	tab.ReadOnly = readOnly
	a.saveTabsLocked()
	a.mu.Unlock()
	if len(terminalSessions) > 0 {

		a.terminals.closeSessions(terminalSessions)
	}
}

func (a *App) rebindTabToSessionPath(tab *WorkspaceTab, sessionPath string) error {
	sessionPath = canonicalTabSessionPath(sessionPath)
	if sessionPath == "" {
		return fmt.Errorf("session path is required")
	}
	loaded, err := loadResumableSession(sessionPath)
	if err != nil {
		return err
	}
	return a.rebindTabToLoadedSessionPath(tab, sessionPath, loaded)
}

func (a *App) rebindTabToLoadedSessionPath(tab *WorkspaceTab, sessionPath string, loaded *agent.Session) error {
	if tab == nil {
		return fmt.Errorf("tab is not ready")
	}
	sessionPath = canonicalTabSessionPath(sessionPath)
	if sessionPath == "" {
		return fmt.Errorf("session path is required")
	}
	if agent.IsCleanupPending(sessionPath) {
		return fmt.Errorf("session is pending cleanup")
	}
	if loaded == nil {
		var err error
		loaded, err = loadResumableSession(sessionPath)
		if err != nil {
			return err
		}
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab {
		a.mu.Unlock()
		return fmt.Errorf("tab is not ready")
	}
	currentPath := ""
	if tab.Ctrl != nil {
		currentPath = strings.TrimSpace(tab.Ctrl.SessionPath())
	}
	if currentPath == "" {
		currentPath = strings.TrimSpace(tab.SessionPath)
	}
	if sessionRuntimeKey(currentPath) == sessionRuntimeKey(sessionPath) {

		a.mu.Unlock()
		return nil
	}
	a.supersedeTabBuildLocked(tab)
	source := snapshotTabRuntimeLocked(tab)
	a.mu.Unlock()

	targetKey := sessionRuntimeKey(sessionPath)
	a.mu.Lock()
	detached := a.detachedSessions[targetKey]
	hasDetached := detached != nil && detached.Ctrl != nil
	a.mu.Unlock()

	if hasDetached {
		a.runtimeAdmissionMu.Lock()
		tab.turnStartMu.Lock()

		a.mu.Lock()
		if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != source.ctrl {
			a.mu.Unlock()
			tab.turnStartMu.Unlock()
			a.runtimeAdmissionMu.Unlock()
			return fmt.Errorf("tab changed while reattaching session; retry")
		}
		a.mu.Unlock()

		if source.ctrl != nil {
			if err := a.snapshotTabForAction(tab, "switching sessions"); err != nil {
				tab.turnStartMu.Unlock()
				a.runtimeAdmissionMu.Unlock()
				return err
			}
			if oldPath := a.reconciledSessionPathForTab(tab); oldPath != "" {
				if err := a.saveTabSessionMeta(tab, oldPath); err != nil {
					tab.turnStartMu.Unlock()
					a.runtimeAdmissionMu.Unlock()
					return fmt.Errorf("save current session metadata before switching sessions: %w", err)
				}
			}
		}

		a.mu.Lock()
		if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != source.ctrl {
			a.mu.Unlock()
			tab.turnStartMu.Unlock()
			a.runtimeAdmissionMu.Unlock()
			return fmt.Errorf("tab changed while reattaching session; retry")
		}
		a.mu.Unlock()

		detachSource := controllerHasActiveRuntimeWork(source.ctrl)
		oldCtrl, oldSink, oldLease, oldHostKey, attached := a.reattachDetachedSessionRuntimeForRebind(
			tab, source, sessionPath, detachSource,
		)
		if !attached {
			tab.turnStartMu.Unlock()
			a.runtimeAdmissionMu.Unlock()
			return fmt.Errorf("failed to reattach detached session runtime")
		}

		if oldSink != nil {
			oldSink.setBinding("", nil)
			oldSink.clearContext()
		}
		if oldCtrl != nil {
			oldCtrl.Close()
		}
		if oldHostKey != "" {
			a.releaseSharedHost(oldHostKey)
		}
		if oldLease != nil {
			oldLease.Release()
		}

		a.clearDeferredRebuild(tab.ID)
		a.emitReady(a.ctx, tab.ID)

		tab.turnStartMu.Unlock()
		a.runtimeAdmissionMu.Unlock()
		return nil
	}

	a.runtimeAdmissionMu.Lock()
	defer a.runtimeAdmissionMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()
	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != source.ctrl {
		a.mu.Unlock()
		return fmt.Errorf("tab changed while preparing to switch sessions; retry")
	}
	source = snapshotTabRuntimeLocked(tab)
	a.mu.Unlock()

	if source.ctrl != nil {
		if err := a.snapshotTabForAction(tab, "switching sessions"); err != nil {
			return err
		}
		if oldPath := a.reconciledSessionPathForTab(tab); oldPath != "" {
			if err := a.saveTabSessionMeta(tab, oldPath); err != nil {
				return fmt.Errorf("save current session metadata before switching sessions: %w", err)
			}
		}
	}

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != source.ctrl {
		a.mu.Unlock()
		return fmt.Errorf("tab changed while preparing to switch sessions; retry")
	}
	source = snapshotTabRuntimeLocked(tab)
	a.mu.Unlock()

	transition, err := a.reserveSessionRuntimePath(tab, sessionPath)
	if err != nil {
		return userFacingSessionLeaseError("", err)
	}
	committed := false
	defer func() {
		if !committed {
			a.rollbackSessionRuntimePath(transition)
		}
	}()

	profile := loadTabSessionProfile(sessionPath)
	detachSource := controllerHasActiveRuntimeWork(source.ctrl)
	candidateNeedsHostRef := detachSource || source.ctrl == nil
	candidate, err := a.buildSessionRebindCandidate(tab, source, sessionPath, loaded, profile, candidateNeedsHostRef)
	if err != nil {
		return fmt.Errorf("resume session: %w", err)
	}
	defer func() {
		if !committed {
			candidate.close()
		}
	}()

	targetLease, err := a.acquireCandidateSessionLease(tab, sessionPath)
	if err != nil {
		return err
	}
	defer func() {
		if !committed {
			targetLease.Release()
		}
	}()
	if err := a.runRebindCandidateHook("lease_acquired"); err != nil {
		return fmt.Errorf("resume session: %w", err)
	}

	a.mu.Lock()
	if err := a.validateAndBindSessionRebindLocked(tab, source, transition, candidate, targetLease); err != nil {
		a.mu.Unlock()
		return err
	}
	var oldLease *agent.SessionLease
	oldCtrl := tab.Ctrl
	oldSink := tab.sink
	if detachSource {
		if !a.detachRuntimeForReplacementLocked(tab) {
			a.mu.Unlock()
			return fmt.Errorf("current session runtime cannot be detached")
		}
		if a.runtimeBySessionKey[transition.targetKey] == transition.runtime {
			delete(a.runtimeBySessionKey, transition.targetKey)
		}
	} else {
		if !a.commitSessionRuntimePathLocked(transition) {
			a.mu.Unlock()
			return fmt.Errorf("tab runtime changed while switching sessions; retry")
		}
		oldLease = tab.takeSessionLease()
	}
	tab.adoptSessionLease(targetLease)
	targetLease = nil
	tab.Ctrl = candidate.ctrl
	tab.sink = candidate.sink
	tab.SessionPath = sessionPath
	tab.model = candidate.model
	tab.Label = candidate.ctrl.Label()
	applyNormalizedRuntimeToTabLocked(tab, candidate.runtime)
	tab.Ready = true
	clearTabStartupError(tab)
	tab.ActivityStatus = ""
	tab.replaceTelemetry(candidate.telemetry, sessionRuntimeKey(sessionPath))
	if tab.sink != nil {
		tab.sink.setBinding(tab.ID, a)
		tab.sink.setContext(a.ctx)
	}
	if detachSource {
		a.newSessionRuntimeLocked(tab, transition.targetKey)
	}
	newEpoch := a.advanceSessionRuntimeEpochLocked(tab)
	a.saveTabsLocked()
	candidate.ctrl = nil
	candidate.sink = nil
	committed = true
	a.mu.Unlock()

	_ = a.runRebindCandidateHook("committed")

	if !detachSource {
		if oldSink != nil {
			oldSink.setBinding("", nil)
			oldSink.clearContext()
		}
		if oldCtrl != nil {
			oldCtrl.Close()
		}
		if oldLease != nil {
			oldLease.Release()
		}
	}
	a.persistTabSessionPath(tab, sessionPath)
	a.clearDeferredRebuild(tab.ID)
	a.notifyTabRuntimeRebuiltAtEpoch(tab, newEpoch)
	a.emitReady(a.ctx, tab.ID)
	return nil
}

// reattachDetachedSessionRuntimeForRebind atomically replaces tab with the
// already-running detached target. If the visible source is still active, its
// controller, sink, lease, and runtime registry entry move to detachedSessions
// in the same App.mu transaction; an idle source is returned for off-lock
// teardown. The caller must hold runtimeRebuildMu, runtimeAdmissionMu, and
// tab.turnStartMu so detachSource cannot become stale through new turn admission.
func (a *App) reattachDetachedSessionRuntimeForRebind(
	tab *WorkspaceTab,
	source tabRuntimeSnapshot,
	sessionPath string,
	detachSource bool,
) (control.SessionAPI, *tabEventSink, *agent.SessionLease, string, bool) {
	key := sessionRuntimeKey(sessionPath)
	if tab == nil || key == "" {
		return nil, nil, nil, "", false
	}

	a.mu.Lock()
	if tab.removed || a.tabs[tab.ID] != tab || tab.Ctrl != source.ctrl {
		a.mu.Unlock()
		return nil, nil, nil, "", false
	}
	detached := a.detachedSessions[key]
	if detached == nil || detached.Ctrl == nil {
		a.mu.Unlock()
		return nil, nil, nil, "", false
	}
	if rt := a.runtimeForTabLocked(detached); rt != nil {
		if rt.Phase != sessionRuntimeReady {
			a.mu.Unlock()
			return nil, nil, nil, "", false
		}
	} else if !detached.Ready {

		a.mu.Unlock()
		return nil, nil, nil, "", false
	}

	oldCtrl := tab.Ctrl
	oldSink := tab.sink
	var oldLease *agent.SessionLease
	oldHostKey := ""
	if detachSource {
		if !a.detachRuntimeForReplacementLocked(tab) {
			a.mu.Unlock()
			return nil, nil, nil, "", false
		}

		oldCtrl = nil
		oldSink = nil
	} else {

		oldLease = tab.takeSessionLease()
		oldHostKey = takeTabSharedHostKey(tab)
	}

	delete(a.detachedSessions, key)
	applyRuntimeTab(tab, detached, sessionPath, a.ctx, a)
	a.saveTabsLocked()
	attachedCtrl := tab.Ctrl
	a.mu.Unlock()

	if attachedCtrl != nil {
		attachedCtrl.ReplayPendingPrompts()
	}
	return oldCtrl, oldSink, oldLease, oldHostKey, true
}

type sessionRebindCandidate struct {
	app               *App
	ctrl              control.SessionAPI
	sink              *tabEventSink
	model             string
	runtime           normalizedTabRuntime
	telemetry         tabTelemetrySnapshot
	sharedHostKey     string
	ownsSharedHostRef bool
}

func (c *sessionRebindCandidate) close() {
	if c == nil {
		return
	}
	if c.sink != nil {
		c.sink.clearContext()
	}
	if c.ctrl != nil {
		c.ctrl.Close()
		c.ctrl = nil
	}
	if c.ownsSharedHostRef && c.app != nil && c.sharedHostKey != "" {
		c.app.releaseSharedHost(c.sharedHostKey)
		c.ownsSharedHostRef = false
	}
}

func normalizedRuntimeForSessionProfile(profile tabSessionProfile) normalizedTabRuntime {
	temp := &WorkspaceTab{}
	applyTabSessionProfile(temp, profile)
	return snapshotTabRuntimeLocked(temp).normalizedRuntime()
}

func (a *App) runRebindCandidateHook(stage string) error {
	if a == nil || a.rebindCandidateHook == nil {
		return nil
	}
	return a.rebindCandidateHook(stage)
}

func (a *App) buildSessionRebindCandidate(
	tab *WorkspaceTab,
	source tabRuntimeSnapshot,
	sessionPath string,
	loaded *agent.Session,
	profile tabSessionProfile,
	separateRuntime bool,
) (*sessionRebindCandidate, error) {
	root := strings.TrimSpace(source.workspaceRoot)
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}
	_ = config.MigrateLegacyCredentialsForRoot(root)
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(source.model)
	if sessionModel, ok := agent.LoadSessionModel(sessionPath); ok {
		config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, sessionModel)
		if _, ok := cfg.ResolveModel(sessionModel); ok {
			model = sessionModel
		}
	}
	if model == "" {
		model = cfg.DefaultModel
	}
	config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, model)
	if resolved, _, ok := cfg.ResolveModelWithFallback(model); ok {
		model = resolved
	}

	sessionDir := controllerSessionDir(source.ctrl)
	if strings.TrimSpace(sessionDir) == "" {
		sessionDir = filepath.Dir(sessionPath)
	}
	sink := &tabEventSink{tabID: tab.ID, app: a}
	runtimeProfile := normalizedRuntimeForSessionProfile(profile)
	sharedHost := a.lookupSharedHost(source.sharedHostKey)
	ownsSharedHostRef := false
	if separateRuntime && source.sharedHostKey != "" {
		sharedHost = a.acquireSharedHost(source.sharedHostKey)
		ownsSharedHostRef = true
	}
	ctrl, err := boot.Build(a.bootContext(), boot.Options{
		Model:                    model,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     a.desktopControllerSink(sink, cfg.Notifications),
		WorkspaceRoot:            root,
		SessionDir:               sessionDir,
		EffortOverride:           cloneStringPtr(source.effort),
		AgentPreset:              boot.NormalizeAgentPreset(runtimeProfile.tokenMode),
		TokenMode:                runtimeProfile.tokenMode,
		SharedHost:               sharedHost,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),
	})
	if err != nil {
		sink.clearContext()
		if ownsSharedHostRef {
			a.releaseSharedHost(source.sharedHostKey)
		}
		return nil, err
	}
	candidate := &sessionRebindCandidate{
		app: a, ctrl: ctrl, sink: sink, model: model, runtime: runtimeProfile,
		sharedHostKey: source.sharedHostKey, ownsSharedHostRef: ownsSharedHostRef,
	}
	a.bindControllerDisplayRecorder(ctrl)
	configureControllerRuntime(ctrl, nil, runtimeProfile)
	if err := a.runRebindCandidateHook("built"); err != nil {
		candidate.close()
		return nil, err
	}
	restoredRuntime, err := resumeControllerRuntimeWithSession(ctrl, loaded, sessionPath, runtimeProfile)
	if err != nil {
		candidate.close()
		return nil, err
	}
	candidate.runtime = restoredRuntime
	candidate.telemetry = loadTelemetry(sessionPath + ".telemetry.json")
	if err := a.runRebindCandidateHook("restored"); err != nil {
		candidate.close()
		return nil, err
	}
	return candidate, nil
}

func (a *App) acquireCandidateSessionLease(tab *WorkspaceTab, path string) (*agent.SessionLease, error) {
	lease, err := withSessionLeaseContentionRetry(func() (*agent.SessionLease, error) {
		lease, err := agent.TryAcquireSessionLease(path)
		if err == nil {
			return lease, nil
		}
		if a.canReclaimCurrentProcessSessionLease(tab, path, err) {
			if reclaimed, reclaimErr := agent.TryReclaimCurrentProcessSessionLease(path); reclaimErr == nil {
				return reclaimed, nil
			} else {
				err = reclaimErr
			}
		}
		return nil, err
	})
	if err != nil {
		return nil, userFacingSessionLeaseError("", err)
	}
	return lease, nil
}

func loadResumableSession(sessionPath string) (*agent.Session, error) {
	if agent.IsCleanupPending(sessionPath) {
		return nil, fmt.Errorf("session is pending cleanup")
	}
	return agent.LoadSession(sessionPath)
}

// PreviewSession reads a saved session for display only. It does not snapshot or
// swap the active controller, so the history drawer can call it while a turn runs.
func (a *App) PreviewSession(path string) ([]HistoryMessage, error) {
	sessionDir, sessionPath, err := a.sessionDirForPath(path)
	if err != nil {
		return nil, err
	}
	return previewSessionMessages(sessionDir, sessionPath)
}

// invalidatePromptHistoryCache resets the lazy prompt-history tape so the next
// ScanPromptHistory call rebuilds session order and reloads sessions on demand.
// Called from every session-mutating path: NewSession, ClearSession,
// DeleteSession, RestoreSession, PurgeTrashedSession, RenameSession.
func (a *App) invalidatePromptHistoryCache() {
	a.promptHistoryMu.Lock()
	a.promptHistoryTape = nil
	a.promptHistoryMu.Unlock()
}
