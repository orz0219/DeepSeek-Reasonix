package main

import (
	"fmt"
	"strings"
	"time"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/control"
	"reasonix/internal/provider"
)

// Compact runs a plain compaction pass (the "compact now" button). Focus-guided
// compaction goes through Submit("/compact <focus>") instead.
func (a *App) Compact() error {
	return a.CompactForTab("")
}

// CompactForTab compacts the requested tab without depending on which tab is
// focused when the asynchronous frontend call reaches the backend.
func (a *App) CompactForTab(tabID string) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return nil
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	ctrl = a.controllerForTab(tab)
	if ctrl == nil {
		return nil
	}
	return ctrl.Compact(a.ctx, "")
}

// workspaceNotReadyErr names why a session action arrived before the tab's
// controller existed: still starting, or failed to start. Silently returning
// nil here swallowed the click with no feedback (#3938).
//
// This is the bound-method form: StartupErr is written under a.mu by the
// build goroutine while Submit-family calls race it, so read it under the
// lock. Callers must not hold a.mu.
func (a *App) workspaceNotReadyErr(tab *WorkspaceTab) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.workspaceNotReadyErrLocked(tab)
}

func (a *App) workspaceNotReadyErrLocked(tab *WorkspaceTab) error {
	startupErr := ""
	var issue *SessionRuntimeIssue
	if tab != nil {
		startupErr = tab.StartupErr
		issue = a.sessionRuntimeViewLocked(tab).Issue
	}
	if strings.TrimSpace(startupErr) != "" {
		return fmt.Errorf("workspace failed to start: %s", startupErr)
	}
	if issue != nil && strings.TrimSpace(issue.Message) != "" {
		return fmt.Errorf("workspace failed to start: %s", issue.Message)
	}
	return fmt.Errorf("workspace is still starting")
}

// workspaceRuntimeAdmissionErr is the backend half of the composer readiness
// contract. The frontend gate avoids an optimistic bubble for known startup
// states; this check closes the race where a rebuild or lease failure lands
// after the last metadata refresh but before a bound Submit call.
func (a *App) workspaceRuntimeAdmissionErr(tab *WorkspaceTab, ctrl control.SessionAPI) error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if tab != nil && ctrl != nil && tab.Ctrl == ctrl {
		runtimeView := a.sessionRuntimeViewLocked(tab)
		if runtimeView.Phase == sessionRuntimeReady {
			return nil
		}
	}
	return a.workspaceNotReadyErrLocked(tab)
}

// tabIsReadOnly reads tab.ReadOnly under a.mu; setTabReadOnly can flip it
// concurrently with Submit-family bound calls. Callers must not hold a.mu.
func (a *App) tabIsReadOnly(tab *WorkspaceTab) bool {
	if tab == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return tab.ReadOnly
}

// NewSession snapshots the current conversation and rotates to a fresh one.
func (a *App) NewSession() error {
	return a.NewSessionForTab("")
}

// NewSessionForTab snapshots and rotates the requested tab regardless of which
// tab becomes active while the Wails call is in flight.
func (a *App) NewSessionForTab(tabID string) error {
	tab, ctrl := a.tabAndCtrlByID(tabID)
	if a.tabIsReadOnly(tab) {
		return readOnlyChannelErr()
	}
	if ctrl == nil {
		return a.workspaceNotReadyErr(tab)
	}
	if err := a.ensureTabControllerWorkspace(tab); err != nil {
		return err
	}
	ctrl = a.controllerForTab(tab)
	if ctrl == nil {
		return a.workspaceNotReadyErr(tab)
	}

	if !controllerHasActiveRuntimeWork(ctrl) && !messagesHaveConversationContent(ctrl.History()) {
		a.persistTabSessionPath(tab, ctrl.SessionPath())
		return nil
	}

	if err := ctrl.NewSession(); err != nil {
		return err
	}

	tab.resetTelemetry(ctrl.SessionPath())

	a.clearTabGoal(tab)
	a.assignFreshSessionTopic(tab)
	a.persistTabSessionPath(tab, ctrl.SessionPath())
	a.invalidatePromptHistoryCache()
	a.emitProjectTreeChangedForSessionDirs(ctrl.SessionDir())
	return nil
}

func (a *App) assignFreshSessionTopic(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	topicID := newTopicID()
	a.mu.Lock()
	scope := tab.Scope
	workspaceRoot := tab.WorkspaceRoot
	tab.TopicID = topicID
	tab.TopicTitle = defaultTopicTitle
	tab.topicTitleSource = topicTitleSourceAuto
	if current := a.tabs[tab.ID]; current == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	if strings.TrimSpace(scope) == "global" {
		workspaceRoot = ""
	} else {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
	}

	_ = ensureTopicIndexed(scope, workspaceRoot, topicID, defaultTopicTitle, topicTitleSourceAuto)
	_ = setTopicCreatedAt(topicTitleRoot(scope, workspaceRoot), topicID, time.Now().UnixMilli())
}

