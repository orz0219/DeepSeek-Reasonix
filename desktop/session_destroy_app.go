package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"reasonix/internal/agent"
	"reasonix/internal/boot"
	"reasonix/internal/control"
)

func (a *App) openTransientBlankRuntime(scope, workspaceRoot string) error {
	scope = strings.TrimSpace(scope)
	if scope != "project" {
		scope = "global"
	}
	actualRoot := ""
	if scope == "project" {
		workspaceRoot = normalizeProjectRoot(workspaceRoot)
		if workspaceRoot == "" {
			return fmt.Errorf("workspaceRoot is required")
		}
		saveWorkspace(workspaceRoot)
		a.registerProjectRoot(workspaceRoot)
		actualRoot = workspaceRoot
	} else {
		actualRoot = globalWorkspaceRoot()
		if err := os.MkdirAll(actualRoot, 0o755); err != nil {
			return fmt.Errorf("create global workspace: %w", err)
		}
	}

	model, toolApprovalMode := desktopNewSessionDefaults(scope, actualRoot)
	sessionPath, err := createEmptySessionFile(desktopSessionDir(actualRoot), model)
	if err != nil {
		return err
	}
	if err := pinNewEmptySessionBranchMeta(sessionPath, scope, actualRoot, "", defaultTopicTitle); err != nil {
		return err
	}
	tab := &WorkspaceTab{
		Scope:            scope,
		WorkspaceRoot:    actualRoot,
		TopicTitle:       defaultTopicTitle,
		topicTitleSource: topicTitleSourceAuto,
		SessionPath:      sessionPath,
		model:            model,
		tokenMode:        boot.TokenModeFull,
		mode:             tabModeFromAxes(false, toolApprovalMode == control.ToolApprovalYolo),
		toolApprovalMode: toolApprovalMode,
		disabledMCP:      map[string]ServerView{},
	}
	a.mu.Lock()
	tab.ID = a.newUniqueTabIDLocked()
	tab.sink = &tabEventSink{tabID: tab.ID, app: a}
	a.tabs[tab.ID] = tab
	a.tabOrder = append(a.tabOrder, tab.ID)
	a.activeTabID = tab.ID
	a.saveTabsLocked()
	a.mu.Unlock()

	a.startTabControllerBuild(tab)
	return nil
}

func (a *App) beginDestroySessionJobs(dir, sessionPath string) []control.SessionDestroyHandle {
	a.mu.RLock()
	defer a.mu.RUnlock()
	var destroys []control.SessionDestroyHandle
	for _, tab := range a.runtimeTabsLocked() {
		if tab == nil || tab.Ctrl == nil || tabRuntimeSessionDir(tab) != dir {
			continue
		}
		destroys = append(destroys, tab.Ctrl.BeginDestroySession(sessionPath))
	}
	return destroys
}

func (a *App) openSessionPaths(dir string) map[string]struct{} {
	a.mu.RLock()
	paths := make([]string, 0, len(a.tabs)+len(a.detachedSessions))
	for _, tab := range a.runtimeTabsLocked() {
		if tab != nil {
			paths = append(paths, tab.currentSessionPath())
		}
	}
	a.mu.RUnlock()

	out := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		currentPath, _, err := validateSessionPath(dir, path)
		if err == nil {
			out[currentPath] = struct{}{}
		}
	}
	return out
}

func (a *App) activeSessionPath(dir string) string {
	a.mu.RLock()
	var path string
	if tab := a.tabs[a.activeTabID]; tab != nil {
		path = tab.currentSessionPath()
	}
	a.mu.RUnlock()
	currentPath, _, err := validateSessionPath(dir, path)
	if err != nil {
		return ""
	}
	return currentPath
}

// RestoreSession moves a trashed session back into the saved-session list.
func (a *App) RestoreSession(path string) error {
	return friendlySessionFileError(a.restoreSession(path))
}

func (a *App) restoreSession(path string) error {
	dir, err := a.trashedSessionDir(path)
	if err != nil {
		return err
	}
	_, key, _, err := validateTrashedSessionPath(dir, path)
	if err != nil {
		return err
	}

	a.sessionRemovalMu.Lock()
	defer a.sessionRemovalMu.Unlock()
	target := filepath.Join(dir, key)
	if a.sessionDestroying(dir, target) {
		return fmt.Errorf("session cleanup is still in progress: %s", key)
	}
	if a.sessionOpen(dir, target) {
		return fmt.Errorf("session is open: %s", key)
	}
	if err := restoreTrashedSessionFile(dir, path); err != nil {
		return err
	}
	if err := restoreSessionTopicIndex(dir, target); err != nil {
		return err
	}
	a.requestSessionCatalogPath("", "", target)
	a.emitProjectTreeChangedForSessionDirs(dir)
	a.invalidatePromptHistoryCache()
	return nil
}

func (a *App) sessionDestroying(dir, sessionPath string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tab := range a.runtimeTabsLocked() {
		if tab == nil || tab.Ctrl == nil || tabRuntimeSessionDir(tab) != dir {
			continue
		}
		if tab.Ctrl.IsDestroyingSession(sessionPath) {
			return true
		}
	}
	return false
}

func (a *App) sessionOpen(dir, sessionPath string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	for _, tab := range a.runtimeTabsLocked() {
		if tabMatchesSession(tab, dir, sessionPath) {
			return true
		}
	}
	return false
}

// PurgeTrashedSession permanently removes a trashed session and its title/display
// sidecars.
func (a *App) PurgeTrashedSession(path string) error {
	return friendlySessionFileError(a.purgeTrashedSession(path, false))
}

// PurgeRecoveryCopy is the guarded permanent-cleanup path. A trashed branch is
// rechecked against its live parent; missing, stale, or divergent data is kept.
func (a *App) PurgeRecoveryCopy(path string) error {
	return friendlySessionFileError(a.purgeTrashedSession(path, true))
}

func (a *App) purgeTrashedSession(path string, requireRedundantRecovery bool) error {
	dir, err := a.trashedSessionDir(path)
	if err != nil {
		return err
	}
	a.sessionRemovalMu.Lock()
	defer a.sessionRemovalMu.Unlock()
	var parentGuard *agent.SessionRemovalGuard
	if requireRedundantRecovery {
		parentGuard, err = agent.TryAcquireRecoveryParentGuard(path, dir)
		if err != nil {
			switch {
			case errors.Is(err, agent.ErrRecoveryBranchNotCovered):
				return errRecoveryCopyNotRedundant
			case errors.Is(err, agent.ErrSessionLeaseHeld):
				return errSessionBusyElsewhere
			default:
				return err
			}
		}
		defer parentGuard.Release()
	}
	if err := purgeTrashedSessionFile(dir, path); err != nil {
		return err
	}
	a.invalidatePromptHistoryCache()
	return nil
}

// RenameSession sets a custom display name for a session (empty clears it back to
// the preview). The transcript file is unchanged; the canonical name lives in
// the branch meta sidecar, with the legacy .titles.json map kept as a
// compatibility write-through for older desktop data paths.
func (a *App) RenameSession(path, title string) error {
	dir := a.activeSessionDir()
	sessionPath, _, err := validateSessionPath(dir, path)
	if err != nil {
		return err
	}
	if err := agent.RenameSession(sessionPath, title); err != nil {
		return err
	}
	if err := setSessionTitle(dir, sessionPath, title); err != nil {
		return err
	}
	a.requestSessionCatalogPath("", "", sessionPath)
	a.invalidatePromptHistoryCache()
	a.emitProjectTreeChangedForSessionDirs(dir)
	return nil
}
