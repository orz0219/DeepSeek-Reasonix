package main

import (
	"context"
	"fmt"
	"log/slog"
	"reasonix/internal/control"
	"strings"
)

// SetActiveTab switches the frontend's active tab. A no-op when tabID is
// already active or unknown.
func (a *App) SetActiveTab(tabID string) error {
	a.mu.RLock()
	_, ok := a.tabs[tabID]
	alreadyActive := a.activeTabID == tabID
	a.mu.RUnlock()
	if !ok {
		return fmt.Errorf("tab %q not found", tabID)
	}
	if alreadyActive {
		return nil
	}
	a.mu.RLock()
	active := a.tabs[a.activeTabID]
	a.mu.RUnlock()
	if err := a.snapshotTabForAction(active, "switching tabs"); err != nil {
		return err
	}

	a.mu.Lock()
	if _, ok := a.tabs[tabID]; !ok {
		a.mu.Unlock()
		return fmt.Errorf("tab %q not found", tabID)
	}
	if a.activeTabID == tabID {
		a.mu.Unlock()
		return nil
	}
	a.activeTabID = tabID
	next := a.tabs[tabID]

	supersededReq, supersededTab := a.supersedePendingTopicActivationLocked(tabID, false)
	dir, entries, activeID, version := a.saveTabsCollectLocked()
	a.mu.Unlock()

	a.saveTabsWrite(dir, entries, activeID, version)
	if active != nil {
		active.clearRuntimeDisplayCurrency()
	}
	if next != nil {
		next.clearRuntimeDisplayCurrency()
	}
	if supersededReq != "" {
		a.emitTopicActivation(TopicActivationEvent{RequestID: supersededReq, TabID: supersededTab, Phase: topicActivationPhaseCancelled})
	}
	a.kickDeferredRebuildRetry()
	return nil
}

// ReorderTabs persists the frontend's manual tab order. The submitted order must
// contain every currently open tab exactly once.
func (a *App) ReorderTabs(tabIDs []string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(tabIDs) != len(a.tabs) {
		return fmt.Errorf("tab order length mismatch")
	}
	seen := make(map[string]bool, len(tabIDs))
	next := make([]string, 0, len(tabIDs))
	for _, id := range tabIDs {
		if _, ok := a.tabs[id]; !ok {
			return fmt.Errorf("tab %q not found", id)
		}
		if seen[id] {
			return fmt.Errorf("duplicate tab %q", id)
		}
		seen[id] = true
		next = append(next, id)
	}
	a.tabOrder = next
	a.saveTabsLocked()
	return nil
}

// CloseTab removes a visible tab. If the tab's session still has foreground or
// background work, the controller is detached so closing a view does not destroy
// the session runtime.
func (a *App) CloseTab(tabID string) error {
	return a.closeTab(tabID, true)
}

