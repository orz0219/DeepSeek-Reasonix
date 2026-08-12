package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/config"
	"reasonix/internal/event"
	"reasonix/internal/extension/providerext"
	"reasonix/internal/notify"
	"strings"
)

type loadedTabSession struct {
	Path    string
	Session *agent.Session
}

func (s loadedTabSession) matches(path string) bool {
	return s.Session != nil && sessionRuntimeKey(s.Path) != "" && sessionRuntimeKey(s.Path) == sessionRuntimeKey(path)
}

func (a *App) buildTabControllerWithLoadedSession(tab *WorkspaceTab, loadedSession loadedTabSession) {
	a.buildTabControllerWithContext(tab, loadedSession, a.bootContext(), 0, nil)
}

func (a *App) desktopNotificationSender() notify.Sender {
	if a == nil {
		return notify.NewPlatformSender()
	}
	a.notificationSenderOnce.Do(func() {
		if a.notificationSender == nil {
			a.notificationSender = notify.NewPlatformSender()
		}
	})
	return a.notificationSender
}

func (a *App) desktopControllerSink(inner event.Sink, cfg config.NotificationsConfig) event.Sink {
	if !cfg.Enabled {
		return inner
	}
	sender := a.desktopNotificationSender()
	if sender == nil {
		return inner
	}
	return notify.NewSink(inner, sender, cfg)
}

func setTabStartupError(tab *WorkspaceTab, err error) bool {
	if tab == nil {
		return false
	}
	tab.StartupErr = userFacingSessionLeaseError("", err).Error()
	tab.StartupErrLeaseHeld = errors.Is(err, agent.ErrSessionLeaseHeld)
	return tab.StartupErrLeaseHeld
}

func clearTabStartupError(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	tab.StartupErr = ""
	tab.StartupErrLeaseHeld = false
}

func (a *App) recordTabStartupFailure(tab *WorkspaceTab, buildGeneration uint64, wailsCtx context.Context, err error) {
	leaseHeld := false
	a.mu.Lock()
	if a.tabBuildSupersededLocked(tab, buildGeneration) {
		a.mu.Unlock()
		return
	}
	leaseHeld = setTabStartupError(tab, err)
	tab.Ready = false
	if leaseHeld {
		a.setSessionRuntimePhaseLocked(tab, sessionRuntimeLeaseBlocked, err)
	} else {
		a.setSessionRuntimePhaseLocked(tab, sessionRuntimeFailed, err)
	}
	tab.releaseSessionLease()
	a.mu.Unlock()
	if leaseHeld {
		a.scheduleDeferredStartupBuild(tab.ID)
	}
	a.emitReady(wailsCtx, tab.ID)
}

// closeTabBuildDone signals waiters (topic-activation completions) that the
// build owning buildGeneration has terminated. Every build funnels through
// buildTabControllerWithContextCore, whose deferred call guarantees
// the channel startTabControllerBuild created is closed exactly once, on every
// terminal path — success, failure, and superseded abandon alike. Synchronous
// rebuild paths pass generation 0 and never created a channel.
func (a *App) closeTabBuildDone(tab *WorkspaceTab, buildGeneration uint64) {
	if tab == nil || buildGeneration == 0 {
		return
	}
	a.mu.Lock()
	if tab.buildDoneGen == buildGeneration && tab.buildDone != nil {
		close(tab.buildDone)
		tab.buildDone = nil
	}
	a.mu.Unlock()
}

func (a *App) buildTabControllerWithContext(tab *WorkspaceTab, loadedSession loadedTabSession, buildCtx context.Context, buildGeneration uint64, buildCancel context.CancelFunc) {
	a.buildTabControllerWithContextCore(tab, loadedSession, buildCtx, buildGeneration, buildCancel)
}

