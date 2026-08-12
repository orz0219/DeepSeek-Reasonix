package main

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"reasonix/internal/config"
	"reasonix/internal/control"
	"reasonix/internal/event"
)

func (a *App) controllerForTab(tab *WorkspaceTab) control.SessionAPI {
	if tab == nil {
		return nil
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if tab.ID != "" && a.tabs[tab.ID] != tab {
		return nil
	}
	return tab.Ctrl
}

// currentSessionPathFor is the locked form of tab.currentSessionPath: it
// snapshots Ctrl/SessionPath under a.mu, then queries the controller off-lock.
// Use it on paths that do not otherwise hold a.mu.
func (a *App) currentSessionPathFor(tab *WorkspaceTab) string {
	if tab == nil {
		return ""
	}
	a.mu.RLock()
	ctrl := tab.Ctrl
	fallback := strings.TrimSpace(tab.SessionPath)
	a.mu.RUnlock()
	if ctrl != nil {
		if path := strings.TrimSpace(ctrl.SessionPath()); path != "" {
			return path
		}
	}
	return fallback
}

// sessionDirForSnapshot mirrors tabSessionDir for callers that hold a
// tabRuntimeSnapshot instead of reading the live tab.
func sessionDirForSnapshot(s tabRuntimeSnapshot) string {
	if s.workspaceRoot != "" {
		return desktopSessionDir(s.workspaceRoot)
	}
	if s.ctrl != nil {
		if dir := s.ctrl.SessionDir(); dir != "" {
			return dir
		}
	}
	return desktopSessionDir("")
}

func readOnlyChannelErr() error {
	return fmt.Errorf("channel session is read-only")
}

func (a *App) snapshotTab(tab *WorkspaceTab) error {
	if tab == nil {
		return nil
	}
	a.mu.RLock()
	readOnly := tab.ReadOnly
	ctrl := tab.Ctrl
	a.mu.RUnlock()
	if readOnly || ctrl == nil {
		return nil
	}
	return ctrl.Snapshot()
}

func (a *App) snapshotTabForAction(tab *WorkspaceTab, action string) error {
	if err := a.snapshotTab(tab); err != nil {
		a.reportTabSnapshotError(tab, action, err)
		if strings.TrimSpace(action) == "" {
			return fmt.Errorf("save current session: %w", err)
		}
		return fmt.Errorf("save current session before %s: %w", action, err)
	}
	return nil
}

func (a *App) reportTabSnapshotError(tab *WorkspaceTab, action string, err error) {
	if err == nil {
		return
	}
	tabID := ""
	if tab != nil {
		tabID = tab.ID
	}
	slog.Warn("desktop: session snapshot failed", "tab", tabID, "action", action, "err", err)
	if tab == nil || tab.sink == nil {
		return
	}

	if action == "autosave" {
		tab.saveMu.Lock()
		now := time.Now()
		if !tab.lastAutosaveWarnAt.IsZero() && now.Sub(tab.lastAutosaveWarnAt) < autosaveWarnInterval {
			tab.saveMu.Unlock()
			return
		}
		tab.lastAutosaveWarnAt = now
		tab.saveMu.Unlock()
	}
	prefix := "Session autosave failed"
	if strings.TrimSpace(action) != "" && action != "autosave" {
		prefix = "Session save failed before " + action
	}
	tab.sink.Emit(event.Event{Kind: event.Notice, Level: event.LevelWarn, Text: prefix + ": " + err.Error()})
}

func (a *App) reconciledSessionPathForTab(tab *WorkspaceTab) string {
	if tab == nil {
		return ""
	}
	path, _ := a.reconcileTabWithPinnedSessionMeta(tab)
	if ctrl := a.controllerForTab(tab); path == "" && ctrl != nil {
		path = ctrl.SessionPath()
	}
	return path
}

func (a *App) ensureTabControllerWorkspace(tab *WorkspaceTab) error {
	if tab == nil {
		return nil
	}
	tab.reconcileMu.Lock()
	defer tab.reconcileMu.Unlock()

	a.mu.RLock()
	current := a.tabs[tab.ID]
	ctrl := tab.Ctrl
	readOnly := tab.ReadOnly
	a.mu.RUnlock()
	if current != tab || ctrl == nil || readOnly {
		return nil
	}
	if controllerHasActiveRuntimeWork(ctrl) {
		return nil
	}
	path, hasBinding := a.reconcileTabWithPinnedSessionMeta(tab)
	desiredRoot := strings.TrimSpace(tab.WorkspaceRoot)
	ctrlRoot, rootOK := safeControllerWorkspaceRoot(ctrl)
	ctrlDir, dirOK := safeControllerSessionDir(ctrl)
	if !rootOK || !dirOK {
		return nil
	}
	if !hasBinding {
		if desiredRoot == "" || strings.TrimSpace(ctrlRoot) == "" || sameDesktopPath(ctrlRoot, desiredRoot) {
			return nil
		}
	}
	desiredDir := tabSessionDir(tab)
	rootMatches := desiredRoot == "" || sameDesktopPath(ctrlRoot, desiredRoot)
	dirMatches := desiredDir == "" || sameDesktopPath(ctrlDir, desiredDir)
	if !dirMatches && path != "" {
		if validPath, _, err := validateSessionPath(ctrlDir, path); err == nil && sessionRuntimeKey(validPath) == sessionRuntimeKey(path) {
			dirMatches = true
		}
	}
	if strings.TrimSpace(ctrlRoot) == "" && dirMatches {
		rootMatches = true
	}
	if tab.Scope == "global" {
		if strings.TrimSpace(ctrlRoot) == "" {
			rootMatches = true
		}
		if sameDesktopPath(ctrlDir, config.SessionDir()) || sameDesktopPath(ctrlDir, desktopSessionDir(globalWorkspaceRoot())) {
			dirMatches = true
		}
	}
	sessionMatches := path == "" || sessionRuntimeKey(ctrl.SessionPath()) == sessionRuntimeKey(path)
	if rootMatches && dirMatches && sessionMatches {
		return nil
	}
	if err := ctrl.Snapshot(); err != nil {
		return err
	}
	ctrl.Close()

	a.mu.Lock()
	var hostKey string
	if current := a.tabs[tab.ID]; current == tab {
		tab.Ctrl = nil
		tab.Ready = false
		clearTabStartupError(tab)
		tab.ActivityStatus = ""
		if tab.sink == nil {
			tab.sink = &tabEventSink{tabID: tab.ID, app: a, ctx: a.ctx}
		}
		hostKey = takeTabSharedHostKey(tab)
		a.saveTabsLocked()
	}
	a.mu.Unlock()
	if hostKey != "" {
		a.releaseSharedHost(hostKey)
	}

	a.buildTabController(tab)
	if tab.Ctrl == nil {
		if tab.StartupErr != "" {
			return fmt.Errorf("workspace failed to restart with corrected root: %s", tab.StartupErr)
		}
		return fmt.Errorf("workspace failed to restart with corrected root")
	}
	return nil
}

func safeControllerWorkspaceRoot(ctrl control.SessionAPI) (root string, ok bool) {
	if ctrl == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			root = ""
			ok = false
		}
	}()
	return ctrl.WorkspaceRoot(), true
}

func safeControllerSessionDir(ctrl control.SessionAPI) (dir string, ok bool) {
	if ctrl == nil {
		return "", false
	}
	defer func() {
		if recover() != nil {
			dir = ""
			ok = false
		}
	}()
	return ctrl.SessionDir(), true
}