func (a *App) closeTab(tabID string, allowDetach bool) error {
	defer a.lockRuntimeMutation("close-tab")()
	a.sessionRemovalMu.Lock()
	defer a.sessionRemovalMu.Unlock()

	a.mu.Lock()
	tab, ok := a.tabs[tabID]
	if !ok {
		a.mu.Unlock()
		return fmt.Errorf("tab %q not found", tabID)
	}
	if len(a.tabs) <= 1 {
		a.mu.Unlock()
		return fmt.Errorf("cannot close the last tab")
	}
	a.mu.Unlock()

	if err := a.snapshotTab(tab); err != nil {
		slog.Warn("desktop: snapshot before closing tab failed", "tab", tabID, "err", err)
		return fmt.Errorf("save current session before closing tab: %w", err)
	}
	if err := a.saveTabSessionMetaForCurrentSession(tab); err != nil {
		slog.Warn("desktop: session metadata before closing tab failed", "tab", tabID, "err", err)
		return fmt.Errorf("save current session metadata before closing tab: %w", err)
	}

	if a.terminals != nil {
		a.terminals.closeForTab(tabID)
	}

	a.mu.Lock()
	if current := a.tabs[tabID]; current != tab {
		a.mu.Unlock()
		if current == nil {
			return fmt.Errorf("tab %q not found", tabID)
		}
		return fmt.Errorf("tab %q changed while closing", tabID)
	}
	if len(a.tabs) <= 1 {
		a.mu.Unlock()
		return fmt.Errorf("cannot close the last tab")
	}
	if !allowDetach && tab.hasActiveRuntimeWork() {
		a.mu.Unlock()
		return fmt.Errorf("task still has active work")
	}
	if tab.Ctrl == nil || !tab.hasActiveRuntimeWork() {
		a.markTabRemovedLocked(tab)
	}

	ordered := a.orderedTabIDsLocked()
	closedIndex := -1
	for i, id := range ordered {
		if id == tabID {
			closedIndex = i
			break
		}
	}
	delete(a.tabs, tabID)
	a.removeTabOrderLocked(tabID)
	wasActive := a.activeTabID == tabID
	if wasActive {
		a.activeTabID = ""
		if len(a.tabOrder) > 0 {
			nextIndex := max(closedIndex, 0)
			if nextIndex >= len(a.tabOrder) {
				nextIndex = len(a.tabOrder) - 1
			}
			a.activeTabID = a.tabOrder[nextIndex]
		}
	}
	a.saveTabsLocked()

	closeCtrl := tab.Ctrl
	closeSink := tab.sink
	a.mu.Unlock()
	if a.workspaceHub != nil {
		a.workspaceHub.reconcileRoots()
	}

	discardPath, discardTransientBlank := a.transientBlankSessionArtifactPath(tab)
	if closeCtrl != nil {
		if allowDetach && controllerHasActiveRuntimeWork(closeCtrl) && a.detachSessionRuntime(tab) {

			return nil
		}
		closeCtrl.SetSessionPath("")
		a.quiesceTabAutosave(tab)
		closeCtrl.Cancel()
		closeCtrl.Close()

		a.releaseTabSharedHost(tab)
		tab.releaseSessionLease()
	}
	if closeSink != nil {
		closeSink.clearContext()
	}
	if discardTransientBlank {
		discardTransientBlankSessionArtifacts(discardPath)
	}
	return nil
}

func (a *App) keepOnlyVisibleTab(tabID string) (TabMeta, error) {
	type pruneCandidate struct {
		id  string
		tab *WorkspaceTab
	}

	meta, err := func() (TabMeta, error) {
		defer a.lockRuntimeMutation("prune-visible-tabs")()
		a.sessionRemovalMu.Lock()
		defer a.sessionRemovalMu.Unlock()

		a.mu.Lock()
		active := a.tabs[tabID]
		if active == nil {
			a.mu.Unlock()
			return TabMeta{}, fmt.Errorf("tab %q not found", tabID)
		}
		candidates := make([]pruneCandidate, 0, len(a.tabs)-1)
		for id, tab := range a.tabs {
			if id == tabID {
				continue
			}
			candidates = append(candidates, pruneCandidate{id: id, tab: tab})
		}
		a.mu.Unlock()

		snapshotted := make(map[string]*WorkspaceTab, len(candidates))
		for _, candidate := range candidates {
			id, tab := candidate.id, candidate.tab
			snapshotted[id] = tab
			if err := a.snapshotTab(tab); err != nil {
				slog.Warn("desktop: snapshot before pruning hidden tab failed", "tab", id, "err", err)
				return TabMeta{}, fmt.Errorf("save current session before switching tabs: %w", err)
			}
			if err := a.saveTabSessionMetaForCurrentSession(tab); err != nil {
				slog.Warn("desktop: session metadata before pruning hidden tab failed", "tab", id, "err", err)
				return TabMeta{}, fmt.Errorf("save current session metadata before switching tabs: %w", err)
			}
		}

		a.mu.Lock()
		active = a.tabs[tabID]
		if active == nil {
			a.mu.Unlock()
			return TabMeta{}, fmt.Errorf("tab %q not found", tabID)
		}
		for id, tab := range a.tabs {
			if id != tabID && snapshotted[id] != tab {
				a.mu.Unlock()
				return TabMeta{}, fmt.Errorf("visible tabs changed while switching; retry")
			}
		}
		a.activeTabID = tabID
		removed := make([]*WorkspaceTab, 0, len(candidates))
		for _, candidate := range candidates {
			id, tab := candidate.id, candidate.tab
			if tab == nil || a.tabs[id] != tab {
				continue
			}
			if tab.Ctrl == nil || !tab.hasActiveRuntimeWork() {
				a.markTabRemovedLocked(tab)
			}
			removed = append(removed, tab)
			delete(a.tabs, id)
			a.removeTabOrderLocked(id)
		}
		a.tabOrder = []string{tabID}
		a.saveTabsLocked()
		meta := a.tabMeta(active, true)
		a.mu.Unlock()

		for _, tab := range removed {
			a.removeVisibleTabRuntimeAdmissionHeld(tab)
		}
		return meta, nil
	}()
	if err != nil {
		return TabMeta{}, err
	}
	a.emitProjectTreeChanged()
	return enrichTabMeta(meta), nil
}