// buildTabControllerWithContextCore performs configuration, session routing,
// and extension boot outside runtimeAdmissionMu. Only publication enters the
// lifecycle barrier.
func (a *App) buildTabControllerWithContextCore(tab *WorkspaceTab, loadedSession loadedTabSession, buildCtx context.Context, buildGeneration uint64, buildCancel context.CancelFunc) {
	defer a.recoverToPending("buildTabController")
	keepBuildContext := false
	defer func() {
		a.clearTabBuildCancel(tab, buildGeneration, buildCancel, keepBuildContext)
	}()
	defer a.closeTabBuildDone(tab, buildGeneration)
	if hook := a.tabBuildStartHook; hook != nil && tab != nil {

		hook(tab.ID)
	}
	wailsCtx := a.ctx
	if a.tabBuildSuperseded(tab, buildGeneration) {
		return
	}
	a.mu.Lock()
	if !tab.removed && tab.Ctrl == nil {
		tab.Ready = false
		clearTabStartupError(tab)
		a.setSessionRuntimePhaseLocked(tab, sessionRuntimeStarting, nil)
	}
	a.mu.Unlock()

	a.reconcileTabWithPinnedSessionMeta(tab)

	a.mu.RLock()
	tabWorkspaceRoot := tab.WorkspaceRoot
	tabScope := tab.Scope
	tabTopicID := tab.TopicID
	tabSessionPath := tab.SessionPath
	tabModel := tab.model
	tabSink := tab.sink
	a.mu.RUnlock()

	root := tabWorkspaceRoot
	if root == "" {
		if wd, err := os.Getwd(); err == nil {
			root = wd
		}
	}

	_ = config.MigrateLegacyCredentialsForRoot(root)
	cfg, err := config.LoadForRoot(root)
	if err != nil {
		a.recordTabStartupFailure(tab, buildGeneration, wailsCtx, err)
		return
	}

	if a.tabBuildSuperseded(tab, buildGeneration) {
		return
	}
	if tabSink != nil {
		tabSink.setContext(wailsCtx)
	}

	sessionDir := desktopSessionDir(root)
	if tabScope == "global" {
		sessionDir = desktopSessionDir(globalWorkspaceRoot())
	}
	topicID := strings.TrimSpace(tabTopicID)
	pinnedPath, hasPinnedPath := pinnedTabSessionPathForBuild(tabScope, tabWorkspaceRoot, sessionDir, tabSessionPath)
	if hasPinnedPath && agent.IsCleanupPending(pinnedPath) {

		hasPinnedPath = false
		pinnedPath = ""
	}
	catalogTopicPath := ""
	if hasPinnedPath {

		sessionDir = filepath.Dir(pinnedPath)
	} else {
		catalogTopicPath = a.catalogSessionPathForTopic(tabScope, tabWorkspaceRoot, topicID)
	}
	if !hasPinnedPath && catalogTopicPath != "" {
		sessionDir = filepath.Dir(catalogTopicPath)
	}
	startupSessionPath := ""
	if hasPinnedPath {
		if !agent.IsCleanupPending(pinnedPath) {
			startupSessionPath = pinnedPath
		}
	} else if catalogTopicPath != "" {
		startupSessionPath = catalogTopicPath
	}

	model := strings.TrimSpace(tabModel)
	if sessionModel, ok := agent.LoadSessionModel(startupSessionPath); ok {
		config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, sessionModel)
		if _, ok := cfg.ResolveModel(sessionModel); ok {
			model = sessionModel
		}
	}
	if model == "" {
		if def := strings.TrimSpace(cfg.DefaultModel); providerext.PluginRefOwner(def) != "" {

			model = def
		} else {
			resolved, _, ok := cfg.ResolveDesktopNewSessionModel()
			if !ok {
				a.recordTabStartupFailure(tab, buildGeneration, wailsCtx, errNoDesktopChatModel)
				return
			}
			model = resolved
		}
	}
	config.NormalizeLegacyMimoCustomProvidersForRefs(cfg, model)
	requestedModel := model
	if providerext.PluginRefOwner(model) == "" {

		if resolved, fallback, ok := cfg.ResolveModelWithFallback(model); ok {
			if fallback && strings.TrimSpace(tabModel) != "" {
				a.noticeForTab(tab.ID, fmt.Sprintf("model %q is no longer available; switched to %s", requestedModel, resolved))
			}
			model = resolved
		}
	}

	rootKey := tabWorkspaceRoot
	if rootKey == "" {
		rootKey = "__global__"
	}
	a.mu.Lock()
	if a.tabBuildSupersededLocked(tab, buildGeneration) {
		a.mu.Unlock()
		return
	}
	tab.model = model
	tab.Label = model
	tab.SharedHostKey = rootKey
	buildEffort := cloneStringPtr(tab.effort)
	buildTokenMode := boot.NormalizeTokenMode(tab.tokenMode)
	buildMode := tab.mode
	buildToolApprovalMode := tab.toolApprovalMode
	buildGoal := tab.goal
	buildSink := tab.sink
	a.saveTabsLocked()
	a.mu.Unlock()
	buildRuntime := (tabRuntimeSnapshot{
		tokenMode:        buildTokenMode,
		mode:             buildMode,
		goal:             buildGoal,
		toolApprovalMode: buildToolApprovalMode,
	}).normalizedRuntime()

	extensionGen := a.currentExtensionGeneration()
	sharedHost := a.acquireSharedHost(rootKey)
	sink := a.desktopControllerSink(buildSink, cfg.Notifications)
	buildCtx, registration := beginSharedHostMCPRegistration(buildCtx, sharedHost)
	defer registration.rollback()
	ctrl, err := a.buildTabControllerBootFenced(buildCtx, extensionGen, boot.Options{
		Model:                    model,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     sink,
		WorkspaceRoot:            root,
		SessionDir:               sessionDir,
		EffortOverride:           cloneStringPtr(buildEffort),
		AgentPreset:              boot.NormalizeAgentPreset(buildTokenMode),
		TokenMode:                buildTokenMode,
		SharedHost:               sharedHost,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),
	})
	if a.handleTabControllerBootError(tab, registration, rootKey, buildGeneration, wailsCtx, err) {
		return
	}
	if a.tabBuildSuperseded(tab, buildGeneration) {
		registration.rollback()
		a.abandonSupersededBuild(tab, ctrl, rootKey, "")
		return
	}
	if a.currentExtensionGeneration() != extensionGen {
		registration.rollback()
		a.abandonSupersededBuild(tab, ctrl, rootKey, "")
		a.scheduleDeferredStartupBuild(tab.ID)
		return
	}
	a.bindControllerDisplayRecorder(ctrl)
	configureControllerRuntime(ctrl, nil, buildRuntime)

	acquiredLeaseKey := ""
	restoredRuntime := buildRuntime
	if dir := ctrl.SessionDir(); dir != "" {

		a.mu.RLock()
		tabTopicID = strings.TrimSpace(tab.TopicID)
		tabSessionPath = tab.SessionPath
		a.mu.RUnlock()
		var path string
		var resumeSession *agent.Session
		var resumeLoadErr error

		if loaded, pinnedPath, ok, loadErr := loadPinnedTabSessionWithPreload(dir, tabSessionPath, loadedSession); loadErr != nil {
			resumeLoadErr = loadErr
		} else if ok {
			path = pinnedPath
			resumeSession = loaded
		}
		if resumeLoadErr == nil && path == "" && tabTopicID != "" {
			existingPath := a.catalogSessionPathForTopic(tabScope, tabWorkspaceRoot, tabTopicID)
			if existingPath != "" {
				if loaded, err := loadResumableSession(existingPath); err == nil {
					path = existingPath
					resumeSession = loaded
				} else {
					resumeLoadErr = err
				}
			}
		}
		if resumeLoadErr != nil {
			resumeLoadErr = friendlySessionLoadError(resumeLoadErr)
			leaseHeld := false
			a.mu.Lock()
			if a.tabBuildSupersededLocked(tab, buildGeneration) {
				a.mu.Unlock()
				a.abandonSupersededBuild(tab, ctrl, rootKey, "")
				return
			}
			leaseHeld = setTabStartupError(tab, resumeLoadErr)
			tab.Ready = false
			if leaseHeld {
				a.setSessionRuntimePhaseLocked(tab, sessionRuntimeLeaseBlocked, resumeLoadErr)
			} else {
				a.setSessionRuntimePhaseLocked(tab, sessionRuntimeFailed, resumeLoadErr)
			}
			hostKey := takeTabSharedHostKey(tab)
			tab.releaseSessionLease()
			a.mu.Unlock()
			ctrl.Close()
			if hostKey != "" {
				a.releaseSharedHost(hostKey)
			}
			if leaseHeld {
				a.scheduleDeferredStartupBuild(tab.ID)
			}
			a.emitReady(wailsCtx, tab.ID)
			return
		}
		if path == "" {
			path = agent.NewSessionPath(dir, ctrl.Label())
		}

		if path != "" {
			if a.claimSessionRuntime(tab, path, buildCtx) {
				ctrl.Close()
				a.releaseSharedHost(rootKey)
				a.emitReady(wailsCtx, tab.ID)
				return
			}
			preLeaseKey := tab.sessionLeaseRuntimeKey()
			if err := a.ensureTabSessionLeaseForRebuild(tab, path, ""); err != nil {
				leaseHeld := false
				a.mu.Lock()
				if a.tabBuildSupersededLocked(tab, buildGeneration) {
					a.mu.Unlock()
					a.abandonSupersededBuild(tab, ctrl, rootKey, "")
					return
				}
				leaseHeld = setTabStartupError(tab, err)
				tab.Ready = false
				if leaseHeld {
					a.setSessionRuntimePhaseLocked(tab, sessionRuntimeLeaseBlocked, err)
				} else {
					a.setSessionRuntimePhaseLocked(tab, sessionRuntimeFailed, err)
				}
				hostKey := takeTabSharedHostKey(tab)

				tab.releaseSessionLeaseForKey(sessionRuntimeKey(path))
				a.mu.Unlock()
				ctrl.Close()
				if hostKey != "" {
					a.releaseSharedHost(hostKey)
				}
				if leaseHeld {
					a.scheduleDeferredStartupBuild(tab.ID)
				}
				a.emitReady(wailsCtx, tab.ID)
				return
			}

			if key := sessionRuntimeKey(path); key != preLeaseKey {
				acquiredLeaseKey = key
			}

			if a.tabBuildSuperseded(tab, buildGeneration) {
				a.abandonSupersededBuild(tab, ctrl, rootKey, acquiredLeaseKey)
				return
			}
			var restoreErr error
			restoredRuntime, restoreErr = resumeControllerRuntimeWithSession(ctrl, resumeSession, path, buildRuntime)
			if restoreErr != nil {
				leaseHeld := false
				a.mu.Lock()
				if a.tabBuildSupersededLocked(tab, buildGeneration) {
					a.mu.Unlock()
					a.abandonSupersededBuild(tab, ctrl, rootKey, acquiredLeaseKey)
					return
				}
				leaseHeld = setTabStartupError(tab, restoreErr)
				tab.Ready = false
				if leaseHeld {
					a.setSessionRuntimePhaseLocked(tab, sessionRuntimeLeaseBlocked, restoreErr)
				} else {
					a.setSessionRuntimePhaseLocked(tab, sessionRuntimeFailed, restoreErr)
				}
				hostKey := takeTabSharedHostKey(tab)
				tab.releaseSessionLeaseForKey(sessionRuntimeKey(path))
				a.mu.Unlock()
				ctrl.Close()
				if hostKey != "" {
					a.releaseSharedHost(hostKey)
				}
				if leaseHeld {
					a.scheduleDeferredStartupBuild(tab.ID)
				}
				a.emitReady(wailsCtx, tab.ID)
				return
			}
			a.persistTabSessionPath(tab, path)
			a.mu.RLock()
			indexScope := tab.Scope
			indexRoot := tab.WorkspaceRoot
			indexTopicID := strings.TrimSpace(tab.TopicID)
			indexTopicTitle := tab.TopicTitle
			a.mu.RUnlock()
			if indexTopicID != "" {
				if err := ensureTopicIndexed(indexScope, indexRoot, indexTopicID, indexTopicTitle, loadTopicTitleSource(topicTitleRoot(indexScope, indexRoot), indexTopicID)); err == nil {
					a.emitProjectTreeChangedForSessionDirs(ctrl.SessionDir())
				}
			}

			snapshot := loadTelemetry(path + ".telemetry.json")
			tab.replaceTelemetry(snapshot, sessionRuntimeKey(path))
		}
	}

	releasePublication, extensionsCurrent := a.lockTabControllerPublication(extensionGen)
	if !extensionsCurrent {
		registration.rollback()
		a.abandonSupersededBuild(tab, ctrl, rootKey, acquiredLeaseKey)
		a.scheduleDeferredStartupBuild(tab.ID)
		return
	}
	defer releasePublication()
	a.mu.Lock()
	if a.tabBuildSupersededLocked(tab, buildGeneration) {
		a.mu.Unlock()
		a.abandonSupersededBuild(tab, ctrl, rootKey, acquiredLeaseKey)
		return
	}

	if !a.commitStartupWriteAuthorityLocked(tab, ctrl, registration, rootKey, acquiredLeaseKey, wailsCtx) {
		return
	}
	tab.Ctrl = ctrl
	tab.Label = ctrl.Label()
	applyNormalizedRuntimeToTabLocked(tab, restoredRuntime)
	tab.Ready = true
	clearTabStartupError(tab)
	a.bindSessionRuntimeKeyLocked(tab, tab.currentSessionPath())
	a.advanceSessionRuntimeEpochLocked(tab)
	keepBuildContext = true
	a.mu.Unlock()
	a.emitReady(wailsCtx, tab.ID)
}