func (a *App) ensureTabTopicIndexedForUserTurn(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	topicID := newTopicID()
	a.mu.Lock()
	if strings.TrimSpace(tab.TopicID) != "" {
		a.mu.Unlock()
		return
	}
	scope := tab.Scope
	workspaceRoot := tab.WorkspaceRoot
	tab.TopicID = topicID
	tab.TopicTitle = defaultTopicTitle
	tab.topicTitleSource = topicTitleSourceAuto
	if current := a.tabs[tab.ID]; current == tab {
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	if strings.TrimSpace(scope) == "global" {
		scope = "global"
		workspaceRoot = ""
	} else {
		scope = "project"
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
	}

	_ = ensureTopicIndexed(scope, workspaceRoot, topicID, defaultTopicTitle, topicTitleSourceAuto)
	_ = setTopicCreatedAt(topicTitleRoot(scope, workspaceRoot), topicID, time.Now().UnixMilli())
	path := a.currentSessionPathFor(tab)
	a.persistTabSessionPath(tab, path)
	a.emitProjectTreeChangedForSessionDirs(sessionDirectoryForPath(path))
}

func messagesHaveConversationContent(messages []provider.Message) bool {
	for _, msg := range messages {
		if msg.Role != provider.RoleSystem {
			return true
		}
	}
	return false
}

func (a *App) clearActiveSessionRuntime(tab *WorkspaceTab, oldCtrl control.SessionAPI) (SessionClearResult, error) {
	if tab == nil || oldCtrl == nil {
		return SessionClearResult{}, fmt.Errorf("workspace is still starting")
	}

	a.runtimeRebuildMu.Lock()
	defer a.runtimeRebuildMu.Unlock()
	tab.turnStartMu.Lock()
	defer tab.turnStartMu.Unlock()

	a.sessionRemovalMu.Lock()
	defer a.sessionRemovalMu.Unlock()

	a.reconciledSessionPathForTab(tab)
	oldPath := oldCtrl.SessionPath()

	snap := a.tabRuntimeSnapshot(tab)
	oldSink := snap.sink
	if oldSink != nil {

		oldSink.setBinding(detachedRuntimeTabID(sessionRuntimeKey(oldPath)), nil)
		oldSink.clearContext()
	}
	if oldCtrl.RuntimeStatus().Cancellable {
		oldCtrl.Cancel()
		if err := waitControllerStopped(oldCtrl); err != nil {
			return SessionClearResult{}, err
		}
	}
	destroy := oldCtrl.BeginDestroySession(oldPath)
	destroys := []control.SessionDestroyHandle{destroy}
	teardownTimedOut := waitDestroyHandles(destroys)
	if teardownTimedOut {
		if err := agent.MarkCleanupPending(oldPath, "clear"); err != nil {
			return SessionClearResult{}, err
		}
	}

	newSink := &tabEventSink{tabID: tab.ID, app: a, ctx: a.ctx}
	sharedHost := a.lookupSharedHost(snap.sharedHostKey)
	newCtrl, err := boot.Build(a.bootContext(), boot.Options{
		Model:                    snap.model,
		RequireKey:               false,
		StatsSource:              "desktop",
		TaskStore:                a.taskStore(),
		OnConfigLoadWarnings:     a.configLoadWarningsHandler(),
		Sink:                     newSink,
		WorkspaceRoot:            snap.workspaceRoot,
		SessionDir:               sessionDirForSnapshot(snap),
		EffortOverride:           cloneStringPtr(snap.effort),
		AgentPreset:              boot.NormalizeAgentPreset(snap.currentTokenMode()),
		TokenMode:                snap.currentTokenMode(),
		SharedHost:               sharedHost,
		CleanupPendingReconciler: reconcileDesktopCleanupPending,
		SubagentParentLive:       a.subagentParentProbeForBuild(tab),
		SessionRecoveryMeta:      a.tabSessionRecoveryMeta(tab),
		OnSessionRecovered:       a.handleTabSessionRecovered(tab),
	})
	if err != nil {
		if teardownTimedOut {

			go delayedDesktopSessionCleanup(oldPath, destroys)
		} else {
			finishDestroyHandles(destroys)
		}
		if oldSink != nil {
			oldSink.setBinding(tab.ID, nil)
			oldSink.setContext(a.ctx)
		}
		return SessionClearResult{}, err
	}
	if teardownTimedOut {
		go delayedDesktopSessionCleanup(oldPath, destroys)
	} else {
		if err := removeDesktopSessionArtifacts(oldPath); err != nil {
			finishDestroyHandles(destroys)
			newCtrl.Close()
			return SessionClearResult{}, err
		}
		finishDestroyHandles(destroys)
	}
	a.bindControllerDisplayRecorder(newCtrl)
	newCtrl.EnableInteractiveApproval()
	applyTabModeToController(newCtrl, snap.mode)
	applyTabToolApprovalModeToController(newCtrl, snap.toolApprovalMode)

	path := agent.NewSessionPath(newCtrl.SessionDir(), newCtrl.Label())
	if err := a.ensureTabSessionLeaseForRebuild(tab, path, ""); err != nil {
		newCtrl.Close()

		return SessionClearResult{}, userFacingSessionLeaseError("", err)
	}
	newCtrl.SetFreshSessionPath(path)

	a.mu.Lock()
	if err := a.authorizeTabReplacementLocked(tab, newCtrl, "clearing the session", "fresh"); err != nil {
		a.mu.Unlock()

		newCtrl.Close()
		tab.releaseSessionLease()
		oldCtrl.CloseAfterDestroy()
		a.emitProjectTreeChangedForSessionDirs(newCtrl.SessionDir())
		return SessionClearResult{}, err
	}
	tab.Ctrl = newCtrl
	tab.sink = newSink
	tab.SessionPath = path
	tab.Label = newCtrl.Label()
	tab.Ready = true
	clearTabStartupError(tab)
	tab.goal = ""

	a.supersedeTabBuildLocked(tab)
	a.saveTabsLocked()
	a.mu.Unlock()

	tab.resetTelemetry(path)
	a.persistTabSessionPath(tab, path)
	oldCtrl.CloseAfterDestroy()
	a.emitProjectTreeChangedForSessionDirs(newCtrl.SessionDir())
	a.notifyTabRuntimeRebuilt(tab)
	return a.bumpAndSnapshotSessionClear(tab), nil
}

func removeDesktopSessionArtifacts(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	guard, err := acquireSessionRemovalGuard(path)
	if err != nil {
		return err
	}
	return removeDesktopSessionArtifactsWithGuard(path, guard)
}

// CheckpointMeta summarises one rewind point (a user turn) for the desktop.
// Optional v2 fields use omitempty so older frontends keep reading the rest.
type CheckpointMeta struct {
	Turn               int      `json:"turn"`
	Prompt             string   `json:"prompt"`
	Files              []string `json:"files"`     // stable preview of cumulative files RestoreCode would affect from this turn
	FileCount          int      `json:"fileCount"` // full cumulative file count, including entries omitted from Files
	FilesTruncated     bool     `json:"filesTruncated,omitempty"`
	TurnFileCount      int      `json:"turnFileCount"` // files changed during this turn only
	Time               int64    `json:"time"`          // unix milliseconds
	CanCode            bool     `json:"canCode"`
	CanConversation    bool     `json:"canConversation"`
	Coverage           string   `json:"coverage,omitempty"`
	CoverageGaps       []string `json:"coverageGaps,omitempty"`
	ExpiredFilePayload bool     `json:"expiredFilePayload,omitempty"`
	ActiveWriters      int      `json:"activeWriters,omitempty"`
	Legacy             bool     `json:"legacy,omitempty"`
	CanUndoFiles       bool     `json:"canUndoFiles,omitempty"`
	DisabledReason     string   `json:"disabledReason,omitempty"`
}

// RewindPlanView is the desktop-facing prepare result.
type RewindPlanView struct {
	PlanID             string   `json:"planId"`
	Turn               int      `json:"turn"`
	Scope              string   `json:"scope"`
	Coverage           string   `json:"coverage,omitempty"`
	CoverageGaps       []string `json:"coverageGaps,omitempty"`
	Legacy             bool     `json:"legacy,omitempty"`
	ExpiredFilePayload bool     `json:"expiredFilePayload,omitempty"`
	CanFiles           bool     `json:"canFiles"`
	CanConversation    bool     `json:"canConversation"`
	DisabledReason     string   `json:"disabledReason,omitempty"`
	Conflicts          []string `json:"conflicts,omitempty"`
	Files              []string `json:"files,omitempty"`
	FileCount          int      `json:"fileCount"`
	ActiveWriters      int      `json:"activeWriters,omitempty"`
	Path               string   `json:"path,omitempty"`
	OK                 bool     `json:"ok"`
	Error              string   `json:"error,omitempty"`
}

// RewindResultView is the desktop-facing commit/undo result.
type RewindResultView struct {
	OK             bool     `json:"ok"`
	TransactionID  string   `json:"transactionId,omitempty"`
	UndoAvailable  bool     `json:"undoAvailable"`
	Written        []string `json:"written,omitempty"`
	Deleted        []string `json:"deleted,omitempty"`
	ConversationOK bool     `json:"conversationOk,omitempty"`
	Error          string   `json:"error,omitempty"`
	Conflicts      []string `json:"conflicts,omitempty"`
	Coverage       string   `json:"coverage,omitempty"`
}

const checkpointFilePreviewLimit = 60