func (a *App) applySingleSurfaceTabPolicy() error {
	a.singleSurfaceMu.Lock()
	defer a.singleSurfaceMu.Unlock()

	a.mu.RLock()
	tabID := a.activeTabID
	if tabID == "" || a.tabs[tabID] == nil {
		for _, id := range a.tabOrder {
			if a.tabs[id] != nil {
				tabID = id
				break
			}
		}
		if tabID == "" {
			for id := range a.tabs {
				tabID = id
				break
			}
		}
	}
	a.mu.RUnlock()
	if tabID == "" {
		return nil
	}
	_, err := a.keepOnlyVisibleTab(tabID)
	return err
}

func (a *App) removeVisibleTabRuntimeAdmissionHeld(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	if err := a.snapshotTab(tab); err != nil {
		slog.Warn("desktop: snapshot before removing visible tab runtime failed", "tab", tab.ID, "err", err)
	}
	discardPath, discardTransientBlank := a.transientBlankSessionArtifactPath(tab)
	a.mu.RLock()
	ctrl := tab.Ctrl
	a.mu.RUnlock()
	if ctrl != nil && controllerHasActiveRuntimeWork(ctrl) && a.detachSessionRuntime(tab) {
		return
	}
	a.markTabRemoved(tab)
	a.closeTabRuntimeAdmissionHeld(tab)
	if discardTransientBlank {
		discardTransientBlankSessionArtifacts(discardPath)
	}
}

// transientBlankSessionArtifactPath reports the artifact path to discard when
// closing a still-blank tab. It snapshots the racy tab fields under a.mu and
// keeps the file probe (sessionPathHasNoContent) outside the lock. Callers
// must not hold a.mu.
func (a *App) transientBlankSessionArtifactPath(tab *WorkspaceTab) (string, bool) {
	if tab == nil {
		return "", false
	}
	snap := a.tabRuntimeSnapshot(tab)
	if snap.readOnly || strings.TrimSpace(snap.topicID) != "" || controllerHasActiveRuntimeWork(snap.ctrl) {
		return "", false
	}
	if strings.TrimSpace(snap.sessionPath) == "" {
		return "", false
	}
	dir := sessionDirForSnapshot(snap)
	if !sessionPathHasNoContent(dir, snap.sessionPath) {
		return "", false
	}
	path, ok := pinnedTabSessionPath(dir, snap.sessionPath)
	if !ok {
		return "", false
	}
	return path, true
}

func discardTransientBlankSessionArtifacts(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	if err := removeDesktopSessionArtifacts(path); err != nil {
		slog.Warn("desktop: discard transient blank session artifacts failed", "path", path, "err", err)
	}
}

func (a *App) markTabRemoved(tab *WorkspaceTab) {
	a.mu.Lock()
	a.markTabRemovedLocked(tab)
	a.mu.Unlock()
}

func (a *App) markTabRemovedLocked(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	tab.removed = true
	if tab.buildCancel != nil {
		tab.buildCancel()
		tab.buildCancel = nil
	}
}

// tabBuildSupersededLocked reports whether an in-flight build lost ownership
// of its tab: the tab was removed/replaced, or a session rebind bumped
// buildGeneration to invalidate it. Generation 0 marks the synchronous
// rebuild paths, which serialize through runtimeRebuildMu instead and are
// never superseded by generation bumps. Callers must hold a.mu.
func (a *App) tabBuildSupersededLocked(tab *WorkspaceTab, generation uint64) bool {
	if tab == nil || tab.removed || a.shuttingDown.Load() || a.tabs[tab.ID] != tab {
		return true
	}
	return generation != 0 && tab.buildGeneration != generation
}

func (a *App) tabBuildSuperseded(tab *WorkspaceTab, generation uint64) bool {
	if tab == nil {
		return true
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.tabBuildSupersededLocked(tab, generation)
}

// supersedeTabBuildLocked invalidates any in-flight startup build and cancels
// its context. A synchronous rebuild (model/effort/token switch) that has
// already installed its controller calls this so a slower blank-session build
// cannot finish afterward, overwrite tab.Ctrl, and release or steal the
// session lease the switch just bound. Callers must hold a.mu.
func (a *App) supersedeTabBuildLocked(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	tab.buildGeneration++
	if tab.buildCancel != nil {
		tab.buildCancel()
		tab.buildCancel = nil
	}
}

// abandonSupersededBuild cleans up after a build that lost tab ownership
// mid-flight (removed tab, or a session rebind bumped the generation). It
// releases only what THIS build acquired — its controller, its own
// shared-host reference (rootKey), and the session lease bound to its own
// path (leaseKey) — and never reads or clears the tab's SharedHostKey or
// lease outright: on a live rebound tab the replacement build may already
// have published its own key and lease there, and taking those would leak
// the new runtime's host reference (or close a host still in use) and strip
// the new session's lease. Callers must not hold a.mu.
func (a *App) abandonSupersededBuild(tab *WorkspaceTab, ctrl control.SessionAPI, rootKey, leaseKey string) {
	if ctrl != nil {
		ctrl.Close()
	}
	if rootKey != "" {
		a.releaseSharedHost(rootKey)
	}
	tab.releaseSessionLeaseForKey(leaseKey)
}

func (a *App) clearTabBuildCancel(tab *WorkspaceTab, generation uint64, cancel context.CancelFunc, keepContext bool) {
	if cancel == nil {
		return
	}
	if !keepContext {
		defer cancel()
	}
	if tab == nil {
		return
	}
	a.mu.Lock()
	if tab.buildGeneration == generation {
		tab.buildCancel = nil
	}
	a.mu.Unlock()
}

func (a *App) closeTabRuntimeAdmissionHeld(tab *WorkspaceTab) {
	if tab == nil {
		return
	}
	a.mu.RLock()
	ctrl := tab.Ctrl
	sink := tab.sink
	a.mu.RUnlock()
	if ctrl != nil {
		ctrl.SetSessionPath("")
		a.quiesceTabAutosave(tab)
		ctrl.Cancel()
		ctrl.Close()
		a.releaseTabSharedHost(tab)
	}
	if sink != nil {
		sink.clearContext()
	}
	tab.releaseSessionLease()
	a.mu.Lock()
	a.releaseSessionRuntimeLocked(tab)
	a.mu.Unlock()
}

// buildTabController assembles a controller for a tab in the background, the
// same way buildController works for the single-controller App. On success it
// wires the controller and flips Ready; on failure it stores StartupErr.
func (a *App) startTabControllerBuild(tab *WorkspaceTab) {
	buildCtx, cancel := context.WithCancel(a.bootContext())
	a.mu.Lock()
	if tab == nil || tab.removed {
		a.mu.Unlock()
		cancel()
		return
	}
	tab.buildGeneration++
	generation := tab.buildGeneration
	tab.buildCancel = cancel
	if tab.buildDone != nil {

		close(tab.buildDone)
	}
	tab.buildDone = make(chan struct{})
	tab.buildDoneGen = generation
	a.mu.Unlock()
	if a.ctx == nil {
		a.buildTabControllerWithContext(tab, loadedTabSession{}, buildCtx, generation, cancel)
		return
	}
	go a.buildTabControllerWithContext(tab, loadedTabSession{}, buildCtx, generation, cancel)
}

func (a *App) buildTabController(tab *WorkspaceTab) {
	a.buildTabControllerWithLoadedSession(tab, loadedTabSession{})
}
